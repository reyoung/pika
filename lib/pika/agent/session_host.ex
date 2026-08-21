defmodule Pika.Agent.SessionHost do
  @moduledoc "Owns one Actor's Backend Session, token, frozen profile, and audit lifecycle."

  alias Pika.Agent.{Directory, Profile}
  alias Pika.AgentBackend
  alias Pika.Agent.Role.{Prepared, Progress}
  alias Pika.{ArtifactStore, Repo}

  @enforce_keys [:handle, :session, :token, :token_hash, :directory, :prepared]
  defstruct @enforce_keys ++ [:monitor, :active_turn_id]

  def open(%Prepared{} = prepared, opts) do
    directory = Keyword.get(opts, :directory, Directory)
    backend_modules = Keyword.get(opts, :backend_modules, %{})
    actor = self()

    with {:ok, token} <-
           Directory.issue(
             actor,
             prepared.work,
             prepared.definition.id,
             prepared.definition.tools,
             directory
           ),
         {:ok, backend, module} <- Profile.backend(prepared.profile, backend_modules) do
      backend_profile =
        Profile.backend_profile(prepared.profile, backend, prepared.workspace, prepared.work)

      case AgentBackend.start_link(module, backend_profile, actor) do
        {:ok, handle} ->
          open_backend(prepared, opts, directory, token, handle, backend_profile)

        {:error, reason} ->
          Directory.revoke(token, directory)
          {:error, {:backend_start_failed, reason}}
      end
    end
  end

  def start_turn(%__MODULE__{} = host, prompt) do
    case AgentBackend.start_turn(host.handle, prompt) do
      {:ok, turn_id} ->
        update_status(host.session.id, "running", host.prepared.progress, turn_increment: 1)
        {:ok, %{host | active_turn_id: turn_id}}

      {:error, reason} ->
        {:error, {:turn_start_failed, reason}}
    end
  end

  def record_event(%__MODULE__{} = host, event) do
    _ = append_log(host, event_record(host, event))
    _ = update_status(host.session.id, "running", host.prepared.progress, event_increment: 1)
    host
  end

  def awaiting(%__MODULE__{} = host, %Progress{} = progress) do
    _ = update_status(host.session.id, "awaiting_report", progress)
    %{host | prepared: %{host.prepared | progress: progress}, active_turn_id: nil}
  end

  def close(%__MODULE__{} = host, status, %Progress{} = progress)
      when status in ~w(completed failed stopped interrupted) do
    Directory.revoke(host.token, host.directory)
    safe_close(host.handle)
    _ = update_status(host.session.id, status, progress)
    :ok
  end

  defp open_backend(prepared, opts, directory, token, handle, backend_profile) do
    mcp = %{
      url: Keyword.get(opts, :mcp_url, Profile.mcp_url(prepared.workspace)),
      token: token,
      role: compatibility_role(prepared.definition.id),
      work_kind: prepared.work.kind,
      work_id: prepared.work.id,
      attempt_id: if(prepared.work.kind == :attempt, do: prepared.work.id),
      sync_run_id: if(prepared.work.kind == :sync, do: prepared.work.id),
      coordinator: self()
    }

    result =
      AgentBackend.open_session(
        handle,
        prepared.cwd,
        Profile.model(prepared.profile),
        Profile.reasoning_effort(prepared.profile),
        mcp,
        prepared.skill_roots,
        prepared.instructions.system
      )

    case result do
      {:ok, session} ->
        with :ok <- Directory.bind_session(token, session.id, directory),
             :ok <- persist_open(prepared, session, handle, backend_profile, token, opts) do
          host = %__MODULE__{
            handle: handle,
            session: session,
            token: token,
            token_hash: token_hash(token),
            directory: directory,
            prepared: prepared,
            monitor: Process.monitor(handle.pid),
            active_turn_id: nil
          }

          _ =
            append_log(host, %{
              at: DateTime.utc_now() |> DateTime.to_iso8601(),
              type: "session_opened",
              role: prepared.definition.id,
              work_kind: prepared.work.kind,
              work_id: prepared.work.id
            })

          {:ok, host}
        else
          {:error, reason} ->
            Directory.revoke(token, directory)
            safe_close(handle)
            {:error, {:session_audit_failed, reason}}
        end

      {:error, reason} ->
        Directory.revoke(token, directory)
        safe_close(handle)
        {:error, {:backend_session_open_failed, reason}}
    end
  end

  defp persist_open(prepared, session, handle, backend_profile, token, opts) do
    relative = Path.join(["artifacts", "prompts", "agent-sessions", "#{session.id}.md"])

    with {:ok, artifact} <-
           ArtifactStore.write(
             prepared.workspace,
             relative,
             prepared.instructions.system <> "\n",
             %{
               campaign_id: prepared.work.campaign_id,
               owner_type: "agent_session",
               owner_id: session.id,
               kind: "agent_instructions",
               mime_type: "text/markdown",
               metadata: %{
                 role: prepared.definition.id,
                 work_kind: prepared.work.kind,
                 work_id: prepared.work.id,
                 instructions_sha256: prepared.instructions.sha256,
                 template_sha256: prepared.instructions.template_sha256
               }
             }
           ) do
      now = System.system_time(:microsecond)
      capabilities = AgentBackend.capabilities(handle)

      Repo.query!(
        """
        INSERT INTO agent_sessions(
          id, campaign_id, attempt_id, sync_run_id, role, slot_index, profile_json, backend,
          backend_protocol, provider_session_id, backend_capabilities_json, model,
          reasoning_effort, status, process_pid, process_started_at, mcp_token_hash,
          required_operations_json, work_kind, work_id, role_contract_revision, session_mode,
          role_definition_sha256, template_sha256, instructions_sha256, instructions_artifact_id,
          last_turn_sequence, last_event_seq, started_at
        ) VALUES (?, ?, ?, ?, ?, NULL, ?, ?, ?, ?, ?, ?, ?, 'running', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, 0, ?)
        """,
        [
          session.id,
          prepared.work.campaign_id,
          compatibility_id(prepared.work.kind, :attempt, prepared.work.id),
          compatibility_id(prepared.work.kind, :sync, prepared.work.id),
          prepared.definition.id,
          Jason.encode!(Pika.JSONSafe.json_safe(backend_profile)),
          to_string(session.backend),
          session.backend_protocol,
          session.backend_session_id,
          Jason.encode!(Pika.JSONSafe.json_safe(capabilities)),
          session.model,
          to_string(session.reasoning_effort || ""),
          inspect(self()),
          now,
          token_hash(token),
          Jason.encode!(prepared.progress.required_operations),
          Atom.to_string(prepared.work.kind),
          prepared.work.id,
          prepared.definition.contract_revision,
          Atom.to_string(Keyword.get(opts, :session_mode, :fresh)),
          digest(prepared.definition),
          prepared.instructions.template_sha256,
          prepared.instructions.sha256,
          artifact.id,
          now
        ]
      )

      :ok
    end
  rescue
    error -> {:error, {:agent_session_persist_failed, Exception.message(error)}}
  end

  defp update_status(session_id, status, progress, increments \\ []) do
    ended_at =
      if status in ~w(completed failed stopped interrupted),
        do: System.system_time(:microsecond),
        else: nil

    Repo.query!(
      """
      UPDATE agent_sessions
      SET status = ?, required_operations_json = ?,
          last_turn_sequence = last_turn_sequence + ?,
          last_event_seq = last_event_seq + ?, ended_at = COALESCE(?, ended_at)
      WHERE id = ?
      """,
      [
        status,
        Jason.encode!(progress.required_operations),
        Keyword.get(increments, :turn_increment, 0),
        Keyword.get(increments, :event_increment, 0),
        ended_at,
        session_id
      ]
    )

    :ok
  rescue
    error -> {:error, {:agent_session_update_failed, Exception.message(error)}}
  end

  defp safe_close(handle) do
    AgentBackend.close_session(handle)
  catch
    _, _ -> :ok
  end

  defp append_log(host, record) do
    work = host.prepared.work

    attrs = %{
      campaign_id: work.campaign_id,
      owner_type: owner_type(work.kind),
      owner_id: work.id,
      kind: "agent_jsonl",
      metadata: %{session_id: host.session.id, role: host.prepared.definition.id}
    }

    with {:ok, artifact} <-
           ArtifactStore.append_jsonl(host.prepared.workspace, log_path(host), record, attrs) do
      Pika.AttemptStore.attach_session_log(host.session.id, artifact.id)
    end
  end

  defp event_record(host, event) do
    %{
      at: DateTime.utc_now() |> DateTime.to_iso8601(),
      session_id: event.session_id,
      turn_id: event.turn_id,
      type: event.type,
      backend: event.backend,
      role: host.prepared.definition.id,
      data: Pika.JSONSafe.json_safe(event.data)
    }
  end

  defp log_path(host) do
    work = host.prepared.work

    case work.kind do
      :attempt when work.role_id in ["plan", "iteration"] ->
        Path.join(["artifacts", "logs", work.id, "#{host.session.id}.jsonl"])

      :attempt ->
        Path.join(["artifacts", "logs", work.id, work.role_id, "#{host.session.id}.jsonl"])

      _ ->
        Path.join([
          "artifacts",
          "logs",
          work.role_id,
          work.id,
          "#{host.session.id}.jsonl"
        ])
    end
  end

  defp owner_type(:attempt), do: "attempt"
  defp owner_type(:sync), do: "sync"
  defp owner_type(:progress_summary), do: "progress_summary"
  defp owner_type(kind), do: Atom.to_string(kind)

  defp compatibility_role("plan"), do: :plan
  defp compatibility_role("iteration"), do: :iteration
  defp compatibility_role(role_id), do: role_id

  defp compatibility_id(:attempt, :attempt, id), do: id
  defp compatibility_id(:sync, :sync, id), do: id
  defp compatibility_id(_, _, _), do: nil

  defp digest(value),
    do: :crypto.hash(:sha256, :erlang.term_to_binary(value)) |> Base.encode16(case: :lower)

  defp token_hash(token),
    do: :crypto.hash(:sha256, token) |> Base.encode16(case: :lower)
end
