defmodule Pika.CampaignStore do
  @moduledoc false

  alias Pika.Repo

  @durable_fields ~w(
    status messages artifacts spec_result spec_diff harness reference_review_evidence references skill best_sha setup_base_sha setup_sha
    baseline baseline_retry_count baseline_error sampling_revisions iteration_sampling workflow_kickoffs
    pending_confirmation_input last_error backend_workflow required provider_session_id
  )a

  def load(campaign_id) do
    case Repo.query!(
           "SELECT state_blob FROM campaign_runtime_snapshots WHERE campaign_id = ?",
           [campaign_id]
         ).rows do
      [[blob]] -> decode_snapshot(blob)
      [] -> :none
    end
  rescue
    error -> {:error, {:runtime_snapshot_load_failed, Exception.message(error)}}
  end

  def decode_snapshot(blob) when is_binary(blob) do
    preload_snapshot_atoms()

    case :erlang.binary_to_term(blob, [:safe]) do
      durable when is_map(durable) -> {:ok, durable}
      _other -> {:error, {:runtime_snapshot_load_failed, "snapshot root must be a map"}}
    end
  rescue
    error -> {:error, {:runtime_snapshot_load_failed, Exception.message(error)}}
  end

  def persist(state) do
    campaign_id = state.campaign_id
    durable = Map.take(state, @durable_fields)
    blob = :erlang.term_to_binary(durable, compressed: 6)
    now = System.system_time(:microsecond)

    Repo.transaction(fn ->
      spec_revision_id = persist_spec!(campaign_id, state, now)
      persist_baseline!(campaign_id, spec_revision_id, state, now)
      persist_sampling!(campaign_id, spec_revision_id, state, now)
      persist_artifacts!(campaign_id, state, now)
      persist_agent_session!(campaign_id, state, now)

      status = persisted_campaign_status(state.status)

      Repo.query!(
        "UPDATE campaigns SET status = ?, best_sha = ?, current_spec_revision_id = ?, updated_at = ? WHERE id = ?",
        [status, state.best_sha, spec_revision_id, now, campaign_id]
      )

      Repo.query!(
        """
        INSERT INTO campaign_runtime_snapshots(campaign_id, state_blob, updated_at)
        VALUES (?, ?, ?)
        ON CONFLICT(campaign_id) DO UPDATE SET state_blob = excluded.state_blob,
          updated_at = excluded.updated_at
        """,
        [campaign_id, {:blob, blob}, now]
      )
    end)
    |> case do
      {:ok, _} -> :ok
      {:error, reason} -> {:error, reason}
    end
  rescue
    error -> {:error, {:campaign_persist_failed, Exception.message(error)}}
  end

  def counts(campaign_id) do
    for table <- ~w(spec_revisions benchmark_cases metric_definitions sampling_revisions
                     sampling_revision_cases best_revisions best_metrics),
        into: %{} do
      [[count]] =
        Repo.query!("SELECT COUNT(*) FROM #{table} WHERE #{campaign_filter(table)}", [campaign_id]).rows

      {table, count}
    end
  end

  def latest_provider_session(campaign_id, backend) do
    case Repo.query!(
           """
           SELECT provider_session_id FROM agent_sessions
           WHERE campaign_id = ? AND backend = ? AND provider_session_id IS NOT NULL
           ORDER BY started_at DESC LIMIT 1
           """,
           [campaign_id, to_string(backend)]
         ).rows do
      [[provider_session_id]] -> provider_session_id
      [] -> nil
    end
  rescue
    _error -> nil
  end

  def lookup_idempotency(session_id, tool, key, request_hash, _state) do
    case Repo.query!(
           """
           SELECT request_sha256, response_json FROM idempotency_records
           WHERE backend_session_id = ? AND tool_name = ? AND idempotency_key = ?
           """,
           [session_id, tool, key]
         ).rows do
      [] ->
        :missing

      [[stored_hash, response_json]] ->
        if stored_hash == hex(request_hash),
          do: {:replay, decode_response(response_json)},
          else: :conflict
    end
  end

  def store_idempotency(session_id, tool, key, request_hash, response, _state) do
    session_exists? =
      Repo.query!("SELECT 1 FROM agent_sessions WHERE id = ? LIMIT 1", [session_id]).rows != []

    if session_exists? do
      Repo.query!(
        """
        INSERT INTO idempotency_records(
          backend_session_id, tool_name, idempotency_key, request_sha256,
          response_json, created_at
        ) VALUES (?, ?, ?, ?, ?, ?)
        ON CONFLICT(backend_session_id, tool_name, idempotency_key) DO NOTHING
        """,
        [
          session_id,
          tool,
          key,
          hex(request_hash),
          encode_response(response),
          System.system_time(:microsecond)
        ]
      )
    end

    :ok
  rescue
    error -> {:error, Exception.message(error)}
  end

  defp persist_spec!(campaign_id, state, now) do
    spec = state.spec_result.spec
    revision = spec["revision"] || 1

    {id, definitions_changed?} =
      case Repo.query!(
             "SELECT id, spec_json FROM spec_revisions WHERE campaign_id = ? AND revision = ?",
             [campaign_id, revision]
           ).rows do
        [[id, persisted_spec_json]] ->
          {id, definitions_changed?(persisted_spec_json, spec)}

        [] ->
          {Ecto.UUID.generate(), true}
      end

    status = spec_status(state.status)
    harness = state.harness
    selected_refs = Enum.filter(state.references, & &1.selected)

    ref_snapshot =
      Enum.map(selected_refs, &Map.take(&1, [:id, :url, :description, :branch, :sha]))

    skill_snapshot = Map.take(state.skill, [:name, :url, :branch, :sha])
    confirmed_at = if status == "confirmed", do: now, else: nil
    baseline_sha = if state.setup_sha, do: state.best_sha, else: nil

    Repo.query!(
      """
      INSERT INTO spec_revisions(
        id, campaign_id, revision, status, spec_json, protected_paths_json,
        protected_digest, baseline_sha, reference_snapshot_json, skill_snapshot_json,
        confirmed_at, inserted_at, updated_at
      ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
      ON CONFLICT(campaign_id, revision) DO UPDATE SET
        status = excluded.status,
        spec_json = excluded.spec_json,
        protected_paths_json = excluded.protected_paths_json,
        protected_digest = excluded.protected_digest,
        baseline_sha = COALESCE(excluded.baseline_sha, spec_revisions.baseline_sha),
        reference_snapshot_json = excluded.reference_snapshot_json,
        skill_snapshot_json = excluded.skill_snapshot_json,
        confirmed_at = COALESCE(spec_revisions.confirmed_at, excluded.confirmed_at),
        updated_at = excluded.updated_at
      """,
      [
        id,
        campaign_id,
        revision,
        status,
        Jason.encode!(spec),
        Jason.encode!(if(harness, do: harness.protected_paths, else: [])),
        harness && harness.digest,
        baseline_sha,
        Jason.encode!(Pika.JSONSafe.json_safe(ref_snapshot)),
        Jason.encode!(Pika.JSONSafe.json_safe(skill_snapshot)),
        confirmed_at,
        now,
        now
      ]
    )

    if state.spec_result.ready? and
         (definitions_changed? or not definitions_materialized?(id)) do
      persist_case_metric_definitions!(id, spec)
    end

    id
  end

  defp definitions_changed?(persisted_spec_json, spec) do
    case Jason.decode(persisted_spec_json) do
      {:ok, persisted_spec} ->
        Map.take(persisted_spec, ["benchmark_cases", "metrics"]) !=
          Map.take(spec, ["benchmark_cases", "metrics"])

      _error ->
        true
    end
  end

  defp definitions_materialized?(spec_revision_id) do
    [[case_count, metric_count]] =
      Repo.query!(
        """
        SELECT
          (SELECT COUNT(*) FROM benchmark_cases WHERE spec_revision_id = ?),
          (SELECT COUNT(*) FROM metric_definitions WHERE spec_revision_id = ?)
        """,
        [spec_revision_id, spec_revision_id]
      ).rows

    case_count > 0 and metric_count > 0
  end

  defp persist_case_metric_definitions!(spec_revision_id, spec) do
    has_results? =
      Repo.query!("SELECT 1 FROM best_revisions WHERE spec_revision_id = ? LIMIT 1", [
        spec_revision_id
      ]).rows != []

    unless has_results? do
      Repo.query!("DELETE FROM metric_definitions WHERE spec_revision_id = ?", [spec_revision_id])
      Repo.query!("DELETE FROM benchmark_cases WHERE spec_revision_id = ?", [spec_revision_id])

      Enum.with_index(List.wrap(spec["benchmark_cases"]), 1)
      |> Enum.each(fn {case_, ordinal} ->
        Repo.query!(
          """
          INSERT INTO benchmark_cases(
            id, spec_revision_id, ordinal, name, kind, shape_json, dtype_json,
            layout_json, distribution_json, frequency_weight
          ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
          """,
          [
            Ecto.UUID.generate(),
            spec_revision_id,
            ordinal,
            case_["id"],
            case_["kind"],
            Jason.encode!(case_["shape"]),
            Jason.encode!(case_["dtype"]),
            Jason.encode!(case_["layout"]),
            encode_nullable(case_["distribution"]),
            case_["frequency_weight"]
          ]
        )
      end)

      Enum.each(List.wrap(spec["metrics"]), fn metric ->
        Repo.query!(
          """
          INSERT INTO metric_definitions(
            id, spec_revision_id, name, unit, direction, role,
            min_improvement_ratio, parser_json
          ) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
          """,
          [
            Ecto.UUID.generate(),
            spec_revision_id,
            metric["id"],
            metric["unit"],
            metric["direction"],
            metric["role"],
            metric["min_improvement_ratio"] || 0.01,
            Jason.encode!(metric["parser"] || %{})
          ]
        )
      end)
    end
  end

  defp persist_baseline!(_campaign_id, _spec_revision_id, %{baseline: nil}, _now), do: :ok

  defp persist_baseline!(campaign_id, spec_revision_id, state, now) do
    case Repo.query!(
           "SELECT id FROM best_revisions WHERE campaign_id = ? AND sha = ? AND spec_revision_id = ?",
           [campaign_id, state.best_sha, spec_revision_id]
         ).rows do
      [[_id]] ->
        # A Baseline revision and all of its metrics are inserted in the same
        # transaction. Once the revision exists, later Campaign snapshots must
        # not replay thousands of immutable metric upserts.
        :ok

      [] ->
        [[sequence]] =
          Repo.query!(
            "SELECT COALESCE(MAX(sequence), 0) + 1 FROM best_revisions WHERE campaign_id = ?",
            [campaign_id]
          ).rows

        best_revision_id = Ecto.UUID.generate()

        Repo.query!(
          """
          INSERT INTO best_revisions(
            id, campaign_id, sequence, sha, cause, spec_revision_id, summary, inserted_at
          ) VALUES (?, ?, ?, ?, 'baseline', ?, ?, ?)
          """,
          [
            best_revision_id,
            campaign_id,
            sequence,
            state.best_sha,
            spec_revision_id,
            state.baseline.summary || "Baseline",
            now
          ]
        )

        persist_baseline_metrics!(best_revision_id, spec_revision_id, state, now)
    end
  end

  defp persist_baseline_metrics!(best_revision_id, spec_revision_id, state, now) do
    case_ids =
      Repo.query!(
        "SELECT name, id FROM benchmark_cases WHERE spec_revision_id = ?",
        [spec_revision_id]
      ).rows
      |> Map.new(fn [name, id] -> {name, id} end)

    metric_ids =
      Repo.query!(
        "SELECT name, id FROM metric_definitions WHERE spec_revision_id = ?",
        [spec_revision_id]
      ).rows
      |> Map.new(fn [name, id] -> {name, id} end)

    Enum.each(state.baseline.metrics, fn metric ->
      case_id = Map.fetch!(case_ids, metric.case_id)
      metric_id = Map.fetch!(metric_ids, metric.metric_id)

      Repo.query!(
        """
        INSERT INTO best_metrics(
          best_revision_id, benchmark_case_id, metric_definition_id, measured_sha,
          value, baseline_value, improvement_ratio, mad, noise_tolerance,
          pair_count, valid_pair_count, source, measured_at
        ) VALUES (?, ?, ?, ?, ?, ?, 0.0, ?, ?, ?, ?, 'baseline', ?)
        ON CONFLICT(best_revision_id, benchmark_case_id, metric_definition_id) DO UPDATE SET
          measured_sha = excluded.measured_sha,
          value = excluded.value,
          baseline_value = excluded.baseline_value,
          mad = excluded.mad,
          noise_tolerance = excluded.noise_tolerance,
          pair_count = excluded.pair_count,
          valid_pair_count = excluded.valid_pair_count,
          measured_at = excluded.measured_at
        """,
        [
          best_revision_id,
          case_id,
          metric_id,
          state.best_sha,
          metric.value,
          metric.value,
          metric.mad,
          metric.noise_tolerance,
          metric.pair_count,
          metric.valid_pair_count,
          now
        ]
      )
    end)
  end

  defp persist_sampling!(_campaign_id, _spec_revision_id, %{sampling_revisions: []}, _now),
    do: :ok

  defp persist_sampling!(campaign_id, spec_revision_id, state, now) do
    Enum.each(state.sampling_revisions, fn sampling ->
      id =
        case Repo.query!(
               "SELECT id FROM sampling_revisions WHERE campaign_id = ? AND spec_revision_id = ? AND sequence = ?",
               [campaign_id, spec_revision_id, sampling.revision]
             ).rows do
          [[id]] -> id
          [] -> Ecto.UUID.generate()
        end

      Repo.query!(
        """
        INSERT INTO sampling_revisions(
          id, campaign_id, spec_revision_id, sequence, cause, summary,
          estimated_cost_json, created_at
        ) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
        ON CONFLICT(campaign_id, spec_revision_id, sequence) DO UPDATE SET
          summary = excluded.summary, estimated_cost_json = excluded.estimated_cost_json
        """,
        [
          id,
          campaign_id,
          spec_revision_id,
          sampling.revision,
          sampling.cause,
          sampling.summary,
          Jason.encode!(sampling.estimated_cost),
          datetime_us(sampling.created_at, now)
        ]
      )

      Enum.each(sampling.case_ids, fn case_name ->
        [[case_id]] =
          Repo.query!(
            "SELECT id FROM benchmark_cases WHERE spec_revision_id = ? AND name = ?",
            [spec_revision_id, case_name]
          ).rows

        Repo.query!(
          """
          INSERT INTO sampling_revision_cases(sampling_revision_id, benchmark_case_id, reason)
          VALUES (?, ?, ?)
          ON CONFLICT(sampling_revision_id, benchmark_case_id) DO UPDATE SET reason = excluded.reason
          """,
          [id, case_id, sampling.reasons[case_name]]
        )
      end)
    end)
  end

  defp persist_artifacts!(campaign_id, state, now) do
    Enum.each(state.artifacts, fn {_path, artifact} ->
      Repo.query!(
        """
        INSERT INTO artifacts(
          id, campaign_id, owner_type, owner_id, kind, relative_path, sha256,
          byte_size, mime_type, metadata_json, created_at
        ) VALUES (?, ?, 'campaign', ?, ?, ?, ?, ?, ?, ?, ?)
        ON CONFLICT(relative_path) DO UPDATE SET
          sha256 = excluded.sha256, byte_size = excluded.byte_size,
          mime_type = excluded.mime_type, metadata_json = excluded.metadata_json
        """,
        [
          Ecto.UUID.generate(),
          campaign_id,
          campaign_id,
          artifact.kind,
          artifact.relative_path,
          artifact.sha256,
          artifact.size,
          artifact.mime,
          Jason.encode!(artifact.metadata || %{}),
          now
        ]
      )
    end)
  end

  defp persist_agent_session!(_campaign_id, %{backend_session: nil}, _now), do: :ok

  defp persist_agent_session!(campaign_id, state, now) do
    session = state.backend_session
    required = state.required |> MapSet.to_list() |> Enum.sort()
    token_hash = state.backend_token_hash && Base.encode16(state.backend_token_hash, case: :lower)

    Repo.query!(
      """
      INSERT INTO agent_sessions(
        id, campaign_id, role, profile_json, backend, backend_protocol,
        provider_session_id, backend_capabilities_json, model, reasoning_effort,
        status, mcp_token_hash, required_operations_json, last_turn_sequence,
        last_event_seq, started_at
      ) VALUES (?, ?, 'boundary', ?, ?, ?, ?, ?, ?, ?, 'running', ?, ?, 0, 0, ?)
      ON CONFLICT(id) DO UPDATE SET
        provider_session_id = excluded.provider_session_id,
        backend_capabilities_json = excluded.backend_capabilities_json,
        status = excluded.status,
        mcp_token_hash = excluded.mcp_token_hash,
        required_operations_json = excluded.required_operations_json
      """,
      [
        session.id,
        campaign_id,
        Jason.encode!(state.backend_profile),
        to_string(state.backend_name),
        to_string(session.backend_protocol),
        session.backend_session_id,
        "{}",
        state.model,
        to_string(state.reasoning_effort),
        token_hash,
        Jason.encode!(required),
        now
      ]
    )
  end

  defp persisted_campaign_status(:resolving_references), do: "awaiting_confirmation"
  defp persisted_campaign_status(status), do: to_string(status)

  defp spec_status(status) when status in [:drafting_spec], do: "draft"

  defp spec_status(status) when status in [:awaiting_confirmation, :resolving_references],
    do: "awaiting_confirmation"

  defp spec_status(_status), do: "confirmed"

  defp encode_nullable(nil), do: nil
  defp encode_nullable(value), do: Jason.encode!(value)

  defp datetime_us(%DateTime{} = value, _fallback), do: DateTime.to_unix(value, :microsecond)
  defp datetime_us(_value, fallback), do: fallback

  defp campaign_filter("benchmark_cases"),
    do: "spec_revision_id IN (SELECT id FROM spec_revisions WHERE campaign_id = ?)"

  defp campaign_filter("metric_definitions"),
    do: "spec_revision_id IN (SELECT id FROM spec_revisions WHERE campaign_id = ?)"

  defp campaign_filter("sampling_revision_cases"),
    do: "sampling_revision_id IN (SELECT id FROM sampling_revisions WHERE campaign_id = ?)"

  defp campaign_filter("best_metrics"),
    do: "best_revision_id IN (SELECT id FROM best_revisions WHERE campaign_id = ?)"

  defp campaign_filter(_table), do: "campaign_id = ?"

  defp encode_response(response) do
    Jason.encode!(%{
      "encoding" => "erlang-term-v1",
      "data" => response |> :erlang.term_to_binary(compressed: 6) |> Base.encode64()
    })
  end

  defp decode_response(response_json) do
    %{"encoding" => "erlang-term-v1", "data" => data} = Jason.decode!(response_json)
    data |> Base.decode64!() |> :erlang.binary_to_term([:safe])
  end

  defp preload_snapshot_atoms do
    _ = Application.load(:pika)

    modules =
      case :application.get_key(:pika, :modules) do
        {:ok, modules} -> modules
        _other -> []
      end

    Enum.each(modules ++ [DateTime, MapSet], &Code.ensure_loaded/1)
  end

  defp hex(value), do: Base.encode16(value, case: :lower)
end
