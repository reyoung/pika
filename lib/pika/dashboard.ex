defmodule Pika.Dashboard do
  @moduledoc false

  alias Pika.{AttemptStore, Control, ProgressSummaryStore, Repo, SyncCoordinator}

  @agent_event_limit 500

  def snapshot(campaign_id \\ current_campaign_id(), opts \\ []) do
    attempts = AttemptStore.attempts(campaign_id, limit: 1_000) |> Enum.sort_by(& &1.ordinal)
    sessions = AttemptStore.sessions(campaign_id)
    revisions = spec_revisions(campaign_id)
    guidance = guidance_by_attempt(campaign_id)

    spec =
      Keyword.get_lazy(opts, :spec, fn ->
        optional(&AttemptStore.campaign_spec_overview/1, campaign_id)
      end)

    attempts =
      attempts
      |> with_base_attempts()
      |> Enum.map(fn attempt ->
        Map.merge(attempt, %{
          spec_revision: revisions[attempt.spec_revision_id],
          metrics: AttemptStore.metrics_for_attempt(attempt.id),
          sessions: Enum.filter(sessions, &(&1.attempt_id == attempt.id)),
          agent_events: agent_events(attempt.id),
          guidance: Map.get(guidance, attempt.id, []),
          artifacts: artifacts("attempt", attempt.id)
        })
      end)

    %{
      control: Control.snapshot(campaign_id),
      attempts: attempts,
      sessions: sessions,
      metrics: metric_points(attempts),
      events: AttemptStore.events(campaign_id, 0, 2_000),
      progress_summaries: ProgressSummaryStore.list(campaign_id),
      sync: sync_snapshot(),
      spec: spec
    }
  end

  def attempt(campaign_id, attempt_id) do
    with {:ok, attempt} <- AttemptStore.attempt(attempt_id),
         true <- attempt.campaign_id == campaign_id do
      attempt =
        Map.put(attempt, :base_attempt, accepted_base_attempt(campaign_id, attempt.base_sha))

      {:ok,
       Map.merge(attempt, %{
         spec_revision: spec_revision(attempt.spec_revision_id),
         metrics: AttemptStore.metrics_for_attempt(attempt_id),
         sessions:
           Enum.filter(AttemptStore.sessions(campaign_id), &(&1.attempt_id == attempt_id)),
         guidance: Map.get(guidance_by_attempt(campaign_id), attempt_id, []),
         events: events_for("attempt", attempt_id),
         agent_events: agent_events(attempt_id),
         artifacts: artifacts("attempt", attempt_id)
       })}
    else
      false -> {:error, :attempt_not_found}
      {:error, _} = error -> error
    end
  end

  defp with_base_attempts(attempts) do
    accepted_by_sha =
      attempts
      |> Enum.filter(&is_binary(&1.accepted_sha))
      |> Map.new(&{&1.accepted_sha, Map.take(&1, [:id, :ordinal, :accepted_sha])})

    Enum.map(attempts, fn attempt ->
      Map.put(attempt, :base_attempt, Map.get(accepted_by_sha, attempt.base_sha))
    end)
  end

  defp accepted_base_attempt(campaign_id, base_sha) do
    case AttemptStore.accepted_attempt_by_sha(campaign_id, base_sha) do
      {:ok, attempt} -> Map.take(attempt, [:id, :ordinal, :accepted_sha])
      {:error, _} -> nil
    end
  end

  defp metric_points(attempts) do
    Enum.flat_map(attempts, fn attempt ->
      Enum.map(attempt.metrics, fn metric ->
        %{
          attempt_id: attempt.id,
          ordinal: attempt.ordinal,
          spec_revision: attempt.spec_revision,
          status: attempt.status,
          summary: attempt.summary || attempt.description || "Attempt ##{attempt.ordinal}",
          measured_at: metric.measured_at,
          case_id: metric.case_id,
          metric_id: metric.metric_id,
          unit: metric.unit,
          value: metric.value,
          baseline_value: metric.baseline_value,
          improvement_ratio: metric.improvement_ratio,
          target_snapshot_id: metric.target_snapshot_id,
          target_value: metric.target_value,
          target_relative_improvement: metric.target_relative_improvement,
          best_relative_improvement: metric.best_relative_improvement,
          noise_tolerance: metric.noise_tolerance,
          source: metric.source,
          patch_artifact_id: attempt.patch_artifact_id,
          profiler_summary: attempt.profiler_summary,
          outcome: attempt.outcome_reason
        }
      end)
    end)
    |> Enum.sort_by(&{&1.measured_at, &1.ordinal, &1.case_id, &1.metric_id})
  end

  defp spec_revisions(campaign_id) do
    Repo.query!(
      "SELECT id, revision FROM spec_revisions WHERE campaign_id = ? ORDER BY revision",
      [campaign_id]
    ).rows
    |> Map.new(fn [id, revision] -> {id, revision} end)
  end

  defp spec_revision(spec_revision_id) do
    case Repo.query!("SELECT revision FROM spec_revisions WHERE id = ?", [spec_revision_id]).rows do
      [[revision]] -> revision
      [] -> nil
    end
  end

  defp events_for(type, id) do
    Repo.query!(
      "SELECT sequence, event_type, payload_json, created_at FROM domain_events WHERE aggregate_type = ? AND aggregate_id = ? ORDER BY sequence",
      [type, id]
    ).rows
    |> Enum.map(fn [sequence, event_type, payload, created_at] ->
      %{
        sequence: sequence,
        event_type: event_type,
        payload: Jason.decode!(payload),
        created_at: created_at
      }
    end)
  end

  defp artifacts(owner_type, owner_id) do
    Repo.query!(
      "SELECT id, kind, relative_path, sha256, byte_size, mime_type, metadata_json, created_at FROM artifacts WHERE owner_type = ? AND owner_id = ? ORDER BY created_at",
      [owner_type, owner_id]
    ).rows
    |> Enum.map(fn [id, kind, path, sha, size, mime, metadata, created_at] ->
      %{
        id: id,
        kind: kind,
        relative_path: path,
        sha256: sha,
        byte_size: size,
        mime_type: mime,
        metadata: Jason.decode!(metadata),
        created_at: created_at
      }
    end)
  end

  defp agent_events(attempt_id) do
    root = Repo.config()[:database] |> Path.dirname()

    backend_events =
      Repo.query!(
        "SELECT a.relative_path FROM agent_sessions s JOIN artifacts a ON a.id = s.log_artifact_id WHERE s.attempt_id = ? GROUP BY a.relative_path ORDER BY MIN(s.started_at)",
        [attempt_id]
      ).rows
      |> List.flatten()
      |> Enum.flat_map(fn relative_path ->
        path = Path.expand(relative_path, root)

        if String.starts_with?(path, root <> "/") and File.regular?(path) do
          path
          |> File.stream!(:line)
          |> Stream.map(&Jason.decode/1)
          |> Stream.filter(&match?({:ok, _}, &1))
          |> Stream.map(&elem(&1, 1))
          |> Enum.to_list()
        else
          []
        end
      end)

    campaign_events =
      Repo.query!(
        """
        SELECT e.event_type, e.payload_json, e.created_at
        FROM domain_events e
        JOIN attempts a ON a.id = ?
        WHERE e.aggregate_type = 'campaign' AND e.aggregate_id = a.campaign_id
          AND e.event_type IN ('best_advanced', 'sampling_advanced')
          AND e.created_at >= a.created_at
        ORDER BY e.sequence
        """,
        [attempt_id]
      ).rows
      |> Enum.map(fn [type, payload, created_at] ->
        %{
          "type" => type,
          "at" => created_at |> DateTime.from_unix!(:microsecond) |> DateTime.to_iso8601(),
          "data" => Jason.decode!(payload)
        }
      end)

    (backend_events ++ campaign_events)
    |> Enum.sort_by(&(&1["at"] || ""))
    |> coalesce_message_deltas()
    |> Enum.take(-@agent_event_limit)
  end

  defp coalesce_message_deltas(events) do
    {timeline, messages} =
      Enum.reduce(events, {[], %{}}, fn event, {timeline, messages} ->
        case message_delta_key(event) do
          nil ->
            {[{:event, event} | timeline], messages}

          key ->
            case Map.fetch(messages, key) do
              :error ->
                delta = get_in(event, ["data", "delta"])

                {[{:message, key} | timeline],
                 Map.put(messages, key, %{event: event, chunks: [delta]})}

              {:ok, message} ->
                delta = get_in(event, ["data", "delta"])
                updated = %{message | chunks: [delta | message.chunks]}
                {timeline, Map.put(messages, key, updated)}
            end
        end
      end)

    timeline
    |> Enum.reverse()
    |> Enum.map(fn
      {:event, event} ->
        event

      {:message, key} ->
        %{event: event, chunks: chunks} = Map.fetch!(messages, key)
        put_in(event, ["data", "delta"], chunks |> Enum.reverse() |> IO.iodata_to_binary())
    end)
  end

  defp message_delta_key(
         %{
           "type" => "message_delta",
           "data" => %{"delta" => delta} = data
         } = event
       )
       when is_binary(delta) do
    case data["item_id"] || data["itemId"] do
      item_id when is_binary(item_id) and item_id != "" ->
        {event["session_id"], event["turn_id"], item_id}

      _other ->
        nil
    end
  end

  defp message_delta_key(_event), do: nil

  defp guidance_by_attempt(campaign_id) do
    Repo.query!(
      "SELECT id, attempt_id, kind, body, status, created_at FROM guidance WHERE campaign_id = ? AND attempt_id IS NOT NULL ORDER BY sequence",
      [campaign_id]
    ).rows
    |> Enum.group_by(
      fn [_id, attempt_id, _kind, _body, _status, _created_at] -> attempt_id end,
      fn [id, _attempt_id, kind, body, status, created_at] ->
        %{
          id: id,
          kind: kind,
          body: body,
          status: status,
          created_at: created_at
        }
      end
    )
  end

  defp sync_snapshot do
    case SyncCoordinator.snapshot() do
      {:error, _} -> nil
      snapshot -> snapshot
    end
  end

  defp optional(fun, arg) do
    case fun.(arg) do
      {:ok, value} -> value
      _ -> nil
    end
  end

  defp current_campaign_id, do: Pika.Persistence.current_campaign().id
end
