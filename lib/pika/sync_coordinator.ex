defmodule Pika.SyncCoordinator do
  @moduledoc false

  use GenServer

  alias Pika.AgentBackend
  alias Pika.Alignment.ArtifactStore, as: AlignmentArtifactStore
  alias Pika.{AttemptStore, SyncPrompt, SyncStore, SyncWorkspace}

  @read_tools ~w(get_sync_context)
  @write_tools ~w(register_artifact report_sync_candidate submit_sync_validation create_sync_intent complete_sync)

  def start_link(opts \\ []) do
    GenServer.start_link(__MODULE__, opts, name: Keyword.get(opts, :name, __MODULE__))
  end

  def preview(remote, branch, server \\ __MODULE__),
    do: call(server, {:preview, remote, branch})

  def request(remote, branch, idempotency_key, server \\ __MODULE__),
    do: call(server, {:request, remote, branch, idempotency_key})

  def confirm_spec(run_id, approved, idempotency_key, server \\ __MODULE__),
    do: call(server, {:confirm_spec, run_id, approved, idempotency_key})

  def snapshot(server \\ __MODULE__), do: call(server, :snapshot)
  def stop_now(server \\ __MODULE__), do: call(server, :stop_now)
  def resume(server \\ __MODULE__), do: call(server, :resume)
  def authorize(token, server \\ __MODULE__), do: call(server, {:authorize, token})

  def mcp_call(token, tool, args, server \\ __MODULE__),
    do: call(server, {:mcp, token, tool, args})

  @impl true
  def init(opts) do
    workspace = Keyword.get_lazy(opts, :workspace, &Pika.WorkspaceLock.workspace/0)
    campaign = Keyword.get_lazy(opts, :campaign, &Pika.Persistence.current_campaign/0)

    if Process.whereis(Pika.PubSub) do
      Phoenix.PubSub.subscribe(Pika.PubSub, Pika.Persistence.topic(campaign.id))
      Phoenix.PubSub.subscribe(Pika.PubSub, "pika:sync:events")
    end

    state = %{
      workspace: workspace,
      campaign_id: campaign.id,
      profile: Keyword.get(opts, :profile, profile(workspace)),
      backend_modules: Keyword.get(opts, :backend_modules, %{}),
      mcp_url: Keyword.get(opts, :mcp_url, mcp_url(workspace)),
      start_backends: Keyword.get(opts, :start_backends, true),
      recovery_enabled: campaign.status not in ~w(stopped blocked completed),
      after_remote_push: Keyword.get(opts, :after_remote_push),
      session: nil,
      token_hash: nil,
      monitor: nil,
      last_error: nil
    }

    send(self(), :scan)
    {:ok, state}
  end

  @impl true
  def handle_call(:snapshot, _from, state) do
    {:reply,
     %{
       preview: configured_preview(state),
       run: optional(&SyncStore.latest_run/1, state.campaign_id),
       session: state.session && public_session(state.session),
       last_error: state.last_error
     }, state}
  end

  def handle_call({:preview, remote, branch}, _from, state),
    do: {:reply, SyncWorkspace.preview(state.workspace, remote, branch), state}

  def handle_call({:request, remote, branch, key}, _from, state) do
    result =
      with {:ok, preview} <- SyncWorkspace.preview(state.workspace, remote, branch),
           true <- preview.best_sha == Pika.Persistence.current_campaign().best_sha,
           {:ok, run} <-
             SyncStore.request(
               state.campaign_id,
               remote,
               branch,
               preview.best_sha,
               preview.remote_sha,
               key
             ) do
        {:ok, run}
      else
        false -> {:error, :stale_best}
        {:error, _} = error -> error
      end

    if match?({:ok, _}, result), do: send(self(), :scan)
    {:reply, result, state}
  end

  def handle_call({:confirm_spec, run_id, approved, key}, _from, state) do
    result = SyncStore.confirm_spec(run_id, approved, key)

    case result do
      {:ok, %{status: "failed"} = run} -> SyncWorkspace.cleanup(state.workspace, run)
      _ -> :ok
    end

    if match?({:ok, _}, result), do: send(self(), :scan)
    {:reply, result, state}
  end

  def handle_call(:stop_now, _from, %{session: nil} = state),
    do: {:reply, :ok, %{state | recovery_enabled: false}}

  def handle_call(:stop_now, _from, state) do
    _ = AgentBackend.interrupt(state.session.handle)
    _ = AttemptStore.update_session(state.session.session.id, "stopped", required(state))
    safe_close(state.session.handle)
    if state.monitor, do: Process.demonitor(state.monitor, [:flush])

    {:reply, :ok, %{state | session: nil, token_hash: nil, monitor: nil, recovery_enabled: false}}
  end

  def handle_call(:resume, _from, state) do
    send(self(), :scan)
    {:reply, :ok, %{state | recovery_enabled: true}}
  end

  def handle_call({:authorize, token}, _from, state) do
    {:reply, if(state.token_hash == token_hash(token), do: :ok, else: {:error, :unauthorized}),
     state}
  end

  def handle_call({:mcp, token, tool, args}, _from, state) do
    if state.session && state.token_hash == token_hash(token) do
      execute_mcp(tool, stringify_keys(args), state)
    else
      {:reply, mcp_error("unauthorized", "invalid Sync Session token"), state}
    end
  end

  @impl true
  def handle_info(:scan, %{session: nil, recovery_enabled: true} = state),
    do: {:noreply, scan(state)}

  def handle_info(:scan, state), do: {:noreply, state}

  def handle_info({:domain_event, _event}, state) do
    if state.recovery_enabled, do: send(self(), :scan)
    {:noreply, state}
  end

  def handle_info({:sync_event, _event}, state) do
    if state.recovery_enabled, do: send(self(), :scan)
    {:noreply, state}
  end

  def handle_info({:pika_backend_event, event}, %{session: session} = state)
      when not is_nil(session) do
    if event.session_id == session.session.id do
      state = persist_event(state, event)
      {:noreply, apply_event(state, event)}
    else
      {:noreply, state}
    end
  end

  def handle_info({:DOWN, monitor, :process, _pid, reason}, %{monitor: monitor} = state) do
    {:noreply, recover_session(state, {:process_down, reason})}
  end

  def handle_info(_message, state), do: {:noreply, state}

  @impl true
  def terminate(_reason, %{session: nil}), do: :ok
  def terminate(_reason, state), do: safe_close(state.session.handle)

  defp scan(state) do
    case SyncStore.active_run(state.campaign_id) do
      nil ->
        state

      %{status: "requested"} = run ->
        if SyncStore.ready_to_start?(state.campaign_id), do: prepare(state, run), else: state

      %{status: status} = run when status in ~w(preparing fetching) ->
        prepare(state, run)

      %{status: "awaiting_spec_confirmation"} ->
        state

      %{status: status} = run when status in ~w(pushing advancing_best) ->
        recover_external(state, run)

      run ->
        open_session(state, run, true)
    end
  end

  defp prepare(state, run) do
    with {:ok, _} <- SyncStore.begin_prepare(run.id),
         {:ok, _workspace} <- SyncWorkspace.prepare(state.workspace, run),
         {:ok, run} <- SyncStore.mark_merging(run.id) do
      open_session(state, run, false)
    else
      {:error, reason} ->
        _ = SyncStore.block(run.id, reason)
        %{state | last_error: inspect(reason)}
    end
  end

  defp open_session(%{start_backends: false} = state, _run, _recovering), do: state

  defp open_session(state, run, recovering) do
    profile = state.profile
    backend = backend_atom(profile["backend"] || profile[:backend])
    module = Map.get(state.backend_modules, backend, backend_module(backend))
    token = random_token()
    effort = effort_atom(profile["reasoning_effort"] || profile[:reasoning_effort] || "high")
    {command, args} = backend_command(backend, profile["command"] || profile[:command])

    backend_profile = %{
      backend: backend,
      command: command,
      args: args,
      env: profile["env"] || profile[:env] || %{},
      protocol_config: profile["protocol_config"] || profile[:protocol_config] || %{},
      artifact_dir: Path.join([state.workspace.artifacts, "logs", "sync", run.id])
    }

    with :ok <- Pika.PromptCatalog.validate([:sync]),
         {:ok, context} <- AttemptStore.campaign_context(state.campaign_id),
         {:ok, instructions} <- SyncPrompt.render(run, context),
         {:ok, handle} <- AgentBackend.start_link(module, backend_profile, self()),
         {:ok, session} <-
           AgentBackend.open_session(
             handle,
             Path.join(state.workspace.root, run.worktree_relative_path),
             profile["model"] || profile[:model],
             effort,
             %{
               url: state.mcp_url,
               token: token,
               role: :sync,
               sync_run_id: run.id,
               coordinator: self()
             },
             skill_roots(state.workspace),
             recovery_instructions(instructions, recovering)
           ) do
      required = required_for(run)

      identity = %{
        session_id: session.id,
        campaign_id: state.campaign_id,
        attempt_id: nil,
        sync_run_id: run.id,
        role: :sync,
        slot_index: nil,
        token_hash: token_hash(token)
      }

      :ok =
        AttemptStore.insert_session(
          state.campaign_id,
          identity,
          session,
          backend_profile,
          required
        )

      monitor = Process.monitor(handle.pid)

      session_state = %{
        handle: handle,
        session: session,
        identity: identity,
        required: MapSet.new(required),
        active_turn_id: nil,
        log_path: "artifacts/logs/sync/#{run.id}/#{session.id}.jsonl"
      }

      state = %{state | session: session_state, token_hash: identity.token_hash, monitor: monitor}
      state = ensure_log(state)

      case AgentBackend.start_turn(handle, kickoff(run, recovering)) do
        {:ok, turn_id} -> put_in(state.session.active_turn_id, turn_id)
        {:error, reason} -> recover_session(state, {:start_turn_failed, reason})
      end
    else
      {:error, reason} -> %{state | last_error: inspect(reason)}
    end
  end

  defp execute_mcp(tool, args, state) when tool in @read_tools do
    {response, state} = perform_read(tool, args, state)
    {:reply, response, state}
  end

  defp execute_mcp(tool, args, state) when tool in @write_tools do
    key = args["idempotency_key"]

    if not is_binary(key) or key == "" do
      {:reply, mcp_error("missing_required_data", "idempotency_key is required"), state}
    else
      session_id = state.session.session.id
      request_hash = hash({tool, args})

      case AttemptStore.lookup_idempotency(session_id, tool, key, request_hash) do
        {:replay, response} ->
          {:reply, response, state}

        :conflict ->
          {:reply, mcp_error("idempotency_conflict", "request changed"), state}

        :missing ->
          {response, next} = perform_write(tool, args, state)
          :ok = AttemptStore.store_idempotency(session_id, tool, key, request_hash, response)
          {:reply, response, next}
      end
    end
  end

  defp execute_mcp(tool, _args, state),
    do: {:reply, mcp_error("forbidden_role", "tool is unavailable: #{tool}"), state}

  defp perform_read("get_sync_context", _args, state) do
    run_id = state.session.identity.sync_run_id

    with {:ok, run} <- SyncStore.run(run_id),
         {:ok, context} <- AttemptStore.campaign_context(state.campaign_id) do
      {{:ok,
        %{
          identity: public_identity(state.session.identity),
          sync_run: run,
          campaign: context,
          required_operations: required(state)
        }}, state}
    else
      {:error, reason} -> {mcp_error("missing_required_data", inspect(reason)), state}
    end
  end

  defp perform_write("register_artifact", args, state) do
    run_id = state.session.identity.sync_run_id

    attrs = %{
      sha256: args["sha256"],
      size: args["size"],
      mime: args["mime"],
      kind: args["kind"],
      metadata: args["metadata"] || %{}
    }

    with {:ok, _} <-
           AlignmentArtifactStore.register(state.workspace.root, args["relative_path"], attrs),
         {:ok, artifact} <-
           Pika.ArtifactStore.register(state.workspace, args["relative_path"], %{
             campaign_id: state.campaign_id,
             owner_type: "sync",
             owner_id: run_id,
             kind: args["kind"],
             mime_type: args["mime"],
             metadata: args["metadata"] || %{}
           }) do
      {{:ok, public_artifact(artifact)}, state}
    else
      {:error, reason} ->
        {mcp_error("missing_required_data", "artifact rejected", %{reason: reason}), state}
    end
  end

  defp perform_write("report_sync_candidate", args, state) do
    run_id = state.session.identity.sync_run_id

    with {:ok, run} <- SyncStore.run(run_id),
         {:ok, context} <- AttemptStore.campaign_context(state.campaign_id),
         {:ok, verification} <-
           SyncWorkspace.verify_candidate(
             state.workspace,
             run,
             args["candidate_sha"],
             context.protected_paths
           ),
         {:ok, digest} <- candidate_harness_digest(state, run, context),
         true <- args["harness_digest"] == digest,
         {:ok, updated} <-
           SyncStore.report_candidate(
             run_id,
             args["candidate_sha"],
             verification.protected_paths,
             digest
           ) do
      next =
        if(updated.status == "awaiting_spec_confirmation",
          do: [],
          else: ["submit_sync_validation"]
        )

      {{:ok, updated}, set_required(state, next)}
    else
      false ->
        {mcp_error("identity_mismatch", "Harness digest mismatch"), state}

      {:error, reason} ->
        {mcp_error("missing_required_data", "Sync candidate rejected", %{reason: reason}), state}
    end
  end

  defp perform_write("submit_sync_validation", args, state) do
    run_id = state.session.identity.sync_run_id

    with {:ok, run} <- SyncStore.run(run_id),
         {:ok, context} <- AttemptStore.campaign_context(state.campaign_id),
         true <- args["base_sha"] == run.base_sha and args["candidate_sha"] == run.candidate_sha,
         {:ok, samples} <- registered_artifact(state, args["samples_artifact"], run_id),
         {:ok, correctness} <- registered_artifact(state, args["correctness_artifact"], run_id),
         {:ok, samples_path} <-
           Pika.ArtifactStore.resolve(state.workspace, samples.relative_path),
         {:ok, correctness_path} <-
           Pika.ArtifactStore.resolve(state.workspace, correctness.relative_path),
         {:ok, metrics} <-
           Pika.Measurement.evaluate_iteration(samples_path, correctness_path, %{
             base_sha: run.base_sha,
             candidate_sha: run.candidate_sha,
             case_ids: Enum.map(context.cases, & &1["id"]),
             metrics: context.metrics,
             benchmark: context.spec["benchmark"]
           }),
         {:ok, updated} <-
           SyncStore.record_validation(run_id, metrics, correctness.id, samples.id) do
      {{:ok, %{sync_run: updated, metrics: metrics}}, set_required(state, ["create_sync_intent"])}
    else
      false ->
        {mcp_error("identity_mismatch", "Sync validation identity mismatch"), state}

      {:error, reason} ->
        {mcp_error("missing_required_data", "Sync validation rejected", %{reason: reason}), state}
    end
  end

  defp perform_write("create_sync_intent", args, state) do
    run_id = state.session.identity.sync_run_id

    case SyncStore.create_intent(run_id, args["idempotency_key"]) do
      {:ok, intent} -> {{:ok, intent}, set_required(state, ["complete_sync"])}
      {:error, reason} -> {mcp_error("missing_required_data", inspect(reason)), state}
    end
  end

  defp perform_write("complete_sync", _args, state) do
    run_id = state.session.identity.sync_run_id

    case SyncStore.run(run_id) do
      {:ok, run} ->
        case push_and_complete(state, run) do
          {:ok, completed, next} ->
            {{:ok, completed}, set_required(next, [])}

          {:error, reason, next} ->
            next =
              case SyncStore.run(run_id) do
                {:ok, %{status: status} = terminal} when status in ~w(failed blocked) ->
                  _ = SyncWorkspace.cleanup(state.workspace, terminal)
                  set_required(next, [])

                _ ->
                  next
              end

            {mcp_error("external_state_error", inspect(reason)), next}
        end

      {:error, reason} ->
        {mcp_error("missing_required_data", inspect(reason)), state}
    end
  end

  defp apply_event(state, %{type: :turn_started, turn_id: turn_id}) do
    _ =
      AttemptStore.update_session(state.session.session.id, "running", required(state), %{
        turn_increment: 1
      })

    put_in(state.session.active_turn_id, turn_id)
  end

  defp apply_event(state, %{type: :turn_completed}) do
    state = put_in(state.session.active_turn_id, nil)

    if MapSet.size(state.session.required) == 0 do
      finish_session(state)
    else
      missing = required(state)
      _ = AttemptStore.update_session(state.session.session.id, "awaiting_report", missing)

      case AgentBackend.start_turn(
             state.session.handle,
             "Sync completion gate remains open. Complete: #{Enum.join(missing, ", ")}."
           ) do
        {:ok, turn_id} -> put_in(state.session.active_turn_id, turn_id)
        {:error, reason} -> recover_session(state, {:followup_failed, reason})
      end
    end
  end

  defp apply_event(state, %{type: type, data: data})
       when type in [:backend_error, :process_exited],
       do: recover_session(state, {type, data})

  defp apply_event(state, _event) do
    _ =
      AttemptStore.update_session(state.session.session.id, "running", required(state), %{
        event_increment: 1
      })

    state
  end

  defp finish_session(state) do
    _ = AttemptStore.update_session(state.session.session.id, "completed", [])
    safe_close(state.session.handle)
    if state.monitor, do: Process.demonitor(state.monitor, [:flush])
    send(self(), :scan)
    %{state | session: nil, token_hash: nil, monitor: nil}
  end

  defp recover_session(%{session: nil} = state, _reason), do: state

  defp recover_session(state, reason) do
    _ = AttemptStore.update_session(state.session.session.id, "interrupted", required(state))
    safe_close(state.session.handle)
    if state.monitor, do: Process.demonitor(state.monitor, [:flush])
    if state.recovery_enabled, do: Process.send_after(self(), :scan, 50)
    %{state | session: nil, token_hash: nil, monitor: nil, last_error: inspect(reason)}
  end

  defp recover_external(state, run) do
    case SyncWorkspace.remote_sha(state.workspace.repo, run.remote, run.branch) do
      {:ok, sha} when sha == run.candidate_sha ->
        with {:ok, run} <- ensure_advancing(run),
             :ok <- SyncWorkspace.advance_best(state.workspace, run.base_sha, run.candidate_sha),
             {:ok, trail} <- write_trail(state, run, "recovered_after_remote_push"),
             {:ok, completed} <- SyncStore.complete(run.id, trail.id) do
          _ = SyncWorkspace.cleanup(state.workspace, completed)
          _ = Pika.Control.reconcile(state.campaign_id)
          state
        else
          {:error, reason} -> %{state | last_error: inspect(reason)}
        end

      {:ok, sha} when sha == run.remote_before_sha ->
        case push_and_complete(state, run) do
          {:ok, _completed, next} -> next
          {:error, _reason, next} -> next
        end

      {:ok, third_sha} ->
        _ = SyncStore.block(run.id, {:unexpected_remote_sha, third_sha})
        %{state | last_error: "remote changed to an unrelated SHA: #{third_sha}"}

      {:error, reason} ->
        %{state | last_error: inspect(reason)}
    end
  end

  defp push_and_complete(state, run) do
    with {:ok, _intent} <- SyncStore.intent_for_run(run.id),
         :ok <- SyncWorkspace.push(state.workspace, run, run.candidate_sha),
         {:ok, run} <- SyncStore.mark_advancing(run.id, run.candidate_sha),
         :ok <- after_remote_push(state.after_remote_push, run),
         :ok <- SyncWorkspace.advance_best(state.workspace, run.base_sha, run.candidate_sha),
         {:ok, trail} <- write_trail(state, run, "completed"),
         {:ok, completed} <- SyncStore.complete(run.id, trail.id) do
      _ = SyncWorkspace.cleanup(state.workspace, completed)
      _ = Pika.Control.reconcile(state.campaign_id)
      {:ok, completed, state}
    else
      {:error, :injected_crash} = error ->
        {_, next} = external_crash(state, error)
        {:error, error, next}

      {:error, reason} ->
        _ = maybe_fail_before_remote_advance(state, run, reason)
        {:error, reason, %{state | last_error: inspect(reason)}}
    end
  end

  defp ensure_advancing(%{status: "advancing_best"} = run), do: {:ok, run}
  defp ensure_advancing(run), do: SyncStore.mark_advancing(run.id, run.candidate_sha)

  defp maybe_fail_before_remote_advance(state, run, reason) do
    case SyncWorkspace.remote_sha(state.workspace.repo, run.remote, run.branch) do
      {:ok, sha} when sha == run.remote_before_sha -> SyncStore.fail(run.id, reason)
      _ -> :ok
    end
  end

  defp external_crash(state, reason) do
    if state.session do
      _ = AttemptStore.update_session(state.session.session.id, "interrupted", required(state))
      safe_close(state.session.handle)
    end

    if state.monitor, do: Process.demonitor(state.monitor, [:flush])
    Process.send_after(self(), :scan, 50)
    {reason, %{state | session: nil, token_hash: nil, monitor: nil, last_error: inspect(reason)}}
  end

  defp after_remote_push(nil, _run), do: :ok
  defp after_remote_push(fun, run) when is_function(fun, 1), do: fun.(run)

  defp candidate_harness_digest(state, run, context) do
    spec = context.spec
    reference = get_in(spec, ["computation", "reference_path"])
    benchmark = get_in(spec, ["benchmark", "harness_path"])
    correctness = context.protected_paths -- [reference, benchmark]
    root = Path.join(state.workspace.root, run.worktree_relative_path)

    Pika.Harness.validate(root, %{
      "reference_path" => reference,
      "benchmark_path" => benchmark,
      "correctness_paths" => correctness,
      "protected_paths" => context.protected_paths
    })
    |> case do
      {:ok, harness} -> {:ok, harness.digest}
      {:error, _} = error -> error
    end
  end

  defp registered_artifact(state, path, run_id) do
    with {:ok, artifact} <- AttemptStore.artifact(state.campaign_id, path),
         true <- artifact.owner_type == "sync" and artifact.owner_id == run_id do
      {:ok, artifact}
    else
      false -> {:error, :artifact_identity_mismatch}
      {:error, _} = error -> error
    end
  end

  defp write_trail(state, run, outcome) do
    Pika.ArtifactStore.write(
      state.workspace,
      "artifacts/logs/sync/#{run.id}/trail.json",
      Jason.encode!(
        %{
          sync_run_id: run.id,
          remote: run.remote,
          branch: run.branch,
          before_sha: run.remote_before_sha,
          candidate_sha: run.candidate_sha,
          outcome: outcome,
          recorded_at: DateTime.utc_now()
        },
        pretty: true
      ) <> "\n",
      %{
        campaign_id: state.campaign_id,
        owner_type: "sync",
        owner_id: run.id,
        kind: "sync_trail",
        mime_type: "application/json",
        metadata: %{outcome: outcome}
      }
    )
  end

  defp persist_event(state, event) do
    record = %{
      at: DateTime.utc_now() |> DateTime.to_iso8601(),
      session_id: event.session_id,
      turn_id: event.turn_id,
      type: event.type,
      backend: event.backend,
      data: Pika.JSONSafe.json_safe(event.data)
    }

    append_log(state, record)
  end

  defp ensure_log(state),
    do:
      append_log(state, %{
        at: DateTime.utc_now() |> DateTime.to_iso8601(),
        type: "session_opened",
        role: "sync"
      })

  defp append_log(state, record) do
    attrs = %{
      campaign_id: state.campaign_id,
      owner_type: "sync",
      owner_id: state.session.identity.sync_run_id,
      kind: "agent_jsonl",
      metadata: %{session_id: state.session.session.id, role: :sync}
    }

    case Pika.ArtifactStore.append_jsonl(state.workspace, state.session.log_path, record, attrs) do
      {:ok, artifact} ->
        _ = AttemptStore.attach_session_log(state.session.session.id, artifact.id)
        state

      {:error, reason} ->
        %{state | last_error: inspect(reason)}
    end
  end

  defp set_required(state, operations) do
    required = MapSet.new(operations)
    _ = AttemptStore.update_session(state.session.session.id, "running", MapSet.to_list(required))
    put_in(state.session.required, required)
  end

  defp required_for(%{status: "merging"}), do: ["report_sync_candidate"]

  defp required_for(%{status: "validating", validation_metrics: nil}),
    do: ["submit_sync_validation"]

  defp required_for(%{status: "validating"}), do: ["create_sync_intent"]
  defp required_for(%{status: "pushing"}), do: ["complete_sync"]
  defp required_for(_run), do: []

  defp required(state), do: state.session.required |> MapSet.to_list() |> Enum.sort()

  defp configured_preview(state) do
    sync = get_in(state.workspace.snapshot, ["mutable", "sync"]) || %{}
    remote = sync["remote"]
    branch = sync["branch"]

    if is_binary(remote) and is_binary(branch),
      do: optional(fn _ -> SyncWorkspace.preview(state.workspace, remote, branch) end, nil),
      else: nil
  end

  defp profile(workspace) do
    backend = get_in(workspace.snapshot, ["immutable", "backend"])

    %{
      "name" => "sync",
      "backend" => backend["type"],
      "command" => backend["command"],
      "model" => nil,
      "reasoning_effort" => "high",
      "env" => %{},
      "protocol_config" => backend["protocol_config"] || %{}
    }
  end

  defp mcp_url(workspace) do
    listen = get_in(workspace.snapshot, ["immutable", "listen"])
    host = if listen["host"] in ["0.0.0.0", "::"], do: "127.0.0.1", else: listen["host"]
    "http://#{host}:#{listen["port"]}/mcp"
  end

  defp skill_roots(workspace),
    do: Enum.filter([Path.join(workspace.root, ".pika/skills/ncu-report-skill")], &File.dir?/1)

  defp recovery_instructions(instructions, false), do: instructions

  defp recovery_instructions(instructions, true),
    do: instructions <> "\n\nRecover the persisted Sync Run and Intent before doing new work."

  defp kickoff(run, false), do: "Continue the user-confirmed Sync Run #{run.id}."
  defp kickoff(run, true), do: "Recover the user-confirmed Sync Run #{run.id}."

  defp backend_command(_backend, [command, "app-server", "--listen", "stdio://"]),
    do: {command, []}

  defp backend_command(_backend, [command, "acp"]), do: {command, []}
  defp backend_command(_backend, [command | args]), do: {command, args}
  defp backend_command(:cursor_acp, nil), do: {"cursor-agent", []}
  defp backend_command(_backend, nil), do: {"codex", []}
  defp backend_atom(:cursor_acp), do: :cursor_acp
  defp backend_atom("cursor_acp"), do: :cursor_acp
  defp backend_atom(_), do: :codex_app_server
  defp backend_module(:cursor_acp), do: Pika.AgentBackend.CursorACP
  defp backend_module(:codex_app_server), do: Pika.AgentBackend.CodexAppServer
  defp effort_atom(value) when is_atom(value), do: value
  defp effort_atom(value), do: String.to_existing_atom(value)
  defp random_token, do: :crypto.strong_rand_bytes(32) |> Base.url_encode64(padding: false)
  defp token_hash(token), do: :crypto.hash(:sha256, token) |> Base.encode16(case: :lower)

  defp hash(value),
    do: :crypto.hash(:sha256, :erlang.term_to_binary(value)) |> Base.encode16(case: :lower)

  defp mcp_error(code, message, details \\ %{}), do: {:error, code, message, details}
  defp public_identity(identity), do: Map.drop(identity, [:token_hash])

  defp public_session(session),
    do: %{
      id: session.session.id,
      identity: public_identity(session.identity),
      required: required(%{session: session})
    }

  defp public_artifact(artifact),
    do: %{
      id: artifact.id,
      kind: artifact.kind,
      relative_path: artifact.relative_path,
      sha256: artifact.sha256,
      size: artifact.byte_size,
      mime: artifact.mime_type
    }

  defp optional(fun, arg) do
    case fun.(arg) do
      {:ok, value} -> value
      _ -> nil
    end
  end

  defp safe_close(handle) do
    AgentBackend.close_session(handle)
  catch
    :exit, _ -> :ok
  end

  defp stringify_keys(map) when is_map(map),
    do: Map.new(map, fn {key, value} -> {to_string(key), stringify_keys(value)} end)

  defp stringify_keys(list) when is_list(list), do: Enum.map(list, &stringify_keys/1)
  defp stringify_keys(value), do: value

  defp call(server, message) do
    case GenServer.whereis(server) do
      nil -> {:error, :not_started}
      _ -> GenServer.call(server, message, 120_000)
    end
  end
end
