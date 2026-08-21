defmodule Pika.ProgressSummaryStore do
  @moduledoc false

  alias Pika.Agent.Role.Work
  alias Pika.Repo

  @active_statuses ~w(requested running)

  def request(campaign_id, context, profile) when is_map(context) and is_map(profile) do
    id = Ecto.UUID.generate()
    now = System.system_time(:microsecond)
    phase = field(context, :phase) || "unknown"
    attempt_ids = context |> field(:active_attempts) |> List.wrap() |> Enum.map(&field(&1, :id))

    transaction =
      Repo.transaction(fn ->
        case active_request(campaign_id) do
          nil ->
            backend = profile["backend"] || profile["type"] || "codex_app_server"

            Repo.query!(
              """
              INSERT INTO progress_summaries(
                id, campaign_id, phase, attempt_ids_json, backend, model, reasoning_effort,
                content, created_at, status, context_json, updated_at
              ) VALUES (?, ?, ?, ?, ?, ?, ?, '', ?, 'requested', ?, ?)
              """,
              [
                id,
                campaign_id,
                phase,
                Jason.encode!(attempt_ids),
                backend,
                profile["model"],
                profile["reasoning_effort"],
                now,
                Jason.encode!(Pika.JSONSafe.json_safe(context)),
                now
              ]
            )

            event =
              append_event(
                campaign_id,
                "progress_summary_requested",
                %{
                  summary_id: id,
                  phase: phase,
                  attempt_ids: attempt_ids
                },
                now
              )

            {%{
               id: id,
               campaign_id: campaign_id,
               phase: phase,
               attempt_ids: attempt_ids,
               backend: backend,
               model: profile["model"],
               reasoning_effort: profile["reasoning_effort"],
               content: "",
               status: "requested",
               context: context,
               failure_reason: nil,
               created_at: now,
               updated_at: now
             }, event}

          request ->
            {request, nil}
        end
      end)

    finish_transaction(transaction)
  rescue
    error -> {:error, {:progress_summary_request_failed, Exception.message(error)}}
  end

  def runnable_work(campaign_id) do
    placeholders = Enum.map_join(@active_statuses, ",", fn _ -> "?" end)

    Repo.query!(
      "SELECT id FROM progress_summaries WHERE campaign_id = ? AND status IN (#{placeholders}) ORDER BY created_at",
      [campaign_id | @active_statuses]
    ).rows
    |> Enum.map(fn [id] ->
      %Work{
        role_id: "progress_summary",
        kind: :progress_summary,
        id: id,
        campaign_id: campaign_id
      }
    end)
  end

  def get(id) when is_binary(id) do
    case request_row("WHERE id = ?", [id]) do
      nil -> {:error, :progress_summary_not_found}
      request -> {:ok, request}
    end
  end

  def mark_running(id) when is_binary(id) do
    transition(id, @active_statuses, "running", nil, "progress_summary_started")
  end

  def submit(id, content) when is_binary(id) and is_binary(content) do
    content = String.trim(content)

    if content == "" do
      {:error, :progress_summary_content_required}
    else
      transition(id, @active_statuses, "completed", content, "progress_summary_completed")
    end
  end

  def submit(_id, _content), do: {:error, :progress_summary_content_required}

  def fail(id, reason) when is_binary(id) do
    reason = reason |> inspect() |> String.slice(0, 4_000)
    transition(id, @active_statuses, "failed", reason, "progress_summary_failed")
  end

  def insert(campaign_id, attrs) do
    id = Ecto.UUID.generate()
    now = System.system_time(:microsecond)
    attempt_ids = List.wrap(attrs.attempt_ids)

    transaction =
      Repo.transaction(fn ->
        Repo.query!(
          "INSERT INTO progress_summaries(id, campaign_id, phase, attempt_ids_json, backend, model, reasoning_effort, content, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)",
          [
            id,
            campaign_id,
            attrs.phase,
            Jason.encode!(attempt_ids),
            attrs.backend,
            attrs.model,
            attrs.reasoning_effort,
            attrs.content,
            now
          ]
        )

        payload = %{
          summary_id: id,
          phase: attrs.phase,
          attempt_ids: attempt_ids,
          content: attrs.content
        }

        {%{id: id, created_at: now},
         append_event(campaign_id, "progress_summary_created", payload, now)}
      end)

    finish_transaction(transaction)
  rescue
    error -> {:error, {:progress_summary_persist_failed, Exception.message(error)}}
  end

  def list(campaign_id, limit \\ 50) do
    Repo.query!(
      "SELECT id, phase, attempt_ids_json, backend, model, reasoning_effort, content, created_at FROM progress_summaries WHERE campaign_id = ? AND status = 'completed' ORDER BY created_at DESC LIMIT ?",
      [campaign_id, limit]
    ).rows
    |> Enum.map(fn [id, phase, attempts, backend, model, effort, content, created_at] ->
      %{
        id: id,
        phase: phase,
        attempt_ids: Jason.decode!(attempts),
        backend: backend,
        model: model,
        reasoning_effort: effort,
        content: content,
        created_at: created_at
      }
    end)
  end

  defp active_request(campaign_id) do
    placeholders = Enum.map_join(@active_statuses, ",", fn _ -> "?" end)

    request_row(
      "WHERE campaign_id = ? AND status IN (#{placeholders}) ORDER BY created_at LIMIT 1",
      [campaign_id | @active_statuses]
    )
  end

  defp request_row(where, params) do
    case Repo.query!(
           """
           SELECT id, campaign_id, phase, attempt_ids_json, backend, model, reasoning_effort,
                  content, status, context_json, failure_reason, created_at, updated_at
           FROM progress_summaries
           #{where}
           """,
           params
         ).rows do
      [row] -> decode_request(row)
      [] -> nil
    end
  end

  defp transition(id, from_statuses, to_status, value, event_type) do
    now = System.system_time(:microsecond)
    placeholders = Enum.map_join(from_statuses, ",", fn _ -> "?" end)

    transaction =
      Repo.transaction(fn ->
        update = transition_update(to_status, value, now)

        case Repo.query!(
               """
               UPDATE progress_summaries
               SET #{update.set_clause}
               WHERE id = ? AND status IN (#{placeholders})
               RETURNING campaign_id
               """,
               update.params ++ [id | from_statuses]
             ).rows do
          [[campaign_id]] ->
            request = request_row("WHERE id = ?", [id])

            payload = %{
              summary_id: id,
              status: to_status,
              content: if(to_status == "completed", do: value, else: nil),
              reason: if(to_status == "failed", do: value, else: nil)
            }

            {request, append_event(campaign_id, event_type, payload, now)}

          [] ->
            case request_row("WHERE id = ?", [id]) do
              nil -> Repo.rollback(:progress_summary_not_found)
              %{status: ^to_status} = request -> {request, nil}
              request -> Repo.rollback({:invalid_progress_summary_transition, request.status})
            end
        end
      end)

    finish_transaction(transaction)
  rescue
    error -> {:error, {:progress_summary_transition_failed, Exception.message(error)}}
  end

  defp transition_update("running", _value, now) do
    %{
      set_clause: "status = 'running', started_at = COALESCE(started_at, ?), updated_at = ?",
      params: [now, now]
    }
  end

  defp transition_update("completed", content, now) do
    %{
      set_clause:
        "status = 'completed', content = ?, failure_reason = NULL, completed_at = ?, updated_at = ?",
      params: [content, now, now]
    }
  end

  defp transition_update("failed", reason, now) do
    %{
      set_clause: "status = 'failed', failure_reason = ?, completed_at = ?, updated_at = ?",
      params: [reason, now, now]
    }
  end

  defp decode_request([
         id,
         campaign_id,
         phase,
         attempts,
         backend,
         model,
         effort,
         content,
         status,
         context,
         failure_reason,
         created_at,
         updated_at
       ]) do
    %{
      id: id,
      campaign_id: campaign_id,
      phase: phase,
      attempt_ids: Jason.decode!(attempts),
      backend: backend,
      model: model,
      reasoning_effort: effort,
      content: content,
      status: status,
      context: Jason.decode!(context),
      failure_reason: failure_reason,
      created_at: created_at,
      updated_at: updated_at
    }
  end

  defp append_event(campaign_id, event_type, payload, now) do
    event_id = Ecto.UUID.generate()

    [[sequence]] =
      Repo.query!(
        "INSERT INTO domain_events(event_id, aggregate_type, aggregate_id, event_type, payload_json, created_at) VALUES (?, 'campaign', ?, ?, ?, ?) RETURNING sequence",
        [event_id, campaign_id, event_type, Jason.encode!(payload), now]
      ).rows

    %{
      sequence: sequence,
      event_id: event_id,
      aggregate_type: "campaign",
      aggregate_id: campaign_id,
      event_type: event_type,
      payload: payload,
      created_at: now,
      campaign_id: campaign_id
    }
  end

  defp finish_transaction({:ok, {result, event}}) do
    if event do
      Phoenix.PubSub.broadcast(
        Pika.PubSub,
        Pika.Persistence.topic(event.campaign_id),
        {:domain_event, event}
      )

      Phoenix.PubSub.broadcast(
        Pika.PubSub,
        "pika:optimization:#{event.campaign_id}",
        {:optimization_event, event}
      )
    end

    {:ok, result}
  end

  defp finish_transaction({:error, reason}), do: {:error, reason}

  defp field(nil, _key), do: nil
  defp field(map, key) when is_map(map), do: Map.get(map, key, Map.get(map, to_string(key)))
end
