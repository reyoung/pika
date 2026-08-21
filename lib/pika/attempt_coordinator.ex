defmodule Pika.AttemptCoordinator do
  @moduledoc false

  use GenServer
  require Logger

  alias Pika.AgentBackend
  alias Pika.AgentBackend.JSONLWriter
  alias Pika.AttemptTokenRegistry, as: TokenRegistry
  alias Pika.AttemptPrompt, as: Prompt
  alias Pika.AttemptStore, as: Store
  alias Pika.AttemptWorkspace, as: Workspace
  alias Pika.Alignment.ArtifactStore, as: StageArtifactStore

  @recovery_retry_ms 250
  @progress_log_interval_ms 30_000
  @recovery_tail_records 50
  @recovery_line_max_bytes 262_144
  @recovery_data_max_bytes 4_000
  @read_tools ~w(get_context query_attempt_history get_attempt list_agents read_agent_messages)
  @write_tools ~w(ack_agent_messages send_agent_message register_artifact submit_plan record_metrics submit_attempt_summary reject_attempt complete_attempt)

  def start_link(opts \\ []) do
    GenServer.start_link(__MODULE__, opts, name: Keyword.get(opts, :name, __MODULE__))
  end

  def snapshot(server \\ __MODULE__), do: call(server, :snapshot)
  def dispatch(server \\ __MODULE__), do: call(server, :dispatch)
  def stop_now(server \\ __MODULE__), do: call(server, :stop_now)
  def resume(server \\ __MODULE__), do: call(server, :resume)
  def progress_topic(campaign_id), do: "pika:attempt-progress:#{campaign_id}"
  def authorize(token, _server \\ __MODULE__), do: TokenRegistry.authorize(token)
  def read_plan(token, server \\ __MODULE__), do: call(server, {:read_plan, token})

  def mcp_call(token, tool, args, server \\ __MODULE__),
    do: call(server, {:mcp, token, tool, args})

  def create_btw(attempt_id, body, mode, idempotency_key, server \\ __MODULE__),
    do: call(server, {:create_btw, attempt_id, body, mode, idempotency_key})

  @impl true
  def init(opts) do
    :ok = TokenRegistry.init_owner()
    workspace = Keyword.get_lazy(opts, :workspace, &Pika.WorkspaceLock.workspace/0)
    campaign = Keyword.get_lazy(opts, :campaign, &Pika.Persistence.current_campaign/0)
    campaign_id = Keyword.get(opts, :campaign_id, campaign.id)
    profiles = Keyword.get(opts, :profiles, profiles(workspace))

    if Process.whereis(Pika.PubSub) do
      Phoenix.PubSub.subscribe(Pika.PubSub, Pika.Persistence.topic(campaign_id))
      Phoenix.PubSub.subscribe(Pika.PubSub, Pika.Alignment.Campaign.topic())
      Phoenix.PubSub.subscribe(Pika.PubSub, "pika:optimization:events")
    end

    state = %{
      workspace: workspace,
      campaign_id: campaign_id,
      profiles: profiles,
      reload_profiles: not Keyword.has_key?(opts, :profiles),
      backend_modules: Keyword.get(opts, :backend_modules, %{}),
      mcp_url: Keyword.get(opts, :mcp_url, mcp_url(workspace)),
      start_backends: Keyword.get(opts, :start_backends, true),
      auto_dispatch: Keyword.get(opts, :auto_dispatch, true),
      recovery_enabled: campaign.status not in ~w(stopped blocked completed),
      max_unverified_attempts:
        Keyword.get(opts, :max_unverified_attempts, max_unverified_attempts(workspace)),
      sessions: %{},
      tokens: %{},
      monitors: %{},
      recovery_count: %{},
      last_error: nil,
      progress_log_interval_ms:
        Keyword.get(opts, :progress_log_interval_ms, @progress_log_interval_ms),
      progress_log_level: Keyword.get(opts, :progress_log_level, :info),
      progress_log_task: nil,
      progress_probe: Keyword.get(opts, :progress_probe, &attempt_progress/2)
    }

    send(self(), :recover)
    schedule_progress_log(state.progress_log_interval_ms)
    {:ok, state}
  end

  @impl true
  def handle_call(:snapshot, _from, state), do: {:reply, public_snapshot(state), state}

  def handle_call(:dispatch, _from, state) do
    state = dispatch_available(state)
    {:reply, :ok, state}
  end

  def handle_call(:stop_now, _from, state),
    do: {:reply, :ok, stop_sessions(%{state | recovery_enabled: false})}

  def handle_call(:resume, _from, state) do
    send(self(), :recover)
    {:reply, :ok, %{state | recovery_enabled: true}}
  end

  def handle_call({:read_plan, token}, _from, state) do
    result =
      with {:ok, session_id} <- Map.fetch(state.tokens, token_hash(token)),
           session <- Map.fetch!(state.sessions, session_id),
           attempt_id <- session.identity.attempt_id,
           {:ok, attempt} <- Store.attempt(attempt_id),
           true <- not is_nil(attempt.plan_artifact_id),
           relative_path <- "artifacts/plans/#{attempt_id}/plan.md",
           {:ok, artifact} <- Store.artifact(state.campaign_id, relative_path),
           true <- artifact.id == attempt.plan_artifact_id,
           {:ok, path} <- Pika.ArtifactStore.resolve(state.workspace, relative_path),
           {:ok, body} <- File.read(path) do
        {:ok, %{attempt_id: attempt_id, text: body}}
      else
        :error -> {:error, :unauthorized}
        false -> {:error, :plan_not_found}
        {:error, _reason} = error -> error
      end

    {:reply, result, state}
  end

  def handle_call({:mcp, token, tool, args}, _from, state) do
    case Map.fetch(state.tokens, token_hash(token)) do
      :error ->
        {:reply, mcp_error("unauthorized", "invalid Backend Session token"), state}

      {:ok, session_id} ->
        session_state = Map.fetch!(state.sessions, session_id)
        execute_mcp(tool, stringify_keys(args), session_state, state)
    end
  end

  def handle_call({:create_btw, attempt_id, body, mode, key}, _from, state) do
    body = String.trim(body || "")

    result =
      cond do
        body == "" ->
          {:error, :empty_message}

        mode not in ~w(chat current future) ->
          {:error, :invalid_btw_mode}

        not is_binary(key) or key == "" ->
          {:error, :idempotency_key_required}

        true ->
          create_btw_idempotently(state, attempt_id, body, mode, key)
      end

    case result do
      {:new, {:ok, guidance}} ->
        {:reply, {:ok, guidance}, maybe_steer_guidance(state, guidance)}

      {:new, error} ->
        {:reply, error, state}

      {:replay, response} ->
        {:reply, response, state}

      error ->
        {:reply, error, state}
    end
  end

  @impl true
  def handle_info(:recover, state) do
    state = if(state.recovery_enabled, do: recover_attempts(state), else: state)
    {:noreply, if(state.auto_dispatch, do: dispatch_available(state), else: state)}
  end

  def handle_info({:domain_event, _event}, state),
    do: {:noreply, if(state.auto_dispatch, do: dispatch_available(state), else: state)}

  def handle_info({:integration_event, %{event_type: type} = event}, state)
      when type in ["best_advanced", "sampling_advanced"] do
    state = steer_campaign_event(state, event)
    {:noreply, if(state.auto_dispatch, do: dispatch_available(state), else: state)}
  end

  def handle_info({:integration_event, _event}, state), do: {:noreply, state}

  def handle_info({:campaign_updated, %{status: :optimizing}}, state),
    do: {:noreply, if(state.auto_dispatch, do: dispatch_available(state), else: state)}

  def handle_info({:campaign_updated, _snapshot}, state), do: {:noreply, state}

  def handle_info({:pika_backend_event, event}, state) do
    case Map.fetch(state.sessions, event.session_id) do
      :error ->
        {:noreply, state}

      {:ok, session_state} ->
        session_state = note_backend_event(session_state, event)
        state = put_session(state, event.session_id, session_state)
        state = persist_backend_event(state, session_state, event)
        state = apply_backend_event(state, session_state, event)
        {:noreply, state}
    end
  end

  def handle_info(:log_work_snapshot, state) do
    schedule_progress_log(state.progress_log_interval_ms)
    {:noreply, start_progress_log(state)}
  end

  def handle_info(
        {:DOWN, monitor, :process, _pid, _reason},
        %{progress_log_task: %{monitor: monitor}} = state
      ),
      do: {:noreply, %{state | progress_log_task: nil}}

  def handle_info({:DOWN, monitor, :process, _pid, reason}, state) do
    case Map.pop(state.monitors, monitor) do
      {nil, monitors} ->
        {:noreply, %{state | monitors: monitors}}

      {session_id, monitors} ->
        state = %{state | monitors: monitors}

        case Map.get(state.sessions, session_id) do
          nil ->
            {:noreply, state}

          %{closing: true} ->
            {:noreply, remove_session(state, session_id)}

          session_state ->
            {:noreply, recover_session(state, session_state, {:process_down, reason})}
        end
    end
  end

  def handle_info({:recover_session, attempt_id, role, missing}, state) do
    if state.recovery_enabled do
      case Store.mark_running(attempt_id) do
        {:ok, attempt} ->
          state = open_attempt_session(state, attempt, role, missing, true)
          {:noreply, state}

        {:error, reason} ->
          {:noreply, %{state | last_error: inspect(reason)}}
      end
    else
      {:noreply, state}
    end
  end

  def handle_info(:dispatch_next, state),
    do: {:noreply, if(state.auto_dispatch, do: dispatch_available(state), else: state)}

  def handle_info(_message, state), do: {:noreply, state}

  @impl true
  def terminate(_reason, state) do
    stop_progress_log(state.progress_log_task)

    Enum.each(state.sessions, fn {_id, session_state} ->
      TokenRegistry.delete_hash(session_state.identity.token_hash)
      safe_close(session_state.handle)
    end)

    :ok
  end

  defp dispatch_available(state) do
    case Store.dispatch_state(state.campaign_id) do
      {:ok, %{status: "optimizing", dispatch_gate: gate}} when gate in [nil, ""] ->
        Enum.reduce(0..(length(state.profiles) - 1), state, &dispatch_slot/2)

      _ ->
        state
    end
  end

  defp stop_sessions(state) do
    Enum.each(state.sessions, fn {_session_id, session_state} ->
      TokenRegistry.delete_hash(session_state.identity.token_hash)
      _ = AgentBackend.interrupt(session_state.handle)

      _ =
        Store.update_session(
          session_state.session.id,
          "stopped",
          MapSet.to_list(session_state.required)
        )

      _ = Store.mark_interrupted(session_state.identity.attempt_id, :stop_now)
      safe_close(session_state.handle)
    end)

    Enum.each(Map.keys(state.monitors), &Process.demonitor(&1, [:flush]))
    %{state | sessions: %{}, tokens: %{}, monitors: %{}}
  end

  defp dispatch_slot(slot_index, state) do
    with {:ok, unverified_count} <- Store.unverified_attempt_count(state.campaign_id),
         true <- unverified_dispatch_allowed?(unverified_count, state.max_unverified_attempts) do
      occupied? =
        Store.active_attempts(state.campaign_id)
        |> Enum.any?(&(&1.slot_index == slot_index and &1.status != "ready_for_integration"))

      if occupied? do
        state
      else
        case Store.create_attempt(state.campaign_id, slot_index) do
          {:ok, attempt} -> prepare_attempt(state, attempt)
          {:error, :attempt_budget_exhausted} -> state
          {:error, {:attempt_budget_exhausted, _}} -> state
          {:error, {:slot_busy, _}} -> state
          {:error, reason} -> %{state | last_error: inspect(reason)}
        end
      end
    else
      false -> state
      {:error, reason} -> %{state | last_error: inspect(reason)}
    end
  end

  defp prepare_attempt(state, attempt) do
    with {:ok, context} <- Store.campaign_context(state.campaign_id),
         {:ok, _worktree} <-
           Workspace.create(
             state.workspace,
             attempt.id,
             attempt.base_sha,
             context.references,
             context.target_snapshot
           ),
         {:ok, attempt} <- Store.mark_running(attempt.id) do
      role = if context.plan_enabled, do: :plan, else: :iteration

      if state.start_backends,
        do: open_attempt_session(state, attempt, role, required_for(role), false),
        else: state
    else
      {:error, reason} ->
        _ = Store.mark_interrupted(attempt.id, reason)
        %{state | last_error: inspect(reason)}
    end
  end

  defp recover_attempts(state) do
    Store.active_attempts(state.campaign_id)
    |> Enum.reduce(state, fn attempt, acc ->
      _ = Store.interrupt_active_sessions_for_attempt(attempt.id)

      cond do
        integration_owned_attempt?(attempt) ->
          acc

        Enum.any?(acc.sessions, fn {_id, session} -> session.identity.attempt_id == attempt.id end) ->
          acc

        not acc.start_backends ->
          acc

        true ->
          plan_enabled =
            case Store.campaign_context(state.campaign_id) do
              {:ok, context} -> context.plan_enabled
              _ -> false
            end

          role = recovery_role(attempt, plan_enabled)
          missing = recovery_required(attempt, role)
          _ = Store.mark_interrupted(attempt.id, :server_recovery)

          case Store.mark_running(attempt.id) do
            {:ok, running} -> open_attempt_session(acc, running, role, missing, true)
            {:error, reason} -> %{acc | last_error: inspect(reason)}
          end
      end
    end)
  end

  defp open_attempt_session(state, attempt, role, required, recovering?) do
    state = reload_profiles(state)
    profile = Enum.at(state.profiles, attempt.slot_index) || List.first(state.profiles)
    backend = backend_atom(profile["backend"] || profile[:backend])
    module = Map.get(state.backend_modules, backend, backend_module(backend))
    token = random_token()
    token_hash = token_hash(token)
    effort = profile["reasoning_effort"] || profile[:reasoning_effort] || "high"
    {command, args} = backend_command(backend, profile["command"] || profile[:command])

    backend_profile = %{
      backend: backend,
      command: command,
      args: args,
      approval_policy: profile["approval_policy"] || profile[:approval_policy],
      sandbox_policy: profile["sandbox_policy"] || profile[:sandbox_policy],
      env: profile["env"] || profile[:env] || %{},
      protocol_config: profile["protocol_config"] || profile[:protocol_config] || %{},
      artifact_dir: Path.join([state.workspace.artifacts, "logs", attempt.id])
    }

    registry_identity = %{
      campaign_id: state.campaign_id,
      attempt_id: attempt.id,
      role: role,
      slot_index: attempt.slot_index
    }

    with :ok <- Pika.PromptCatalog.validate([role]),
         {:ok, context} <- Store.campaign_context(state.campaign_id),
         {:ok, instructions} <- Prompt.render(role, attempt, context),
         :ok <- TokenRegistry.put(token, registry_identity),
         {:ok, handle} <- AgentBackend.start_link(module, backend_profile, self()) do
      log_session_opening(state, attempt, role, required, recovering?)

      open_result =
        AgentBackend.open_session(
          handle,
          Path.join(state.workspace.root, attempt.worktree_relative_path),
          profile["model"] || profile[:model],
          effort,
          %{
            url: state.mcp_url,
            token: token,
            role: role,
            attempt_id: attempt.id,
            coordinator: self()
          },
          skill_roots(state.workspace),
          recovery_instructions(
            instructions,
            recovering?,
            required,
            state.workspace,
            attempt
          )
        )

      case open_result do
        {:ok, session} ->
          establish_attempt_session(
            state,
            attempt,
            role,
            required,
            recovering?,
            handle,
            session,
            backend_profile,
            token,
            token_hash
          )

        {:error, reason} ->
          safe_close(handle)
          session_open_failed(state, attempt, role, required, token, reason)
      end
    else
      {:error, reason} ->
        session_open_failed(state, attempt, role, required, token, reason)
    end
  end

  defp reload_profiles(%{reload_profiles: false} = state), do: state

  defp reload_profiles(state) do
    case Pika.RuntimeConfig.mutable(state.workspace) do
      {:ok, mutable} ->
        profiles = mutable["iteration_agents"]

        if is_list(profiles) and profiles != [],
          do: %{state | profiles: profiles, last_error: nil},
          else: state

      {:error, reason} ->
        %{state | last_error: inspect(reason)}
    end
  end

  defp establish_attempt_session(
         state,
         attempt,
         role,
         required,
         recovering?,
         handle,
         session,
         backend_profile,
         token,
         token_hash
       ) do
    identity = %{
      session_id: session.id,
      campaign_id: state.campaign_id,
      attempt_id: attempt.id,
      role: role,
      slot_index: attempt.slot_index,
      token_hash: token_hash
    }

    :ok =
      TokenRegistry.put(token, %{
        campaign_id: state.campaign_id,
        attempt_id: attempt.id,
        role: role,
        slot_index: attempt.slot_index,
        session_id: session.id
      })

    :ok = Store.insert_session(state.campaign_id, identity, session, backend_profile, required)
    monitor = Process.monitor(handle.pid)

    session_state = %{
      handle: handle,
      session: session,
      identity: identity,
      required: MapSet.new(required),
      active_turn_id: nil,
      closing: false,
      last_event_type: :session_opened,
      last_event_at: DateTime.utc_now() |> DateTime.to_iso8601(),
      last_event_summary: "Backend session opened",
      log_path: "artifacts/logs/#{attempt.id}/#{session.id}.jsonl"
    }

    state = %{
      state
      | sessions: Map.put(state.sessions, session.id, session_state),
        tokens: Map.put(state.tokens, token_hash, session.id),
        monitors: Map.put(state.monitors, monitor, session.id),
        recovery_count:
          if(recovering?,
            do: Map.update(state.recovery_count, attempt.id, 1, &(&1 + 1)),
            else: state.recovery_count
          ),
        last_error: nil
    }

    state = ensure_session_log(state, session_state)
    kickoff = kickoff(role, attempt, recovering?, required)

    case AgentBackend.start_turn(handle, kickoff) do
      {:ok, turn_id} ->
        put_session(state, session.id, %{
          session_state
          | active_turn_id: turn_id,
            last_event_type: :turn_start_requested,
            last_event_at: DateTime.utc_now() |> DateTime.to_iso8601(),
            last_event_summary: kickoff
        })

      {:error, reason} ->
        recover_session(state, session_state, {:start_turn_failed, reason})
    end
  end

  defp session_open_failed(state, attempt, role, required, token, reason) do
    TokenRegistry.delete(token)
    _ = Store.mark_interrupted(attempt.id, reason)

    log_session_open_failure(state, attempt, role, required, reason)

    Process.send_after(
      self(),
      {:recover_session, attempt.id, role, required},
      @recovery_retry_ms
    )

    %{state | last_error: inspect(reason)}
  end

  defp execute_mcp(tool, args, session_state, state) when tool in @read_tools do
    {response, state} = perform_read(tool, args, session_state, state)
    {:reply, response, state}
  end

  defp execute_mcp(tool, args, session_state, state) when tool in @write_tools do
    key = args["idempotency_key"]

    if not is_binary(key) or key == "" do
      {:reply, mcp_error("missing_required_data", "idempotency_key is required"), state}
    else
      request_hash = request_hash(tool, args)
      session_id = session_state.session.id

      case Store.lookup_idempotency(session_id, tool, key, request_hash) do
        {:replay, response} ->
          {:reply, response, state}

        :conflict ->
          {:reply, mcp_error("idempotency_conflict", "same key has a different request"), state}

        :missing ->
          {response, next_state} = perform_write(tool, args, session_state, state)
          :ok = Store.store_idempotency(session_id, tool, key, request_hash, response)
          {:reply, response, next_state}
      end
    end
  end

  defp execute_mcp(tool, _args, _session_state, state),
    do: {:reply, mcp_error("forbidden_role", "tool is unavailable: #{tool}"), state}

  defp perform_read("get_context", _args, session_state, state) do
    with {:ok, context} <- Store.campaign_context(state.campaign_id),
         {:ok, attempt} <- Store.attempt(session_state.identity.attempt_id) do
      response =
        {:ok,
         %{
           identity: public_identity(session_state.identity),
           campaign: context,
           attempt: enrich_attempt(attempt),
           required_operations: session_state.required |> MapSet.to_list() |> Enum.sort(),
           guidance:
             Store.guidance_for_attempt(
               state.campaign_id,
               attempt.id,
               attempt.created_at
             )
         }}

      {response, state}
    else
      {:error, reason} -> {mcp_error("missing_required_data", inspect(reason)), state}
    end
  end

  defp perform_read("query_attempt_history", args, _session_state, state) do
    limit = args["limit"] || 100

    if is_integer(limit) and limit in 1..500 do
      attempts =
        Store.query_terminal_history(state.campaign_id,
          limit: limit,
          before_ordinal: args["before_ordinal"],
          outcome: args["outcome"]
        )
        |> Enum.map(&enrich_attempt/1)

      {{:ok, attempts}, state}
    else
      {mcp_error("missing_required_data", "limit must be 1..500"), state}
    end
  end

  defp perform_read("get_attempt", args, session_state, state) do
    id = args["attempt_id"] || session_state.identity.attempt_id

    case Store.attempt(id) do
      {:ok, attempt}
      when id == session_state.identity.attempt_id or
             attempt.status in ~w(accepted rejected cancelled) ->
        {{:ok, enrich_attempt(attempt)}, state}

      {:ok, _attempt} ->
        {mcp_error("identity_mismatch", "only own or terminal Attempts are readable"), state}

      {:error, reason} ->
        {mcp_error("missing_required_data", inspect(reason)), state}
    end
  end

  defp perform_read("list_agents", _args, _session_state, state),
    do: {{:ok, Store.sessions(state.campaign_id)}, state}

  defp perform_read("read_agent_messages", args, session_state, state) do
    case Store.read_messages(
           session_state.session.id,
           args["after_sequence"] || 0,
           args["limit"] || 100
         ) do
      {:ok, messages} -> {{:ok, messages}, state}
      {:error, reason} -> {mcp_error("missing_required_data", inspect(reason)), state}
    end
  end

  defp perform_write("ack_agent_messages", args, session_state, state) do
    case Store.ack_messages(session_state.session.id, args["through_sequence"] || 0) do
      {:ok, result} -> {{:ok, result}, state}
      {:error, reason} -> {mcp_error("missing_required_data", inspect(reason)), state}
    end
  end

  defp perform_write("send_agent_message", args, session_state, state) do
    body = String.trim(args["body"] || "")
    priority = args["priority"] || "normal"

    cond do
      body == "" ->
        {mcp_error("missing_required_data", "body is required"), state}

      priority not in ~w(normal high) ->
        {mcp_error("missing_required_data", "priority is invalid"), state}

      true ->
        case Store.send_message(
               state.campaign_id,
               session_state.session.id,
               args["target_session_id"],
               body,
               priority
             ) do
          {:ok, result} -> {{:ok, result}, state}
          {:error, reason} -> {mcp_error("identity_mismatch", inspect(reason)), state}
        end
    end
  end

  defp perform_write("register_artifact", args, session_state, state) do
    attrs = %{
      sha256: args["sha256"],
      size: args["size"],
      mime: args["mime"],
      kind: args["kind"],
      metadata: args["metadata"] || %{}
    }

    with {:ok, _verified} <-
           StageArtifactStore.register(state.workspace.root, args["relative_path"], attrs),
         {:ok, artifact} <-
           Pika.ArtifactStore.register(state.workspace, args["relative_path"], %{
             campaign_id: state.campaign_id,
             owner_type: "attempt",
             owner_id: session_state.identity.attempt_id,
             kind: args["kind"],
             mime_type: args["mime"],
             metadata: args["metadata"] || %{}
           }) do
      {{:ok, public_artifact(artifact)}, state}
    else
      {:error, reason} ->
        {mcp_error("missing_required_data", "artifact registration failed", %{reason: reason}),
         state}
    end
  end

  defp perform_write("submit_plan", args, %{identity: %{role: :plan}} = session_state, state) do
    markdown = String.trim(args["markdown"] || "")
    summary = String.trim(args["summary"] || "")

    if markdown == "" or summary == "" do
      {mcp_error("missing_required_data", "markdown and summary are required"), state}
    else
      path = "artifacts/plans/#{session_state.identity.attempt_id}/plan.md"

      case Pika.ArtifactStore.write(state.workspace, path, markdown <> "\n", %{
             campaign_id: state.campaign_id,
             owner_type: "attempt",
             owner_id: session_state.identity.attempt_id,
             kind: "plan",
             mime_type: "text/markdown",
             metadata: %{summary: summary}
           }) do
        {:ok, artifact} ->
          :ok =
            Store.attach_artifact(
              session_state.identity.attempt_id,
              "plan_artifact_id",
              artifact.id
            )

          state = complete_required(state, session_state.session.id, "submit_plan")
          {{:ok, %{artifact: public_artifact(artifact), summary: summary}}, state}

        {:error, reason} ->
          {mcp_error("missing_required_data", "plan write failed", %{reason: reason}), state}
      end
    end
  end

  defp perform_write("submit_plan", _args, _session_state, state),
    do: {mcp_error("forbidden_role", "submit_plan requires Plan role"), state}

  defp perform_write(
         "record_metrics",
         args,
         %{identity: %{role: :iteration}} = session_state,
         state
       ) do
    attempt_id = session_state.identity.attempt_id

    with {:ok, context} <- Store.campaign_context(state.campaign_id),
         {:ok, attempt} <- Store.attempt(attempt_id),
         :ok <- validate_attempt_identity(args, attempt, context),
         {:ok, worktree} <- Workspace.current(state.workspace, attempt),
         :ok <-
           Workspace.verify_candidate(
             state.workspace,
             worktree,
             args["candidate_sha"],
             %{protected_paths: context.protected_paths}
           ),
         true <- args["harness_digest"] == context.spec_revision.protected_digest,
         {:ok, samples} <- Store.artifact(state.campaign_id, args["samples_artifact"]),
         {:ok, correctness} <- Store.artifact(state.campaign_id, args["correctness_artifact"]),
         :ok <- verify_attempt_artifact(samples, attempt_id),
         :ok <- verify_attempt_artifact(correctness, attempt_id),
         {:ok, samples_path} <-
           Pika.ArtifactStore.resolve(state.workspace, samples.relative_path),
         {:ok, correctness_path} <-
           Pika.ArtifactStore.resolve(state.workspace, correctness.relative_path),
         {:ok, metrics} <-
           Pika.Measurement.evaluate_iteration(samples_path, correctness_path, %{
             base_sha: attempt.base_sha,
             candidate_sha: args["candidate_sha"],
             target_snapshot_id: context.target_snapshot.id,
             case_ids: context.sampled_case_ids,
             metrics: context.metrics,
             benchmark: context.spec["benchmark"],
             best_metrics: context.best_metrics
           }),
         {:ok, _event} <- Store.record_metrics(attempt_id, metrics, args["candidate_sha"]),
         :ok <- Store.attach_artifact(attempt_id, "metrics_artifact_id", samples.id),
         :ok <- Store.attach_artifact(attempt_id, "correctness_artifact_id", correctness.id) do
      state = complete_required(state, session_state.session.id, "record_metrics")
      {{:ok, %{metrics: metrics, source: "iteration"}}, state}
    else
      false ->
        {mcp_error("protected_path_changed", "Harness digest does not match the frozen Spec"),
         state}

      {:error, {:stale_best, current}} ->
        {mcp_error("stale_best", "Attempt base is stale", %{current_best_sha: current}), state}

      {:error, {:protected_paths_changed, paths}} ->
        {mcp_error("protected_path_changed", "candidate changed protected Harness", %{
           paths: paths
         }), state}

      {:error, reason} ->
        {mcp_error("missing_required_data", "formal metrics rejected", %{reason: reason}), state}
    end
  end

  defp perform_write("record_metrics", _args, _session_state, state),
    do: {mcp_error("forbidden_role", "record_metrics requires Iteration role"), state}

  defp perform_write(
         "submit_attempt_summary",
         args,
         %{identity: %{role: :iteration}} = session_state,
         state
       ) do
    attrs = %{
      description: String.trim(args["description"] || ""),
      summary: String.trim(args["summary"] || ""),
      modification_scope: List.wrap(args["modification_scope"]),
      risks: List.wrap(args["risks"]),
      profiler_summary: args["profiler_summary"],
      recommended_outcome: args["recommended_outcome"]
    }

    if attrs.description == "" or attrs.summary == "" do
      {mcp_error("missing_required_data", "description and summary are required"), state}
    else
      case Store.submit_summary(session_state.identity.attempt_id, attrs) do
        {:ok, _event} ->
          state = complete_required(state, session_state.session.id, "submit_attempt_summary")
          {{:ok, attrs}, state}

        {:error, reason} ->
          {mcp_error("missing_required_data", inspect(reason)), state}
      end
    end
  end

  defp perform_write("submit_attempt_summary", _args, _session_state, state),
    do: {mcp_error("forbidden_role", "submit_attempt_summary requires Iteration role"), state}

  defp perform_write(
         "reject_attempt",
         args,
         %{identity: %{role: :iteration}} = session_state,
         state
       ) do
    reason = String.trim(args["reason"] || "")

    if reason == "" do
      {mcp_error("missing_required_data", "rejection reason is required"), state}
    else
      case Store.reject_from_iteration(session_state.identity.attempt_id, reason) do
        {:ok, rejected} ->
          session_state = %{session_state | required: MapSet.new()}
          _ = Store.update_session(session_state.session.id, "running", [])

          {{:ok, enrich_attempt(rejected)},
           put_session(state, session_state.session.id, session_state)}

        {:error, reason} ->
          {mcp_error("missing_required_data", "Attempt rejection gate is open", %{reason: reason}),
           state}
      end
    end
  end

  defp perform_write("reject_attempt", _args, _session_state, state),
    do: {mcp_error("forbidden_role", "reject_attempt requires Iteration role"), state}

  defp perform_write(
         "complete_attempt",
         args,
         %{identity: %{role: :iteration}} = session_state,
         state
       ) do
    attempt_id = session_state.identity.attempt_id

    with {:ok, context} <- Store.campaign_context(state.campaign_id),
         {:ok, attempt} <- Store.attempt(attempt_id),
         :ok <- validate_attempt_identity(args, attempt, context),
         {:ok, worktree} <- Workspace.current(state.workspace, attempt),
         :ok <-
           Workspace.verify_candidate(
             state.workspace,
             worktree,
             args["candidate_sha"],
             %{protected_paths: context.protected_paths}
           ),
         {:ok, patch} <- Workspace.patch(state.workspace, worktree, args["candidate_sha"]),
         {:ok, artifact} <- write_patch(state, attempt, patch),
         :ok <- Store.attach_artifact(attempt_id, "patch_artifact_id", artifact.id),
         expected <- Store.expected_metric_count(attempt_id),
         {:ok, completed} <- Store.complete_attempt(attempt_id, args["candidate_sha"], expected) do
      state = complete_required(state, session_state.session.id, "complete_attempt")
      {{:ok, enrich_attempt(completed)}, state}
    else
      {:error, {:stale_best, current}} ->
        {mcp_error("stale_best", "Attempt base is stale", %{current_best_sha: current}), state}

      {:error, {:protected_paths_changed, paths}} ->
        {mcp_error("protected_path_changed", "candidate changed protected Harness", %{
           paths: paths
         }), state}

      {:error, reason} ->
        {mcp_error("missing_required_data", "Attempt completion gate is open", %{reason: reason}),
         state}
    end
  end

  defp perform_write("complete_attempt", _args, _session_state, state),
    do: {mcp_error("forbidden_role", "complete_attempt requires Iteration role"), state}

  defp apply_backend_event(state, session_state, %{type: :turn_started} = event) do
    session_state = %{session_state | active_turn_id: event.turn_id}

    _ =
      Store.update_session(
        session_state.session.id,
        "running",
        MapSet.to_list(session_state.required),
        %{turn_increment: 1}
      )

    put_session(state, session_state.session.id, session_state)
  end

  defp apply_backend_event(state, session_state, %{type: :turn_completed}) do
    session_state = %{session_state | active_turn_id: nil}
    state = put_session(state, session_state.session.id, session_state)

    if MapSet.size(session_state.required) == 0 do
      finish_role(state, session_state)
    else
      missing = session_state.required |> MapSet.to_list() |> Enum.sort()
      _ = Store.mark_awaiting_report(session_state.identity.attempt_id, missing)
      _ = Store.update_session(session_state.session.id, "awaiting_report", missing)

      case AgentBackend.start_turn(
             session_state.handle,
             "Pika completion gate remains open. Complete these MCP operations: #{Enum.join(missing, ", ")}."
           ) do
        {:ok, turn_id} ->
          put_session(state, session_state.session.id, %{session_state | active_turn_id: turn_id})

        {:error, reason} ->
          recover_session(state, session_state, {:followup_failed, reason})
      end
    end
  end

  defp apply_backend_event(state, session_state, %{type: type, data: data})
       when type in [:backend_error, :process_exited] do
    if session_state.closing,
      do: remove_session(state, session_state.session.id),
      else: recover_session(state, session_state, {type, data})
  end

  defp apply_backend_event(state, session_state, _event) do
    _ =
      Store.update_session(
        session_state.session.id,
        "running",
        MapSet.to_list(session_state.required),
        %{event_increment: 1}
      )

    state
  end

  defp finish_role(state, %{identity: %{role: :plan}} = session_state) do
    attempt_id = session_state.identity.attempt_id
    _ = Store.update_session(session_state.session.id, "completed", [])
    state = close_and_remove(state, session_state)

    case Store.attempt(attempt_id) do
      {:ok, attempt} ->
        open_attempt_session(state, attempt, :iteration, required_for(:iteration), false)

      {:error, reason} ->
        %{state | last_error: inspect(reason)}
    end
  end

  defp finish_role(state, %{identity: %{role: :iteration}} = session_state) do
    _ = Store.update_session(session_state.session.id, "completed", [])
    state = close_and_remove(state, session_state)
    send(self(), :dispatch_next)
    state
  end

  defp recover_session(state, session_state, reason) do
    attempt_id = session_state.identity.attempt_id
    role = session_state.identity.role
    missing = MapSet.to_list(session_state.required)
    _ = Store.update_session(session_state.session.id, "interrupted", missing)
    _ = Store.mark_interrupted(attempt_id, reason)
    state = close_and_remove(state, session_state)
    send(self(), {:recover_session, attempt_id, role, missing})
    state
  end

  defp close_and_remove(state, session_state) do
    state =
      put_session(state, session_state.session.id, %{session_state | closing: true})

    safe_close(session_state.handle)
    remove_session(state, session_state.session.id)
  end

  defp remove_session(state, session_id) do
    case Map.pop(state.sessions, session_id) do
      {nil, sessions} ->
        %{state | sessions: sessions}

      {session_state, sessions} ->
        TokenRegistry.delete_hash(session_state.identity.token_hash)

        monitors =
          Enum.reduce(state.monitors, state.monitors, fn
            {monitor, ^session_id}, acc ->
              Process.demonitor(monitor, [:flush])
              Map.delete(acc, monitor)

            _, acc ->
              acc
          end)

        %{
          state
          | sessions: sessions,
            tokens: Map.delete(state.tokens, session_state.identity.token_hash),
            monitors: monitors
        }
    end
  end

  defp complete_required(state, session_id, operation) do
    session_state = Map.fetch!(state.sessions, session_id)
    required = MapSet.delete(session_state.required, operation)
    _ = Store.update_session(session_id, "running", MapSet.to_list(required))
    put_session(state, session_id, %{session_state | required: required})
  end

  defp put_session(state, session_id, session_state),
    do: %{state | sessions: Map.put(state.sessions, session_id, session_state)}

  defp note_backend_event(session_state, event) do
    %{
      session_state
      | last_event_type: event.type,
        last_event_at: DateTime.utc_now() |> DateTime.to_iso8601(),
        last_event_summary: backend_event_summary(event)
    }
  end

  defp backend_event_summary(event) do
    data =
      event.data
      |> then(&(&1 || %{}))
      |> JSONLWriter.redact()
      |> Pika.JSONSafe.json_safe()

    value =
      first_progress_value(data) ||
        if(data == %{}, do: to_string(event.type), else: Jason.encode!(data))

    value
    |> to_string()
    |> String.slice(0, 500)
  rescue
    _error -> to_string(event.type)
  end

  defp first_progress_value(data) when is_map(data) do
    Enum.find_value(
      ~w(delta text content output title command path diff message error summary),
      fn key ->
        case Map.get(data, key) do
          value when value in [nil, "", []] -> nil
          value when is_binary(value) -> value
          value -> Jason.encode!(Pika.JSONSafe.json_safe(value))
        end
      end
    ) ||
      case Map.get(data, "item") do
        item when is_map(item) -> first_progress_value(item)
        _other -> nil
      end
  end

  defp first_progress_value(_data), do: nil

  defp schedule_progress_log(interval) when is_integer(interval) and interval > 0,
    do: Process.send_after(self(), :log_work_snapshot, interval)

  defp schedule_progress_log(_interval), do: :ok

  defp start_progress_log(%{progress_log_task: nil} = state) do
    input = %{
      campaign_id: state.campaign_id,
      workspace: state.workspace,
      sessions: Enum.map(state.sessions, fn {_id, session} -> session_progress(session) end),
      recovery_count: state.recovery_count,
      last_error: state.last_error,
      level: state.progress_log_level,
      probe: state.progress_probe
    }

    case Task.start(fn -> log_work_snapshot(input) end) do
      {:ok, pid} ->
        %{state | progress_log_task: %{pid: pid, monitor: Process.monitor(pid)}}

      {:error, reason} ->
        %{state | last_error: inspect(reason)}
    end
  end

  defp start_progress_log(state), do: state

  defp log_work_snapshot(input) do
    attempts =
      input.campaign_id
      |> Store.active_attempts()
      |> Enum.reject(&(&1.status == "ready_for_integration"))

    if attempts != [] or input.sessions != [] or not is_nil(input.last_error) do
      snapshot = %{
        campaign_id: input.campaign_id,
        attempts: Enum.map(attempts, &input.probe.(input.workspace, &1)),
        sessions: input.sessions,
        recovery_count: input.recovery_count,
        last_error: truncate(input.last_error, 1_000)
      }

      Logger.log(
        input.level,
        "Pika attempt work snapshot " <>
          (snapshot |> JSONLWriter.redact() |> Jason.encode!())
      )
    end
  rescue
    error ->
      message = error |> Exception.message() |> JSONLWriter.redact()
      Logger.warning("Pika attempt work snapshot failed: #{message}")
  end

  defp stop_progress_log(nil), do: :ok

  defp stop_progress_log(%{pid: pid, monitor: monitor}) do
    Process.demonitor(monitor, [:flush])
    if Process.alive?(pid), do: Process.exit(pid, :shutdown)
    :ok
  end

  defp attempt_progress(workspace, attempt) do
    %{
      attempt_id: attempt.id,
      ordinal: attempt.ordinal,
      slot_index: attempt.slot_index,
      status: attempt.status,
      current_work: attempt.summary || attempt.description || "awaiting_agent_summary",
      base_sha: short_sha(attempt.base_sha),
      candidate_sha: short_sha(attempt.candidate_sha),
      worktree: attempt.worktree_relative_path,
      git: worktree_git_progress(workspace, attempt)
    }
  end

  defp session_progress(session) do
    %{
      session_id: session.session.id,
      attempt_id: session.identity.attempt_id,
      role: session.identity.role,
      backend: session.session.backend,
      provider_session_id: session.session.backend_session_id,
      process_alive: Process.alive?(session.handle.pid),
      active_turn_id: session.active_turn_id,
      required_operations: session.required |> MapSet.to_list() |> Enum.sort(),
      last_event: session.last_event_type,
      last_event_at: session.last_event_at,
      last_event_summary: session.last_event_summary
    }
  end

  defp worktree_git_progress(workspace, attempt) do
    path = Path.join(workspace.root, attempt.worktree_relative_path)

    with true <- File.dir?(path),
         {:ok, head} <- Pika.Git.run(path, ["rev-parse", "HEAD"]),
         {:ok, status} <-
           Pika.Git.run(path, ["status", "--porcelain=v1", "--untracked-files=normal"]) do
      changes = String.split(status, "\n", trim: true)

      %{
        head: short_sha(head),
        dirty: changes != [],
        changed_path_count: length(changes),
        changed_paths:
          changes
          |> Enum.take(8)
          |> Enum.map(&String.slice(&1, 0, 160)),
        changed_paths_truncated: length(changes) > 8
      }
    else
      false -> %{available: false, reason: "worktree_missing"}
      {:error, reason} -> %{available: false, reason: truncate(inspect(reason), 500)}
    end
  end

  defp log_session_opening(state, attempt, role, required, recovering?) do
    Logger.info(
      "Pika backend session opening " <>
        Jason.encode!(%{
          campaign_id: state.campaign_id,
          attempt_id: attempt.id,
          ordinal: attempt.ordinal,
          slot_index: attempt.slot_index,
          role: role,
          recovery: recovering?,
          required_operations: Enum.sort(required),
          worktree: attempt.worktree_relative_path
        })
    )
  end

  defp log_session_open_failure(state, attempt, role, required, reason) do
    Logger.warning(
      "Pika backend session open failed " <>
        Jason.encode!(%{
          campaign_id: state.campaign_id,
          attempt_id: attempt.id,
          ordinal: attempt.ordinal,
          slot_index: attempt.slot_index,
          role: role,
          required_operations: Enum.sort(required),
          retry_in_ms: @recovery_retry_ms,
          reason: reason |> inspect() |> JSONLWriter.redact() |> truncate(2_000)
        })
    )
  end

  defp short_sha(nil), do: nil
  defp short_sha(sha), do: String.slice(sha, 0, 12)
  defp truncate(nil, _max), do: nil
  defp truncate(value, max), do: value |> to_string() |> String.slice(0, max)

  defp validate_attempt_identity(args, attempt, context) do
    cond do
      args["sampling_revision_id"] != attempt.sampling_revision_id ->
        {:error, :sampling_revision_mismatch}

      args["base_sha"] != attempt.base_sha ->
        {:error, :base_sha_mismatch}

      attempt.base_sha != context.best_sha ->
        {:error, {:stale_best, context.best_sha}}

      not valid_sha?(args["candidate_sha"]) ->
        {:error, :invalid_candidate_sha}

      true ->
        :ok
    end
  end

  defp verify_attempt_artifact(%{owner_type: "attempt", owner_id: attempt_id}, attempt_id),
    do: :ok

  defp verify_attempt_artifact(_artifact, _attempt_id), do: {:error, :artifact_identity_mismatch}

  defp write_patch(state, attempt, patch) do
    Pika.ArtifactStore.write(
      state.workspace,
      "artifacts/patches/#{attempt.id}/candidate.patch",
      patch,
      %{
        campaign_id: state.campaign_id,
        owner_type: "attempt",
        owner_id: attempt.id,
        kind: "patch",
        mime_type: "text/x-diff",
        metadata: %{base_sha: attempt.base_sha, candidate_sha: attempt.candidate_sha}
      }
    )
  end

  defp create_btw_idempotently(state, attempt_id, body, mode, key) do
    case attempt_session(state, attempt_id) do
      nil ->
        {:error, :btw_requires_running_attempt}

      session ->
        hash = request_hash("create_btw", %{attempt_id: attempt_id, body: body, mode: mode})

        case Store.lookup_idempotency(session.session.id, "create_btw", key, hash) do
          {:replay, response} ->
            {:replay, response}

          :conflict ->
            {:error, :idempotency_conflict}

          :missing ->
            kind = %{"chat" => "side", "current" => "attempt", "future" => "campaign"}[mode]

            response =
              Store.create_guidance(
                state.campaign_id,
                attempt_id,
                session.session.id,
                kind,
                body
              )

            :ok =
              Store.store_idempotency(session.session.id, "create_btw", key, hash, response)

            {:new, response}
        end
    end
  end

  defp attempt_session(state, attempt_id) do
    Enum.find_value(state.sessions, fn {_id, session} ->
      if session.identity.attempt_id == attempt_id and session.identity.role == :iteration,
        do: session
    end)
  end

  defp maybe_steer_guidance(
         state,
         %{kind: "attempt", attempt_id: attempt_id, body: body} = guidance
       ) do
    case Enum.find(state.sessions, fn {_id, session} ->
           session.identity.attempt_id == attempt_id and session.identity.role == :iteration
         end) do
      {_id, session} ->
        case AgentBackend.steer(session.handle, body) do
          {:ok, _turn_id} -> Store.mark_guidance_injected(guidance.id)
          _ -> :ok
        end

        state

      nil ->
        state
    end
  end

  defp maybe_steer_guidance(state, _guidance), do: state

  defp steer_campaign_event(state, event) do
    message =
      "Pika #{event.event_type}: " <>
        Jason.encode!(Pika.JSONSafe.json_safe(event.payload)) <>
        ". Keep the current Attempt's fixed Best and Sampling Revision; acknowledge via Mailbox when convenient."

    Enum.each(state.sessions, fn {_id, session} ->
      _ = AgentBackend.steer(session.handle, message)
    end)

    state
  end

  defp persist_backend_event(state, session_state, event) do
    attrs = %{
      campaign_id: state.campaign_id,
      owner_type: "attempt",
      owner_id: session_state.identity.attempt_id,
      kind: "agent_jsonl",
      metadata: %{
        session_id: session_state.session.id,
        role: session_state.identity.role
      }
    }

    record = %{
      at: DateTime.utc_now() |> DateTime.to_iso8601(),
      session_id: event.session_id,
      turn_id: event.turn_id,
      type: event.type,
      backend: event.backend,
      role: session_state.identity.role,
      data: Pika.JSONSafe.json_safe(event.data)
    }

    case Pika.ArtifactStore.append_jsonl(state.workspace, session_state.log_path, record, attrs) do
      {:ok, artifact} ->
        _ = Store.attach_session_log(session_state.session.id, artifact.id)
        broadcast_attempt_progress(state.campaign_id, session_state.identity.attempt_id)
        state

      {:error, reason} ->
        %{state | last_error: inspect(reason)}
    end
  end

  defp ensure_session_log(state, session_state) do
    attrs = %{
      campaign_id: state.campaign_id,
      owner_type: "attempt",
      owner_id: session_state.identity.attempt_id,
      kind: "agent_jsonl",
      metadata: %{session_id: session_state.session.id, role: session_state.identity.role}
    }

    record = %{
      at: DateTime.utc_now() |> DateTime.to_iso8601(),
      session_id: session_state.session.id,
      type: "session_opened",
      role: session_state.identity.role
    }

    case Pika.ArtifactStore.append_jsonl(state.workspace, session_state.log_path, record, attrs) do
      {:ok, artifact} ->
        _ = Store.attach_session_log(session_state.session.id, artifact.id)
        state

      {:error, reason} ->
        %{state | last_error: inspect(reason)}
    end
  end

  defp broadcast_attempt_progress(campaign_id, attempt_id) do
    if Process.whereis(Pika.PubSub) do
      Phoenix.PubSub.broadcast(
        Pika.PubSub,
        progress_topic(campaign_id),
        {:attempt_progress, attempt_id}
      )
    end

    :ok
  end

  defp public_snapshot(state) do
    context =
      case Store.campaign_context(state.campaign_id) do
        {:ok, value} -> value
        {:error, reason} -> %{campaign_id: state.campaign_id, error: inspect(reason)}
      end

    attempts = Store.attempts(state.campaign_id, limit: 500) |> Enum.map(&enrich_attempt/1)

    %{
      campaign: context,
      slots:
        state.profiles
        |> Enum.with_index()
        |> Enum.map(fn {profile, index} ->
          attempt =
            Enum.find(
              attempts,
              &(&1.slot_index == index and &1.status in active_attempt_statuses())
            )

          %{index: index, profile: profile, attempt_id: attempt && attempt.id}
        end),
      attempts: attempts,
      sessions: Store.sessions(state.campaign_id),
      events: Store.events(state.campaign_id),
      recovery_count: state.recovery_count,
      last_error: state.last_error
    }
  end

  defp enrich_attempt(attempt),
    do: Map.put(attempt, :metrics, Store.metrics_for_attempt(attempt.id))

  defp required_for(:plan), do: ["submit_plan"]
  defp required_for(:iteration), do: ~w(record_metrics submit_attempt_summary complete_attempt)

  defp recovery_role(%{plan_artifact_id: nil}, true), do: :plan
  defp recovery_role(_attempt, _plan_enabled), do: :iteration

  defp recovery_required(attempt, :plan),
    do: if(attempt.plan_artifact_id, do: [], else: required_for(:plan))

  defp recovery_required(attempt, :iteration) do
    if attempt.recommended_outcome in ~w(skip reject) and not is_nil(attempt.summary) do
      ["reject_attempt"]
    else
      []
      |> maybe_required(Store.metrics_for_attempt(attempt.id) == [], "record_metrics")
      |> maybe_required(is_nil(attempt.summary), "submit_attempt_summary")
      |> maybe_required(attempt.status != "ready_for_integration", "complete_attempt")
    end
  end

  defp maybe_required(required, true, operation), do: required ++ [operation]
  defp maybe_required(required, false, _operation), do: required

  defp recovery_instructions(instructions, false, _required, _workspace, _attempt),
    do: instructions

  defp recovery_instructions(instructions, true, required, workspace, attempt) do
    instructions <>
      "\n\nThis is a recovery Session for the same Attempt. Do not create a new Attempt. " <>
      "Continue in the existing worktree and complete only the missing operations: " <>
      Enum.join(required, ", ") <>
      ".\n\nRecovery JSONL tail (oldest to newest):\n" <>
      Jason.encode!(recovery_jsonl_tail(workspace, attempt.id), pretty: true)
  end

  defp recovery_jsonl_tail(workspace, attempt_id) do
    pattern = Path.join([workspace.root, "artifacts", "logs", attempt_id, "*.jsonl"])

    pattern
    |> Path.wildcard()
    |> Enum.sort()
    |> Enum.flat_map(fn path ->
      relative = Path.relative_to(path, workspace.root)

      path
      |> File.stream!(:line)
      |> Stream.filter(&recovery_event_line?/1)
      |> Stream.map(&Jason.decode/1)
      |> Stream.filter(fn
        {:ok, %{"type" => type}} when is_binary(type) -> true
        _other -> false
      end)
      |> Stream.map(fn {:ok, record} ->
        %{"artifact" => relative, "record" => compact_recovery_record(record)}
      end)
      |> Enum.to_list()
    end)
    |> Enum.take(-@recovery_tail_records)
  rescue
    _error -> []
  end

  defp recovery_event_line?(line) do
    byte_size(line) <= @recovery_line_max_bytes and
      not String.contains?(binary_part(line, 0, min(byte_size(line), 256)), "\"direction\":")
  end

  defp compact_recovery_record(record) do
    compact = Map.take(record, ~w(at session_id turn_id type backend role))
    data = Map.get(record, "data")

    if data in [nil, %{}, "", []] do
      compact
    else
      Map.put(compact, "data", compact_recovery_data(data))
    end
  end

  defp compact_recovery_data(data) do
    encoded = data |> Pika.JSONSafe.json_safe() |> Jason.encode!()

    if byte_size(encoded) <= @recovery_data_max_bytes do
      data
    else
      %{
        "truncated" => true,
        "preview" => String.slice(encoded, 0, @recovery_data_max_bytes),
        "original_bytes" => byte_size(encoded)
      }
    end
  end

  defp kickoff(:plan, attempt, false, _required),
    do: "Prepare the focused optimization plan for Attempt ##{attempt.ordinal}."

  defp kickoff(:iteration, attempt, false, _required),
    do: "Run Attempt ##{attempt.ordinal} from its fixed Best and Sampling Revision."

  defp kickoff(role, attempt, true, required),
    do:
      "Recover #{role} work for the existing Attempt ##{attempt.ordinal}; complete: #{Enum.join(required, ", ")}."

  defp profiles(workspace) do
    get_in(workspace.snapshot, ["mutable", "iteration_agents"]) || [default_profile(workspace)]
  end

  defp max_unverified_attempts(workspace) do
    get_in(workspace.snapshot, ["mutable", "max_unverified_attempts"]) || 0
  end

  # A completely empty validation queue must be able to start the first batch even
  # with the strict default of zero. After that, the configured value is the queue
  # depth tolerated before new Iteration work is paused.
  defp unverified_dispatch_allowed?(0, _limit), do: true
  defp unverified_dispatch_allowed?(count, limit), do: count < limit

  defp default_profile(workspace) do
    backend = get_in(workspace.snapshot, ["immutable", "backend"])

    %{
      "name" => "slot-1",
      "backend" => backend["type"],
      "command" => backend["command"],
      "model" => nil,
      "reasoning_effort" => "high",
      "approval_policy" => backend["approval_policy"],
      "sandbox_policy" => backend["sandbox_policy"],
      "env" => %{},
      "protocol_config" => backend["protocol_config"] || %{}
    }
  end

  defp mcp_url(workspace) do
    listen = get_in(workspace.snapshot, ["immutable", "listen"])
    host = if listen["host"] in ["0.0.0.0", "::"], do: "127.0.0.1", else: listen["host"]
    "http://#{host}:#{listen["port"]}/mcp"
  end

  defp skill_roots(workspace) do
    [Path.join(workspace.root, ".pika/skills/ncu-report-skill")]
    |> Enum.filter(&File.dir?/1)
  end

  defp backend_command(_backend, [command, "app-server", "--listen", "stdio://"]),
    do: {command, []}

  defp backend_command(_backend, [command, "acp"]), do: {command, []}
  defp backend_command(_backend, [command | args]), do: {command, args}
  defp backend_command(:cursor_acp, nil), do: {"cursor-agent", []}
  defp backend_command(_backend, nil), do: {"codex", []}

  defp backend_atom(:codex_app_server), do: :codex_app_server
  defp backend_atom(:cursor_acp), do: :cursor_acp
  defp backend_atom("cursor_acp"), do: :cursor_acp
  defp backend_atom(_value), do: :codex_app_server

  defp backend_module(:cursor_acp), do: Pika.AgentBackend.CursorACP
  defp backend_module(:codex_app_server), do: Pika.AgentBackend.CodexAppServer

  defp public_identity(identity), do: Map.drop(identity, [:token_hash])

  defp public_artifact(artifact) do
    %{
      id: artifact.id,
      kind: artifact.kind,
      relative_path: artifact.relative_path,
      sha256: artifact.sha256,
      size: artifact.byte_size,
      mime: artifact.mime_type
    }
  end

  defp request_hash(tool, args) do
    :crypto.hash(:sha256, :erlang.term_to_binary({tool, args}))
    |> Base.encode16(case: :lower)
  end

  defp random_token, do: :crypto.strong_rand_bytes(32) |> Base.url_encode64(padding: false)
  defp token_hash(token), do: :crypto.hash(:sha256, token) |> Base.encode16(case: :lower)

  defp valid_sha?(value) when is_binary(value),
    do: Regex.match?(~r/^(?:[0-9a-f]{40}|[0-9a-f]{64})$/, value)

  defp valid_sha?(_value), do: false

  defp mcp_error(code, message, details \\ %{}), do: {:error, code, message, details}

  defp safe_close(handle) do
    AgentBackend.close_session(handle)
  catch
    :exit, _reason -> :ok
  end

  defp call(server, message) do
    case GenServer.whereis(server) do
      nil -> {:error, :not_started}
      _pid -> GenServer.call(server, message, 30_000)
    end
  end

  defp stringify_keys(map) when is_map(map),
    do: Map.new(map, fn {key, value} -> {to_string(key), stringify_keys(value)} end)

  defp stringify_keys(list) when is_list(list), do: Enum.map(list, &stringify_keys/1)
  defp stringify_keys(value), do: value

  defp active_attempt_statuses,
    do: ~w(queued running awaiting_report refreshing integrating interrupted)

  # Integration owns these states. Recovering them as iteration work races the
  # IntegrationCoordinator after a server restart and rewinds the Attempt.
  defp integration_owned_attempt?(%{status: status}),
    do: status in ~w(ready_for_integration refreshing integrating)
end
