defmodule Pika.AgentBackend.Conformance do
  @moduledoc "Real Codex App Server, Cursor ACP, and Cursor Headless conformance runner."

  alias Pika.AgentBackend
  alias Pika.AgentBackend.{EventCollector, JSONLWriter, Replay}
  alias Pika.JSONSafe, as: Evidence
  alias Pika.SkillRegistry, as: Skill

  @backend_modules %{
    codex_app_server: Pika.AgentBackend.CodexAppServer,
    cursor_acp: Pika.AgentBackend.CursorACP,
    cursor_headless: Pika.AgentBackend.CursorHeadless
  }

  def run(opts \\ []) do
    workspace = opts |> Keyword.get(:workspace, File.cwd!()) |> Path.expand()

    artifact_dir =
      opts |> Keyword.get(:artifact_dir, "artifacts/backend-conformance") |> Path.expand()

    backends = Keyword.get(opts, :backends, Map.keys(@backend_modules))
    control_probes = Keyword.get(opts, :control_probes, true)

    skill_path =
      Keyword.get(opts, :skill_path, Path.join(workspace, ".pika/skills/ncu-report-skill"))

    File.mkdir_p!(workspace)
    File.mkdir_p!(artifact_dir)
    Pika.MCP.ProbeState.reset()
    skill = Skill.ensure_latest(skill_path)
    {:ok, probe} = Pika.MCP.ProbeServer.start_link()

    try do
      results =
        Map.new(backends, fn backend ->
          module = Map.fetch!(@backend_modules, backend)

          {backend,
           run_backend(backend, module, workspace, artifact_dir, probe.url, skill, control_probes)}
        end)

      schema = Evidence.generate_codex_schema(artifact_dir)
      versions = versions()

      conformance = %{
        generated_at: DateTime.utc_now(),
        status: "passed",
        workspace: workspace,
        skill: Map.drop(skill, [:path]),
        versions: versions,
        codex_schema: schema,
        backends: results
      }

      Evidence.write_json(Path.join(artifact_dir, "conformance.json"), conformance)
      {:ok, conformance}
    after
      Pika.MCP.ProbeServer.stop(probe)
    end
  rescue
    error -> {:error, Exception.format(:error, error, __STACKTRACE__)}
  end

  defp run_backend(backend, module, workspace, artifact_dir, mcp_url, skill, control_probes) do
    {:ok, collector} = EventCollector.start_link()

    registration =
      Pika.MCP.ProbeState.register_session(%{backend: backend, purpose: :conformance})

    profile = %{backend: backend, artifact_dir: artifact_dir}
    {:ok, handle} = AgentBackend.start_link(module, profile, collector)

    {:ok, session} =
      AgentBackend.open_session(
        handle,
        workspace,
        nil,
        :low,
        %{url: mcp_url, token: registration.token},
        [skill.path],
        conformance_instructions(skill.path)
      )

    idempotency_key = "#{backend}-conformance"

    {:ok, probe_turn_id} =
      AgentBackend.start_turn(
        handle,
        "Read the injected skill name. Call get_probe_context, then call complete_probe with " <>
          "idempotency_key #{idempotency_key}. The summary must contain ncu-report-skill. Do not do other work."
      )

    {:ok, probe_completion_event} = wait_turn(collector, probe_turn_id, 180_000)
    completion = wait_mcp_completion(idempotency_key, 10_000)

    unless String.contains?(completion.summary, "ncu-report-skill") do
      raise "#{backend} completion did not prove skill visibility: #{completion.summary}"
    end

    controls =
      if control_probes do
        run_control_probes(handle, collector, backend)
      else
        %{steer: "skipped", interrupt: "skipped"}
      end

    capabilities = AgentBackend.capabilities(handle)

    isolation =
      run_isolation_probe(
        backend,
        module,
        handle,
        workspace,
        artifact_dir,
        mcp_url,
        skill.path,
        capabilities
      )

    owned_os_pid = module.process_os_pid(handle.pid)
    :ok = AgentBackend.close_session(handle)
    {:ok, exit_event} = EventCollector.wait_for(collector, &(&1.type == :process_exited), 10_000)
    Process.sleep(100)

    close_status =
      if is_integer(owned_os_pid) do
        {_output, status} =
          System.cmd("kill", ["-0", Integer.to_string(owned_os_pid)], stderr_to_stdout: true)

        status
      else
        1
      end

    if is_integer(owned_os_pid) and close_status == 0 do
      raise "#{backend} process #{owned_os_pid} remained alive after close_session"
    end

    stable_log = Path.join(artifact_dir, "#{backend}.jsonl")
    File.cp!(session.jsonl_path, stable_log)
    replay = Replay.cursor(stable_log, backend)
    records = JSONLWriter.replay(stable_log)
    assert_no_resume!(backend, records)

    event_types = collector |> EventCollector.events() |> Enum.map(& &1.type)

    %{
      status: "passed",
      protocol: session.backend_protocol,
      session_id: session.id,
      backend_session_id: session.backend_session_id,
      probe_turn_id: probe_turn_id,
      probe_turn_status: event_status(probe_completion_event),
      mcp_completion: completion,
      capabilities: capabilities,
      controls: controls,
      isolation: isolation,
      event_types: Enum.uniq(event_types),
      replay: replay,
      raw_jsonl: Path.basename(stable_log),
      process_exit: Map.put(exit_event.data, :process_alive_after_close, false)
    }
  end

  defp run_control_probes(handle, collector, backend) do
    before_steer = length(EventCollector.events(collector))

    {:ok, original_turn} =
      AgentBackend.start_turn(
        handle,
        "Run the shell command `sleep 15`, then reply ORIGINAL. Start the command immediately."
      )

    {:ok, _} = wait_turn_started_after(collector, original_turn, before_steer)
    Process.sleep(750)

    {:ok, steered_turn} =
      AgentBackend.steer(handle, "Stop waiting and reply STEERED now without running tools.")

    {:ok, steer_completion} = wait_turn(collector, steered_turn, 90_000)

    if backend == :codex_app_server and
         event_status(steer_completion) in ["interrupted", "cancelled"] do
      raise "Codex native turn/steer produced interrupted completion"
    end

    before_interrupt = length(EventCollector.events(collector))

    {:ok, interrupt_turn} =
      AgentBackend.start_turn(
        handle,
        "Run the shell command `sleep 15`, then reply SHOULD_NOT_COMPLETE. Start immediately."
      )

    {:ok, _} = wait_turn_started_after(collector, interrupt_turn, before_interrupt)
    Process.sleep(750)
    :ok = AgentBackend.interrupt(handle)
    {:ok, interrupt_completion} = wait_turn(collector, interrupt_turn, 90_000)

    %{
      steer: %{
        original_turn_id: original_turn,
        resulting_turn_id: steered_turn,
        completion_status: event_status(steer_completion),
        implementation:
          if(backend == :codex_app_server,
            do: "turn/steer",
            else: "session/cancel + session/prompt"
          )
      },
      interrupt: %{
        turn_id: interrupt_turn,
        completion_status: event_status(interrupt_completion)
      }
    }
  end

  defp run_isolation_probe(
         backend,
         module,
         survivor,
         workspace,
         artifact_dir,
         mcp_url,
         skill_path,
         %{process_lifecycle: :per_turn}
       ) do
    run_per_turn_isolation_probe(
      backend,
      module,
      survivor,
      workspace,
      artifact_dir,
      mcp_url,
      skill_path
    )
  end

  defp run_isolation_probe(
         backend,
         module,
         survivor,
         workspace,
         artifact_dir,
         mcp_url,
         skill_path,
         _capabilities
       ) do
    {:ok, crash_collector} = EventCollector.start_link()

    crash_registration =
      Pika.MCP.ProbeState.register_session(%{backend: backend, purpose: :crash_probe})

    {:ok, crash_handle} =
      AgentBackend.start_link(
        module,
        %{backend: backend, artifact_dir: artifact_dir},
        crash_collector
      )

    {:ok, crashed_session} =
      AgentBackend.open_session(
        crash_handle,
        workspace,
        nil,
        :low,
        %{url: mcp_url, token: crash_registration.token},
        [skill_path],
        conformance_instructions(skill_path)
      )

    survivor_os_pid = survivor.module.process_os_pid(survivor.pid)
    crashed_os_pid = crash_handle.module.process_os_pid(crash_handle.pid)

    {_output, 0} =
      System.cmd("kill", ["-KILL", Integer.to_string(crashed_os_pid)], stderr_to_stdout: true)

    {:ok, crash_event} =
      EventCollector.wait_for(crash_collector, &(&1.type == :process_exited), 15_000)

    {_output, survivor_status} =
      System.cmd("kill", ["-0", Integer.to_string(survivor_os_pid)], stderr_to_stdout: true)

    if survivor_status != 0 do
      raise "killing one #{backend} process terminated the survivor"
    end

    {:ok, recovery_collector} = EventCollector.start_link()

    recovery_registration =
      Pika.MCP.ProbeState.register_session(%{backend: backend, purpose: :recovery_probe})

    {:ok, recovery_handle} =
      AgentBackend.start_link(
        module,
        %{backend: backend, artifact_dir: artifact_dir},
        recovery_collector
      )

    {:ok, recovery_session} =
      AgentBackend.open_session(
        recovery_handle,
        workspace,
        nil,
        :low,
        %{url: mcp_url, token: recovery_registration.token},
        [skill_path],
        conformance_instructions(skill_path)
      )

    if recovery_session.backend_session_id == crashed_session.backend_session_id do
      raise "#{backend} recovery reused the crashed provider session"
    end

    :ok = AgentBackend.close_session(recovery_handle)
    recovery_records = JSONLWriter.replay(recovery_session.jsonl_path)
    assert_no_resume!(backend, recovery_records)

    %{
      killed_process_status: crash_event.data.status,
      survivor_process_alive: true,
      crashed_backend_session_id: crashed_session.backend_session_id,
      recovery_backend_session_id: recovery_session.backend_session_id,
      provider_resume_used: false
    }
  end

  defp run_per_turn_isolation_probe(
         backend,
         module,
         survivor,
         workspace,
         artifact_dir,
         mcp_url,
         skill_path
       ) do
    # The main session is idle after its probes. Start a dedicated survivor turn so the
    # per-turn backend owns an OS process while another Session is crashed.
    {:ok, survivor_turn} =
      AgentBackend.start_turn(
        survivor,
        "Run the shell command `sleep 30`, then reply SURVIVOR. Start immediately."
      )

    Process.sleep(750)
    survivor_os_pid = survivor.module.process_os_pid(survivor.pid)
    {:ok, crash_collector} = EventCollector.start_link()

    registration =
      Pika.MCP.ProbeState.register_session(%{backend: backend, purpose: :crash_probe})

    {:ok, crash_handle} =
      AgentBackend.start_link(
        module,
        %{backend: backend, artifact_dir: artifact_dir},
        crash_collector
      )

    {:ok, crashed_session} =
      AgentBackend.open_session(
        crash_handle,
        workspace,
        nil,
        :low,
        %{url: mcp_url, token: registration.token},
        [skill_path],
        conformance_instructions(skill_path)
      )

    {:ok, _crash_turn} =
      AgentBackend.start_turn(
        crash_handle,
        "Run the shell command `sleep 30`, then reply CRASHED. Start immediately."
      )

    Process.sleep(750)
    crashed_os_pid = crash_handle.module.process_os_pid(crash_handle.pid)

    {_output, 0} =
      System.cmd("kill", ["-KILL", Integer.to_string(crashed_os_pid)], stderr_to_stdout: true)

    {:ok, crash_event} =
      EventCollector.wait_for(crash_collector, &(&1.type == :process_exited), 15_000)

    :ok = AgentBackend.close_session(crash_handle)

    {_output, 0} =
      System.cmd("kill", ["-0", Integer.to_string(survivor_os_pid)], stderr_to_stdout: true)

    :ok = AgentBackend.interrupt(survivor)

    {:ok, recovery_collector} = EventCollector.start_link()

    recovery_registration =
      Pika.MCP.ProbeState.register_session(%{backend: backend, purpose: :recovery_probe})

    {:ok, recovery_handle} =
      AgentBackend.start_link(
        module,
        %{backend: backend, artifact_dir: artifact_dir},
        recovery_collector
      )

    {:ok, recovery_session} =
      AgentBackend.open_session(
        recovery_handle,
        workspace,
        nil,
        :low,
        %{url: mcp_url, token: recovery_registration.token},
        [skill_path],
        conformance_instructions(skill_path)
      )

    if recovery_session.backend_session_id == crashed_session.backend_session_id do
      raise "#{backend} recovery reused the crashed provider session"
    end

    :ok = AgentBackend.close_session(recovery_handle)

    %{
      killed_process_status: crash_event.data.status,
      survivor_process_alive: true,
      survivor_turn_id: survivor_turn,
      crashed_backend_session_id: crashed_session.backend_session_id,
      recovery_backend_session_id: recovery_session.backend_session_id,
      provider_resume_used: false
    }
  end

  defp conformance_instructions(skill_path) do
    "Pika backend conformance session. Read #{Path.join(skill_path, "SKILL.md")} before acting."
  end

  defp wait_turn(collector, turn_id, timeout) do
    EventCollector.wait_for(
      collector,
      &(&1.type == :turn_completed and &1.turn_id == turn_id),
      timeout
    )
  end

  defp wait_turn_started_after(collector, turn_id, offset) do
    EventCollector.wait_for(
      collector,
      fn event ->
        events = EventCollector.events(collector)
        event.type == :turn_started and event.turn_id == turn_id and length(events) > offset
      end,
      30_000
    )
  end

  defp wait_mcp_completion(idempotency_key, timeout) do
    deadline = System.monotonic_time(:millisecond) + timeout
    do_wait_mcp_completion(idempotency_key, deadline)
  end

  defp do_wait_mcp_completion(idempotency_key, deadline) do
    case Enum.find(Pika.MCP.ProbeState.completions(), &(&1.idempotency_key == idempotency_key)) do
      nil ->
        if System.monotonic_time(:millisecond) >= deadline do
          raise "timed out waiting for MCP completion #{idempotency_key}"
        else
          Process.sleep(50)
          do_wait_mcp_completion(idempotency_key, deadline)
        end

      completion ->
        completion
    end
  end

  defp assert_no_resume!(backend, records) do
    forbidden =
      case backend do
        :codex_app_server -> "thread/resume"
        :cursor_acp -> "session/load"
        :cursor_headless -> nil
      end

    if forbidden && Enum.any?(records, &(get_in(&1, ["payload", "method"]) == forbidden)) do
      raise "#{backend} recovery used forbidden #{forbidden}"
    end
  end

  defp event_status(event) do
    event.data["status"] || event.data["stopReason"] || event.data[:status] ||
      event.data[:stopReason] || "completed"
  end

  defp versions do
    {codex, 0} = System.cmd("codex", ["--version"], stderr_to_stdout: true)
    {cursor, 0} = System.cmd("cursor-agent", ["--version"], stderr_to_stdout: true)
    {elixir, 0} = System.cmd("elixir", ["--version"], stderr_to_stdout: true)

    %{
      codex: String.trim(codex),
      cursor: String.trim(cursor),
      elixir: elixir |> String.trim() |> String.split("\n") |> List.last()
    }
  end
end
