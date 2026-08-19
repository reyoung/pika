defmodule Pika.Control do
  @moduledoc false

  alias Pika.{Persistence, Repo, SyncStore}

  def snapshot(campaign_id \\ current_campaign_id()) do
    campaign = campaign!(campaign_id)

    %{
      campaign: campaign,
      attempts: attempt_counts(campaign_id),
      active_sessions: active_session_count(campaign_id),
      integration_active: integration_active?(campaign_id),
      sync: optional(&SyncStore.latest_run/1, campaign_id)
    }
  end

  def pause(idempotency_key), do: change_state("pause", idempotency_key)
  def stop_now(idempotency_key), do: change_state("stop", idempotency_key)
  def resume(idempotency_key), do: change_state("resume", idempotency_key)

  def reconcile(campaign_id \\ current_campaign_id()) do
    transaction =
      Repo.transaction(fn ->
        campaign = campaign!(campaign_id)

        cond do
          campaign.status == "optimizing" and budget_exhausted?(campaign) ->
            now = now_us()

            Repo.query!(
              "UPDATE campaigns SET status = 'draining', dispatch_gate = 'budget', updated_at = ?, lock_version = lock_version + 1 WHERE id = ?",
              [now, campaign_id]
            )

            event =
              insert_event!(campaign_id, "campaign_draining", %{
                attempts_created: campaign.attempts_created,
                max_attempts: campaign.max_attempts
              })

            {campaign!(campaign_id), event}

          campaign.status == "draining" and drained?(campaign_id) ->
            now = now_us()

            Repo.query!(
              "UPDATE campaigns SET status = 'completed', dispatch_gate = 'completed', updated_at = ?, lock_version = lock_version + 1 WHERE id = ?",
              [now, campaign_id]
            )

            event = insert_event!(campaign_id, "campaign_completed", %{})
            {campaign!(campaign_id), event}

          true ->
            {campaign, nil}
        end
      end)

    publish(transaction)
  rescue
    error -> {:error, {:campaign_reconcile_failed, Exception.message(error)}}
  end

  defp change_state(action, idempotency_key) do
    campaign_id = current_campaign_id()
    request_hash = hash({campaign_id, action})

    transaction =
      Repo.transaction(fn ->
        case existing_action(idempotency_key, action, request_hash) do
          {:replay, response} ->
            {response, nil}

          :conflict ->
            Repo.rollback(:idempotency_conflict)

          :missing ->
            campaign = campaign!(campaign_id)
            {changes, event_type, payload} = transition(action, campaign)
            now = now_us()

            Repo.query!(
              "UPDATE campaigns SET status = ?, resume_state = ?, dispatch_gate = ?, updated_at = ?, lock_version = lock_version + 1 WHERE id = ?",
              [changes.status, changes.resume_state, changes.dispatch_gate, now, campaign_id]
            )

            event = insert_event!(campaign_id, event_type, payload)
            response = campaign!(campaign_id)

            Repo.query!(
              "INSERT INTO control_actions(idempotency_key, campaign_id, action, request_sha256, response_json, created_at) VALUES (?, ?, ?, ?, ?, ?)",
              [
                idempotency_key,
                campaign_id,
                action,
                request_hash,
                Jason.encode!(response),
                now
              ]
            )

            {response, event}
        end
      end)

    case publish(transaction) do
      {:ok, _response} = ok ->
        apply_runtime_action(action)
        ok

      error ->
        error
    end
  rescue
    error -> {:error, {:campaign_control_failed, Exception.message(error)}}
  end

  defp transition("pause", campaign) do
    if campaign.status in ~w(paused stopped blocked completed),
      do: Repo.rollback({:invalid_campaign_state, campaign.status})

    {%{status: "paused", resume_state: campaign.status, dispatch_gate: "paused"},
     "campaign_paused", %{resume_state: campaign.status}}
  end

  defp transition("stop", campaign) do
    if campaign.status == "completed", do: Repo.rollback({:invalid_campaign_state, "completed"})

    resume_state =
      if campaign.status in ~w(paused stopped blocked),
        do: campaign.resume_state || "optimizing",
        else: campaign.status

    {%{status: "stopped", resume_state: resume_state, dispatch_gate: "stopped"},
     "campaign_stopped", %{resume_state: resume_state}}
  end

  defp transition("resume", campaign) do
    unless campaign.status in ~w(paused stopped blocked),
      do: Repo.rollback({:invalid_campaign_state, campaign.status})

    status = campaign.resume_state || "optimizing"

    {%{status: status, resume_state: nil, dispatch_gate: nil}, "campaign_resumed",
     %{status: status}}
  end

  defp apply_runtime_action("stop") do
    _ = Pika.AttemptCoordinator.stop_now()
    _ = Pika.IntegrationCoordinator.stop_now()
    _ = Pika.SyncCoordinator.stop_now()
    :ok
  end

  defp apply_runtime_action("resume") do
    _ = Pika.AttemptCoordinator.resume()
    _ = Pika.IntegrationCoordinator.resume()
    _ = Pika.SyncCoordinator.resume()
    :ok
  end

  defp apply_runtime_action(_action), do: :ok

  defp existing_action(key, action, request_hash) do
    case Repo.query!(
           "SELECT action, request_sha256, response_json FROM control_actions WHERE idempotency_key = ?",
           [key]
         ).rows do
      [] -> :missing
      [[^action, ^request_hash, response]] -> {:replay, Jason.decode!(response)}
      [_] -> :conflict
    end
  end

  defp publish({:ok, {value, nil}}), do: {:ok, value}

  defp publish({:ok, {value, event}}) do
    if Process.whereis(Pika.PubSub),
      do:
        Phoenix.PubSub.broadcast(
          Pika.PubSub,
          Persistence.topic(value.id || value["id"]),
          {:domain_event, event}
        )

    {:ok, value}
  end

  defp publish({:error, reason}), do: {:error, reason}

  defp campaign!(campaign_id) do
    case Repo.query!(
           "SELECT id, status, resume_state, dispatch_gate, best_sha, attempts_created, max_attempts, current_spec_revision_id, updated_at FROM campaigns WHERE id = ?",
           [campaign_id]
         ).rows do
      [[id, status, resume, gate, best, created, max, spec, updated]] ->
        %{
          id: id,
          status: status,
          resume_state: resume,
          dispatch_gate: gate,
          best_sha: best,
          attempts_created: created,
          max_attempts: max,
          current_spec_revision_id: spec,
          updated_at: updated
        }

      [] ->
        raise "campaign not found"
    end
  end

  defp attempt_counts(campaign_id) do
    Repo.query!(
      "SELECT status, COUNT(*) FROM attempts WHERE campaign_id = ? GROUP BY status ORDER BY status",
      [campaign_id]
    ).rows
    |> Map.new(fn [status, count] -> {status, count} end)
  end

  defp active_session_count(campaign_id) do
    [[count]] =
      Repo.query!(
        "SELECT COUNT(*) FROM agent_sessions WHERE campaign_id = ? AND status IN ('starting','running','awaiting_report')",
        [campaign_id]
      ).rows

    count
  end

  defp integration_active?(campaign_id) do
    Repo.query!("SELECT 1 FROM integration_leases WHERE campaign_id = ? LIMIT 1", [campaign_id]).rows !=
      []
  end

  defp budget_exhausted?(%{max_attempts: nil}), do: false

  defp budget_exhausted?(campaign),
    do: campaign.attempts_created >= campaign.max_attempts

  defp drained?(campaign_id) do
    [[attempts]] =
      Repo.query!(
        "SELECT COUNT(*) FROM attempts WHERE campaign_id = ? AND status NOT IN ('accepted','rejected','cancelled')",
        [campaign_id]
      ).rows

    attempts == 0 and not integration_active?(campaign_id) and
      is_nil(SyncStore.active_run(campaign_id))
  end

  defp insert_event!(campaign_id, event_type, payload) do
    event = %{
      event_id: Ecto.UUID.generate(),
      aggregate_type: "campaign",
      aggregate_id: campaign_id,
      event_type: event_type,
      payload: payload,
      created_at: now_us()
    }

    [[sequence]] =
      Repo.query!(
        "INSERT INTO domain_events(event_id, aggregate_type, aggregate_id, event_type, payload_json, created_at) VALUES (?, 'campaign', ?, ?, ?, ?) RETURNING sequence",
        [
          event.event_id,
          campaign_id,
          event_type,
          Jason.encode!(Pika.JSONSafe.json_safe(payload)),
          event.created_at
        ]
      ).rows

    Map.put(event, :sequence, sequence)
  end

  defp optional(fun, arg) do
    case fun.(arg) do
      {:ok, value} -> value
      _ -> nil
    end
  end

  defp current_campaign_id, do: Pika.Persistence.current_campaign().id

  defp hash(value),
    do: :crypto.hash(:sha256, :erlang.term_to_binary(value)) |> Base.encode16(case: :lower)

  defp now_us, do: System.system_time(:microsecond)
end
