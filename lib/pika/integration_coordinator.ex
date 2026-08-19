defmodule Pika.IntegrationCoordinator do
  @moduledoc false

  use GenServer

  alias Pika.AgentBackend
  alias Pika.Alignment.ArtifactStore, as: AlignmentArtifactStore
  alias Pika.{AttemptStore, IntegrationPrompt, IntegrationStore, IntegrationWorkspace}

  @read_tools ~w(get_integration_context)
  @write_tools ~w(register_artifact acquire_integration_lease complete_refresh submit_full_regression reject_attempt create_merge_intent complete_merge)

  def start_link(opts \\ []) do
    GenServer.start_link(__MODULE__, opts, name: Keyword.get(opts, :name, __MODULE__))
  end

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
      Phoenix.PubSub.subscribe(Pika.PubSub, "pika:optimization:events")
    end

    state = %{
      workspace: workspace,
      campaign_id: campaign.id,
      profile: Keyword.get(opts, :profile, profile(workspace)),
      backend_modules: Keyword.get(opts, :backend_modules, %{}),
      mcp_url: Keyword.get(opts, :mcp_url, mcp_url(workspace)),
      start_backends: Keyword.get(opts, :start_backends, true),
      recovery_enabled: campaign.status not in ~w(stopped blocked completed),
      session: nil,
      token_hash: nil,
      monitor: nil,
      recovery_count: 0,
      last_error: nil
    }

    state = cleanup_terminal_worktrees(state)
    send(self(), :scan)
    {:ok, state}
  end

  @impl true
  def handle_call(:snapshot, _from, state) do
    {:reply,
     %{
       queue_head: optional(&IntegrationStore.queue_head/1, state.campaign_id),
       lease: optional(&IntegrationStore.lease/1, state.campaign_id),
       session: state.session && public_session(state.session),
       recovery_count: state.recovery_count,
       last_error: state.last_error
     }, state}
  end

  def handle_call(:stop_now, _from, %{session: nil} = state),
    do: {:reply, :ok, %{state | recovery_enabled: false}}

  def handle_call(:stop_now, _from, state) do
    _ = AgentBackend.interrupt(state.session.handle)
    _ = AttemptStore.update_session(state.session.session.id, "stopped", required(state))
    safe_close(state.session.handle)
    if state.monitor, do: Process.demonitor(state.monitor, [:flush])

    {:reply, :ok,
     %{
       state
       | session: nil,
         token_hash: nil,
         monitor: nil,
         recovery_enabled: false
     }}
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
      {:reply, mcp_error("unauthorized", "invalid Integration Session token"), state}
    end
  end

  @impl true
  def handle_info(:scan, %{session: nil} = state), do: {:noreply, maybe_start(state)}
  def handle_info(:scan, state), do: {:noreply, state}

  def handle_info({:domain_event, _event}, state), do: {:noreply, maybe_start(state)}
  def handle_info({:integration_event, _event}, state), do: {:noreply, maybe_start(state)}

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
    {:noreply, recover(state, {:process_down, reason})}
  end

  def handle_info({:restart, _attempt_id}, %{recovery_enabled: false} = state),
    do: {:noreply, state}

  def handle_info({:restart, attempt_id}, state) do
    case AttemptStore.attempt(attempt_id) do
      {:ok, %{status: status} = attempt} when status in ~w(accepted rejected cancelled) ->
        _ = IntegrationWorkspace.cleanup(state.workspace, attempt)
        send(self(), :scan)
        {:noreply, state}

      {:ok, attempt} ->
        {:noreply, open_session(%{state | session: nil}, attempt, true)}

      {:error, reason} ->
        {:noreply, %{state | last_error: inspect(reason)}}
    end
  end

  def handle_info(_message, state), do: {:noreply, state}

  @impl true
  def terminate(_reason, %{session: nil}), do: :ok
  def terminate(_reason, state), do: safe_close(state.session.handle)

  defp maybe_start(%{session: nil, start_backends: true, recovery_enabled: true} = state) do
    if Pika.Persistence.current_campaign().status in ~w(stopped blocked completed) do
      state
    else
      case IntegrationStore.queue_head(state.campaign_id) do
        {:ok, attempt} -> open_session(state, attempt, false)
        {:error, :integration_queue_empty} -> state
        {:error, reason} -> %{state | last_error: inspect(reason)}
      end
    end
  end

  defp maybe_start(state), do: state

  defp open_session(state, attempt, recovering?) do
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
      artifact_dir: Path.join([state.workspace.artifacts, "logs", attempt.id, "integration"])
    }

    with :ok <- ensure_recoverable_best(state, attempt),
         :ok <- Pika.PromptCatalog.validate([:integration]),
         {:ok, context} <- IntegrationStore.integration_context(state.campaign_id, attempt.id),
         {:ok, instructions} <- IntegrationPrompt.render(attempt, context, state.workspace),
         {:ok, handle} <- AgentBackend.start_link(module, backend_profile, self()),
         {:ok, session} <-
           AgentBackend.open_session(
             handle,
             Path.join(state.workspace.root, attempt.worktree_relative_path),
             profile["model"] || profile[:model],
             effort,
             %{
               url: state.mcp_url,
               token: token,
               role: :integration,
               attempt_id: attempt.id,
               coordinator: self()
             },
             skill_roots(state.workspace),
             recovery_instructions(instructions, recovering?)
           ) do
      identity = %{
        session_id: session.id,
        campaign_id: state.campaign_id,
        attempt_id: attempt.id,
        role: :integration,
        slot_index: nil,
        token_hash: token_hash(token)
      }

      required = ["acquire_integration_lease"]

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
        log_path: "artifacts/logs/#{attempt.id}/integration/#{session.id}.jsonl"
      }

      state = %{
        state
        | session: session_state,
          token_hash: identity.token_hash,
          monitor: monitor,
          recovery_count: state.recovery_count + if(recovering?, do: 1, else: 0)
      }

      state = ensure_log(state)

      case AgentBackend.start_turn(handle, kickoff(attempt, recovering?)) do
        {:ok, turn_id} -> put_in(state.session.active_turn_id, turn_id)
        {:error, reason} -> recover(state, {:start_turn_failed, reason})
      end
    else
      {:error, {:unexplained_best_state, _details} = reason} ->
        _ = IntegrationStore.block(state.campaign_id, attempt.id, reason)

        %{
          state
          | recovery_enabled: false,
            last_error: inspect(reason),
            session: nil,
            token_hash: nil,
            monitor: nil
        }

      {:error, reason} ->
        Process.send_after(self(), {:restart, attempt.id}, 250)
        %{state | last_error: inspect(reason)}
    end
  end

  defp ensure_recoverable_best(state, attempt) do
    with {:ok, actual_sha} <- Pika.Git.head(state.workspace.repo) do
      expected_sha = Pika.Persistence.current_campaign().best_sha

      if actual_sha == expected_sha do
        :ok
      else
        result =
          with {:ok, receipt} <- IntegrationStore.receipt_for_attempt(attempt.id),
               {:ok, intent} <- IntegrationStore.intent_for_attempt(attempt.id),
               :ok <-
                 IntegrationWorkspace.verify_merge(
                   state.workspace,
                   attempt,
                   receipt,
                   intent,
                   actual_sha
                 ) do
            :ok
          end

        case result do
          :ok ->
            :ok

          error ->
            {:error,
             {:unexplained_best_state,
              %{expected_sha: expected_sha, actual_sha: actual_sha, verification: inspect(error)}}}
        end
      end
    else
      {:error, reason} ->
        {:error, {:unexplained_best_state, %{verification: inspect(reason)}}}
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
      hash = request_hash(tool, args)

      case AttemptStore.lookup_idempotency(session_id, tool, key, hash) do
        {:replay, response} ->
          {:reply, response, state}

        :conflict ->
          {:reply, mcp_error("idempotency_conflict", "request changed"), state}

        :missing ->
          {response, next_state} = perform_write(tool, args, state)
          :ok = AttemptStore.store_idempotency(session_id, tool, key, hash, response)
          {:reply, response, next_state}
      end
    end
  end

  defp execute_mcp(tool, _args, state),
    do: {:reply, mcp_error("forbidden_role", "tool is unavailable: #{tool}"), state}

  defp perform_read("get_integration_context", _args, state) do
    attempt_id = state.session.identity.attempt_id

    case IntegrationStore.integration_context(state.campaign_id, attempt_id) do
      {:ok, context} ->
        response =
          {:ok,
           Map.merge(context, %{
             identity: public_identity(state.session.identity),
             required_operations: state.session.required |> MapSet.to_list() |> Enum.sort(),
             best_worktree: state.workspace.repo,
             patch_path:
               Path.join(state.workspace.root, "artifacts/patches/#{attempt_id}/candidate.patch")
           })}

        {response, state}

      {:error, reason} ->
        {mcp_error("missing_required_data", inspect(reason)), state}
    end
  end

  defp perform_write("register_artifact", args, state) do
    attempt_id = state.session.identity.attempt_id

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
             owner_type: "attempt",
             owner_id: attempt_id,
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

  defp perform_write("acquire_integration_lease", args, state) do
    attempt_id = state.session.identity.attempt_id

    case IntegrationStore.acquire_lease(
           state.campaign_id,
           attempt_id,
           state.session.session.id,
           args["expected_best_sha"]
         ) do
      {:ok, lease} ->
        state = next_after_lease(state, lease)
        {{:ok, lease}, state}

      {:error, reason} ->
        {mcp_error("lease_conflict", inspect(reason)), state}
    end
  end

  defp perform_write("complete_refresh", args, state) do
    attempt_id = state.session.identity.attempt_id

    with {:ok, context} <- AttemptStore.campaign_context(state.campaign_id),
         {:ok, attempt} <- AttemptStore.attempt(attempt_id),
         true <- args["sampling_revision_id"] == attempt.sampling_revision_id,
         true <- args["new_base_sha"] == context.best_sha,
         {:ok, worktree} <- Pika.AttemptWorkspace.current(state.workspace, attempt),
         :ok <-
           Pika.AttemptWorkspace.verify_candidate(
             state.workspace,
             %{worktree | base_sha: context.best_sha},
             args["candidate_sha"],
             %{protected_paths: context.protected_paths}
           ),
         true <- args["harness_digest"] == context.spec_revision.protected_digest,
         {:ok, samples} <- AttemptStore.artifact(state.campaign_id, args["samples_artifact"]),
         {:ok, correctness} <-
           AttemptStore.artifact(state.campaign_id, args["correctness_artifact"]),
         :ok <- own_artifact(samples, attempt_id),
         :ok <- own_artifact(correctness, attempt_id),
         {:ok, samples_path} <- Pika.ArtifactStore.resolve(state.workspace, samples.relative_path),
         {:ok, correctness_path} <-
           Pika.ArtifactStore.resolve(state.workspace, correctness.relative_path),
         {:ok, metrics} <-
           Pika.Measurement.evaluate_iteration(samples_path, correctness_path, %{
             base_sha: context.best_sha,
             candidate_sha: args["candidate_sha"],
             case_ids: sampled_case_ids(attempt.sampling_revision_id),
             metrics: context.metrics
           }),
         {:ok, refreshed} <-
           IntegrationStore.complete_refresh(
             args["lease_id"],
             state.session.session.id,
             attempt_id,
             context.best_sha,
             args["candidate_sha"],
             metrics
           ) do
      state = set_required(state, ["submit_full_regression"])
      {{:ok, refreshed}, state}
    else
      false ->
        {mcp_error("identity_mismatch", "refresh identity mismatch"), state}

      {:error, reason} ->
        {mcp_error("missing_required_data", "refresh rejected", %{reason: reason}), state}
    end
  end

  defp perform_write("submit_full_regression", args, state) do
    attempt_id = state.session.identity.attempt_id

    with {:ok, context} <- AttemptStore.campaign_context(state.campaign_id),
         {:ok, attempt} <- AttemptStore.attempt(attempt_id),
         true <- args["base_sha"] == context.best_sha and args["base_sha"] == attempt.base_sha,
         true <- args["candidate_sha"] == attempt.candidate_sha,
         true <- args["harness_digest"] == context.spec_revision.protected_digest,
         {:ok, screening} <- registered_artifact(state, args["screening_artifact"], attempt_id),
         {:ok, correctness} <-
           registered_artifact(state, args["correctness_artifact"], attempt_id),
         {:ok, full} <- optional_artifact(state, args["full_artifact"], attempt_id),
         {:ok, screening_path} <-
           Pika.ArtifactStore.resolve(state.workspace, screening.relative_path),
         {:ok, correctness_path} <-
           Pika.ArtifactStore.resolve(state.workspace, correctness.relative_path),
         {:ok, full_path} <- optional_path(state.workspace, full),
         {:ok, result} <-
           Pika.Measurement.evaluate_integration(
             screening_path,
             full_path,
             correctness_path,
             %{
               base_sha: attempt.base_sha,
               candidate_sha: attempt.candidate_sha,
               case_ids: Enum.map(context.cases, & &1["id"]),
               metrics: context.metrics
             },
             context.best_metrics
           ),
         {:ok, receipt} <-
           IntegrationStore.issue_receipt(
             args["lease_id"],
             state.session.session.id,
             attempt_id,
             %{
               candidate_sha: attempt.candidate_sha,
               harness_digest: context.spec_revision.protected_digest,
               metrics: result.metrics,
               regressions: result.regressions,
               correctness_artifact_id: correctness.id,
               screening_artifact_id: screening.id,
               full_artifact_id: full && full.id
             }
           ) do
      next = if(receipt.status == "passed", do: ["create_merge_intent"], else: ["reject_attempt"])
      {{:ok, Map.put(receipt, :escalated, result.escalated)}, set_required(state, next)}
    else
      false ->
        {mcp_error("identity_mismatch", "Full Regression identity mismatch"), state}

      {:error, reason} ->
        {mcp_error("missing_required_data", "Full Regression rejected", %{reason: reason}), state}
    end
  end

  defp perform_write("reject_attempt", args, state) do
    attempt_id = state.session.identity.attempt_id

    case IntegrationStore.reject_attempt(
           args["lease_id"],
           state.session.session.id,
           attempt_id,
           args["receipt_id"],
           List.wrap(args["representative_case_ids"])
         ) do
      {:ok, attempt} -> {{:ok, attempt}, set_required(state, [])}
      {:error, reason} -> {mcp_error("missing_required_data", inspect(reason)), state}
    end
  end

  defp perform_write("create_merge_intent", args, state) do
    attempt_id = state.session.identity.attempt_id

    case IntegrationStore.create_merge_intent(
           args["lease_id"],
           state.session.session.id,
           attempt_id,
           args["receipt_id"],
           args["idempotency_key"]
         ) do
      {:ok, intent} -> {{:ok, intent}, set_required(state, ["complete_merge"])}
      {:error, reason} -> {mcp_error("missing_required_data", inspect(reason)), state}
    end
  end

  defp perform_write("complete_merge", args, state) do
    attempt_id = state.session.identity.attempt_id

    with {:ok, attempt} <- AttemptStore.attempt(attempt_id),
         {:ok, receipt} <- IntegrationStore.receipt_for_attempt(attempt_id),
         {:ok, intent} <- IntegrationStore.intent_for_attempt(attempt_id),
         true <- receipt.id == args["receipt_id"] and intent.id == args["intent_id"],
         :ok <-
           IntegrationWorkspace.verify_merge(
             state.workspace,
             attempt,
             receipt,
             intent,
             args["new_sha"]
           ),
         {:ok, accepted} <-
           IntegrationStore.complete_merge(
             args["lease_id"],
             state.session.session.id,
             attempt_id,
             receipt.id,
             intent.id,
             args["new_sha"]
           ) do
      {{:ok, accepted}, set_required(state, [])}
    else
      false ->
        {mcp_error("identity_mismatch", "Receipt or Intent mismatch"), state}

      {:error, reason} ->
        {mcp_error("missing_required_data", "merge verification failed", %{reason: reason}),
         state}
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
      finish(state)
    else
      missing = required(state)
      _ = AttemptStore.update_session(state.session.session.id, "awaiting_report", missing)

      case AgentBackend.start_turn(
             state.session.handle,
             "Integration completion gate remains open. Complete: #{Enum.join(missing, ", ")}."
           ) do
        {:ok, turn_id} -> put_in(state.session.active_turn_id, turn_id)
        {:error, reason} -> recover(state, {:followup_failed, reason})
      end
    end
  end

  defp apply_event(state, %{type: type, data: data})
       when type in [:backend_error, :process_exited],
       do: recover(state, {type, data})

  defp apply_event(state, _event) do
    _ =
      AttemptStore.update_session(state.session.session.id, "running", required(state), %{
        event_increment: 1
      })

    state
  end

  defp next_after_lease(state, lease) do
    operation =
      cond do
        lease.stale_base ->
          "complete_refresh"

        match?(
          {:ok, %{status: "rejected"}},
          IntegrationStore.receipt_for_attempt(lease.attempt_id)
        ) ->
          "reject_attempt"

        match?({:ok, %{status: "passed"}}, IntegrationStore.receipt_for_attempt(lease.attempt_id)) and
            match?({:ok, _}, IntegrationStore.intent_for_attempt(lease.attempt_id)) ->
          "complete_merge"

        match?({:ok, %{status: "passed"}}, IntegrationStore.receipt_for_attempt(lease.attempt_id)) ->
          "create_merge_intent"

        true ->
          "submit_full_regression"
      end

    set_required(state, [operation])
  end

  defp set_required(state, operations) do
    required = MapSet.new(operations)
    _ = AttemptStore.update_session(state.session.session.id, "running", MapSet.to_list(required))
    put_in(state.session.required, required)
  end

  defp finish(state) do
    attempt_id = state.session.identity.attempt_id
    _ = AttemptStore.update_session(state.session.session.id, "completed", [])
    safe_close(state.session.handle)
    Process.demonitor(state.monitor, [:flush])

    case AttemptStore.attempt(attempt_id) do
      {:ok, %{status: status} = attempt} when status in ~w(accepted rejected) ->
        _ = IntegrationWorkspace.cleanup(state.workspace, attempt)

      _ ->
        :ok
    end

    send(self(), :scan)
    %{state | session: nil, token_hash: nil, monitor: nil}
  end

  defp recover(%{session: nil} = state, _reason), do: state

  defp recover(state, reason) do
    attempt_id = state.session.identity.attempt_id
    _ = AttemptStore.update_session(state.session.session.id, "interrupted", required(state))
    safe_close(state.session.handle)
    if state.monitor, do: Process.demonitor(state.monitor, [:flush])
    next = %{state | session: nil, token_hash: nil, monitor: nil, last_error: inspect(reason)}

    case AttemptStore.attempt(attempt_id) do
      {:ok, %{status: status} = attempt} when status in ~w(accepted rejected cancelled) ->
        _ = IntegrationWorkspace.cleanup(state.workspace, attempt)
        send(self(), :scan)
        next

      _ when state.recovery_enabled ->
        Process.send_after(self(), {:restart, attempt_id}, 50)
        next

      _ ->
        next
    end
  end

  defp cleanup_terminal_worktrees(state) do
    AttemptStore.attempts(state.campaign_id, limit: 10_000)
    |> Enum.filter(&(&1.status in ~w(accepted rejected cancelled)))
    |> Enum.each(&IntegrationWorkspace.cleanup(state.workspace, &1))

    state
  end

  defp persist_event(state, event) do
    attrs = %{
      campaign_id: state.campaign_id,
      owner_type: "attempt",
      owner_id: state.session.identity.attempt_id,
      kind: "agent_jsonl",
      metadata: %{session_id: state.session.session.id, role: :integration}
    }

    record = %{
      at: DateTime.utc_now() |> DateTime.to_iso8601(),
      session_id: event.session_id,
      turn_id: event.turn_id,
      type: event.type,
      backend: event.backend,
      data: Pika.JSONSafe.json_safe(event.data)
    }

    case Pika.ArtifactStore.append_jsonl(state.workspace, state.session.log_path, record, attrs) do
      {:ok, artifact} ->
        _ = AttemptStore.attach_session_log(state.session.session.id, artifact.id)
        state

      {:error, reason} ->
        %{state | last_error: inspect(reason)}
    end
  end

  defp ensure_log(state) do
    attrs = %{
      campaign_id: state.campaign_id,
      owner_type: "attempt",
      owner_id: state.session.identity.attempt_id,
      kind: "agent_jsonl",
      metadata: %{session_id: state.session.session.id, role: :integration}
    }

    case Pika.ArtifactStore.append_jsonl(
           state.workspace,
           state.session.log_path,
           %{
             at: DateTime.utc_now() |> DateTime.to_iso8601(),
             type: "session_opened",
             role: "integration"
           },
           attrs
         ) do
      {:ok, artifact} ->
        _ = AttemptStore.attach_session_log(state.session.session.id, artifact.id)
        state

      {:error, reason} ->
        %{state | last_error: inspect(reason)}
    end
  end

  defp registered_artifact(state, path, attempt_id) do
    with {:ok, artifact} <- AttemptStore.artifact(state.campaign_id, path),
         :ok <- own_artifact(artifact, attempt_id) do
      {:ok, artifact}
    end
  end

  defp optional_artifact(_state, nil, _attempt_id), do: {:ok, nil}
  defp optional_artifact(_state, "", _attempt_id), do: {:ok, nil}

  defp optional_artifact(state, path, attempt_id),
    do: registered_artifact(state, path, attempt_id)

  defp optional_path(_workspace, nil), do: {:ok, nil}

  defp optional_path(workspace, artifact),
    do: Pika.ArtifactStore.resolve(workspace, artifact.relative_path)

  defp own_artifact(%{owner_type: "attempt", owner_id: id}, id), do: :ok
  defp own_artifact(_artifact, _attempt_id), do: {:error, :artifact_identity_mismatch}

  defp sampled_case_ids(sampling_revision_id) do
    Pika.Repo.query!(
      "SELECT bc.name FROM sampling_revision_cases src JOIN benchmark_cases bc ON bc.id = src.benchmark_case_id WHERE src.sampling_revision_id = ? ORDER BY bc.ordinal",
      [sampling_revision_id]
    ).rows
    |> List.flatten()
  end

  defp required(state), do: state.session.required |> MapSet.to_list() |> Enum.sort()
  defp recovery_instructions(instructions, false), do: instructions

  defp recovery_instructions(instructions, true),
    do:
      instructions <>
        "\n\nRecover the persisted Integration Lease, Receipt, and Intent before doing new work."

  defp kickoff(attempt, false), do: "Integrate FIFO Attempt ##{attempt.ordinal}."
  defp kickoff(attempt, true), do: "Recover Integration for existing Attempt ##{attempt.ordinal}."

  defp profile(workspace) do
    backend = get_in(workspace.snapshot, ["immutable", "backend"])

    %{
      "name" => "integration",
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
  defp backend_atom(:cursor_acp), do: :cursor_acp
  defp backend_atom("cursor_acp"), do: :cursor_acp
  defp backend_atom(_), do: :codex_app_server
  defp backend_module(:cursor_acp), do: Pika.AgentBackend.CursorACP
  defp backend_module(:codex_app_server), do: Pika.AgentBackend.CodexAppServer
  defp effort_atom(value) when is_atom(value), do: value
  defp effort_atom(value), do: String.to_existing_atom(value)
  defp random_token, do: :crypto.strong_rand_bytes(32) |> Base.url_encode64(padding: false)
  defp token_hash(token), do: :crypto.hash(:sha256, token) |> Base.encode16(case: :lower)

  defp request_hash(tool, args),
    do: :crypto.hash(:sha256, :erlang.term_to_binary({tool, args})) |> Base.encode16(case: :lower)

  defp mcp_error(code, message, details \\ %{}), do: {:error, code, message, details}
  defp public_identity(identity), do: Map.drop(identity, [:token_hash])

  defp public_session(session),
    do: %{
      id: session.session.id,
      identity: public_identity(session.identity),
      required: session.required |> MapSet.to_list() |> Enum.sort()
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

  defp optional(fun, arg),
    do:
      case(fun.(arg),
        do: (
          {:ok, value} -> value
          _ -> nil
        )
      )

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
      _ -> GenServer.call(server, message, 30_000)
    end
  end
end
