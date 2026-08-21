defmodule Pika.AttemptCoordinator do
  @moduledoc "Attempt lifecycle and user-control coordinator; Agent execution is delegated to Symphony."

  use GenServer
  require Logger

  alias Pika.Agent.{Actor, Directory, Symphony}
  alias Pika.Agent.Role.Work
  alias Pika.AgentBackend.JSONLWriter
  alias Pika.AttemptStore, as: Store
  alias Pika.AttemptWorkspace, as: Workspace
  alias Pika.Repo

  @progress_log_interval_ms 30_000

  def start_link(opts \\ []) do
    case Keyword.get(opts, :name, __MODULE__) do
      nil -> GenServer.start_link(__MODULE__, opts)
      name -> GenServer.start_link(__MODULE__, opts, name: name)
    end
  end

  def snapshot(server \\ __MODULE__), do: call(server, :snapshot)
  def dispatch(server \\ __MODULE__), do: call(server, :dispatch)
  def stop_now(server \\ __MODULE__), do: call(server, :stop_now)
  def resume(server \\ __MODULE__), do: call(server, :resume)
  def progress_topic(campaign_id), do: "pika:attempt-progress:#{campaign_id}"

  def authorize(token, server \\ __MODULE__) do
    case Directory.authorize(token) do
      :ok -> :ok
      _ -> call(server, {:authorize, token})
    end
  end

  def read_plan(token, server \\ __MODULE__) do
    case Directory.lookup(token) do
      {:ok, binding} when binding.role_id in ["plan", "iteration"] ->
        read_plan_for(binding.work.id, binding.work.campaign_id, workspace_for(server))

      _ ->
        call(server, {:read_plan, token})
    end
  end

  def mcp_call(token, tool, args, server \\ __MODULE__) do
    case Directory.lookup(token) do
      {:ok, %{role_id: role_id, actor: actor}} when role_id in ["plan", "iteration"] ->
        legacy_actor_call(actor, tool, args)

      _ ->
        call(server, {:mcp, token, tool, args})
    end
  end

  def create_btw(attempt_id, body, mode, idempotency_key, server \\ __MODULE__),
    do: call(server, {:create_btw, attempt_id, body, mode, idempotency_key})

  @impl true
  def init(opts) do
    workspace = Keyword.get_lazy(opts, :workspace, &Pika.WorkspaceLock.workspace/0)
    campaign = Keyword.get_lazy(opts, :campaign, &Pika.Persistence.current_campaign/0)
    campaign_id = Keyword.get(opts, :campaign_id, campaign.id)
    profiles = Keyword.get(opts, :profiles, configured_profiles(workspace))

    subscribe(campaign_id)

    state = %{
      workspace: workspace,
      campaign_id: campaign_id,
      profiles: profiles,
      reload_profiles: not Keyword.has_key?(opts, :profiles),
      backend_modules: Keyword.get(opts, :backend_modules, %{}),
      mcp_url: Keyword.get(opts, :mcp_url),
      start_backends: Keyword.get(opts, :start_backends, true),
      auto_dispatch: Keyword.get(opts, :auto_dispatch, true),
      recovery_enabled: campaign.status not in ~w(stopped blocked completed),
      max_unverified_attempts:
        Keyword.get(opts, :max_unverified_attempts, configured_max_unverified(workspace)),
      directory: Keyword.get(opts, :directory, Directory),
      symphony: Keyword.get(opts, :symphony),
      owns_symphony: false,
      actor_supervisor: Keyword.get(opts, :actor_supervisor, Pika.Agent.ActorSupervisor),
      sessions: %{},
      last_error: nil,
      progress_log_interval_ms:
        Keyword.get(opts, :progress_log_interval_ms, @progress_log_interval_ms),
      progress_log_level: Keyword.get(opts, :progress_log_level, :info),
      progress_log_task: nil,
      progress_probe: Keyword.get(opts, :progress_probe, &attempt_progress/2)
    }

    state = if state.recovery_enabled, do: recover_existing_attempts(state), else: state

    with {:ok, state} <- ensure_symphony(state) do
      schedule_progress_log(state.progress_log_interval_ms)
      send(self(), :recover)
      {:ok, state}
    end
  end

  @impl true
  def handle_call(:snapshot, _from, state), do: {:reply, public_snapshot(state), state}

  def handle_call(:dispatch, _from, state) do
    state = state |> dispatch_available() |> reconcile_actors()
    {:reply, :ok, state}
  end

  def handle_call(:stop_now, _from, state) do
    state = stop_attempt_actors(%{state | recovery_enabled: false})
    {:reply, :ok, state}
  end

  def handle_call(:resume, _from, state) do
    state = %{state | recovery_enabled: true} |> recover_existing_attempts() |> reconcile_actors()
    if state.auto_dispatch, do: send(self(), :dispatch_next)
    {:reply, :ok, state}
  end

  def handle_call({:authorize, token}, _from, state) do
    result =
      case Directory.lookup(token, state.directory) do
        {:ok, %{role_id: role_id}} when role_id in ["plan", "iteration"] -> :ok
        _ -> {:error, :unauthorized}
      end

    {:reply, result, state}
  end

  def handle_call({:read_plan, token}, _from, state) do
    result =
      case Directory.lookup(token, state.directory) do
        {:ok, %{role_id: role_id, work: work}} when role_id in ["plan", "iteration"] ->
          read_plan_for(work.id, state.campaign_id, state.workspace)

        _ ->
          {:error, :unauthorized}
      end

    {:reply, result, state}
  end

  def handle_call({:mcp, token, tool, args}, _from, state) do
    response =
      case Directory.lookup(token, state.directory) do
        {:ok, %{role_id: role_id, actor: actor}} when role_id in ["plan", "iteration"] ->
          legacy_actor_call(actor, tool, args)

        _ ->
          {:error, "unauthorized", "invalid Backend Session token", %{}}
      end

    {:reply, response, state}
  end

  def handle_call({:create_btw, attempt_id, body, mode, key}, _from, state) do
    body = String.trim(body || "")

    result =
      cond do
        body == "" -> {:error, :empty_message}
        mode not in ~w(chat current future) -> {:error, :invalid_btw_mode}
        not is_binary(key) or key == "" -> {:error, :idempotency_key_required}
        true -> create_btw_idempotently(state, attempt_id, body, mode, key)
      end

    {:reply, unwrap_btw(result), maybe_steer_guidance(state, result)}
  end

  @impl true
  def handle_info(:recover, state) do
    state = reconcile_actors(state)

    {:noreply,
     if(state.auto_dispatch, do: dispatch_available(state) |> reconcile_actors(), else: state)}
  end

  def handle_info(:dispatch_next, state) do
    {:noreply,
     if(state.auto_dispatch, do: dispatch_available(state) |> reconcile_actors(), else: state)}
  end

  def handle_info({:domain_event, _event}, state) do
    state = reconcile_actors(state)

    {:noreply,
     if(state.auto_dispatch, do: dispatch_available(state) |> reconcile_actors(), else: state)}
  end

  def handle_info({:integration_event, %{event_type: type} = event}, state)
      when type in ["best_advanced", "sampling_advanced"] do
    state = steer_campaign_event(state, event)

    {:noreply,
     if(state.auto_dispatch, do: dispatch_available(state) |> reconcile_actors(), else: state)}
  end

  def handle_info({:integration_event, _event}, state), do: {:noreply, state}

  def handle_info({:campaign_updated, %{status: :optimizing}}, state) do
    state = %{state | recovery_enabled: true} |> reconcile_actors()

    {:noreply,
     if(state.auto_dispatch, do: dispatch_available(state) |> reconcile_actors(), else: state)}
  end

  def handle_info({:campaign_updated, _snapshot}, state), do: {:noreply, state}

  def handle_info({:agent_event, %Work{} = work, event}, state)
      when work.role_id in ["plan", "iteration"] do
    {:noreply, track_agent_event(state, work, event)}
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

  def handle_info(_message, state), do: {:noreply, state}

  @impl true
  def terminate(_reason, state) do
    stop_progress_log(state.progress_log_task)
    state = stop_attempt_actors(state)

    if state.owns_symphony and is_pid(state.symphony) and Process.alive?(state.symphony),
      do: GenServer.stop(state.symphony, :normal)

    :ok
  end

  defp dispatch_available(%{recovery_enabled: false} = state), do: state

  defp dispatch_available(state) do
    state = reload_profiles(state)

    case Store.dispatch_state(state.campaign_id) do
      {:ok, %{status: "optimizing", dispatch_gate: gate}} when gate in [nil, ""] ->
        state.profiles
        |> Enum.with_index()
        |> Enum.reduce(state, fn {_profile, slot_index}, acc -> dispatch_slot(slot_index, acc) end)

      _ ->
        state
    end
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
         {:ok, _attempt} <- Store.mark_running(attempt.id) do
      state
    else
      {:error, reason} ->
        _ = Store.mark_interrupted(attempt.id, reason)
        %{state | last_error: inspect(reason)}
    end
  end

  defp recover_existing_attempts(%{start_backends: false} = state), do: state

  defp recover_existing_attempts(state) do
    Store.active_attempts(state.campaign_id)
    |> Enum.reduce(state, fn attempt, acc ->
      if integration_owned_attempt?(attempt) or attempt.status == "ready_for_integration" do
        acc
      else
        _ = Store.interrupt_active_sessions_for_attempt(attempt.id)

        if attempt.status != "interrupted",
          do: Store.mark_interrupted(attempt.id, :server_recovery)

        case Store.mark_running(attempt.id) do
          {:ok, _running} -> acc
          {:error, reason} -> %{acc | last_error: inspect(reason)}
        end
      end
    end)
  end

  defp reconcile_actors(%{start_backends: false} = state), do: state
  defp reconcile_actors(%{recovery_enabled: false} = state), do: state

  defp reconcile_actors(state) do
    case Symphony.reconcile(state.symphony) do
      :ok -> state
      other -> %{state | last_error: inspect(other)}
    end
  catch
    :exit, reason -> %{state | last_error: inspect(reason)}
  end

  defp ensure_symphony(%{symphony: symphony} = state) when not is_nil(symphony),
    do: {:ok, state}

  defp ensure_symphony(state) do
    case Process.whereis(Symphony) do
      pid when is_pid(pid) ->
        {:ok, %{state | symphony: pid}}

      nil ->
        opts = [
          name: nil,
          workspace: state.workspace,
          campaign_id: state.campaign_id,
          directory: state.directory,
          actor_supervisor: state.actor_supervisor,
          sources: [Pika.Agent.WorkSources.Plan, Pika.Agent.WorkSources.Iteration],
          role_owners: %{"plan" => :actor, "iteration" => :actor},
          actor_opts: actor_opts(state),
          capacity: max(length(state.profiles), 1),
          reconcile_interval_ms: :infinity
        ]

        case Symphony.start_link(opts) do
          {:ok, pid} -> {:ok, %{state | symphony: pid, owns_symphony: true}}
          {:error, reason} -> {:stop, {:attempt_symphony_start_failed, reason}}
        end
    end
  end

  defp actor_opts(state) do
    base = [
      backend_modules: state.backend_modules,
      profiles: state.profiles,
      max_followups: 8
    ]

    if state.mcp_url, do: Keyword.put(base, :mcp_url, state.mcp_url), else: base
  end

  defp stop_attempt_actors(state) do
    state.symphony
    |> Symphony.active()
    |> Enum.each(fn
      {%{role_id: role_id, campaign_id: campaign_id, id: attempt_id}, actor}
      when role_id in ["plan", "iteration"] and campaign_id == state.campaign_id ->
        _ = Store.mark_interrupted(attempt_id, :stop_now)
        Actor.stop(actor)

      _ ->
        :ok
    end)

    %{state | sessions: %{}}
  catch
    :exit, _reason -> %{state | sessions: %{}}
  end

  defp create_btw_idempotently(state, attempt_id, body, mode, key) do
    case active_iteration(state, attempt_id) do
      nil ->
        {:error, :btw_requires_running_attempt}

      %{session_id: session_id} ->
        hash = request_hash("create_btw", %{attempt_id: attempt_id, body: body, mode: mode})

        case Store.lookup_idempotency(session_id, "create_btw", key, hash) do
          {:replay, response} ->
            {:replay, response}

          :conflict ->
            {:error, :idempotency_conflict}

          :missing ->
            kind = %{"chat" => "side", "current" => "attempt", "future" => "campaign"}[mode]

            response =
              Store.create_guidance(state.campaign_id, attempt_id, session_id, kind, body)

            :ok = Store.store_idempotency(session_id, "create_btw", key, hash, response)
            {:new, response}
        end
    end
  end

  defp unwrap_btw({:new, response}), do: response
  defp unwrap_btw({:replay, response}), do: response
  defp unwrap_btw(error), do: error

  defp maybe_steer_guidance(state, {:new, {:ok, %{kind: "attempt"} = guidance}}) do
    case active_iteration(state, guidance.attempt_id) do
      %{actor: actor} ->
        case Actor.steer(actor, guidance.body) do
          {:ok, _turn_id} -> Store.mark_guidance_injected(guidance.id)
          _ -> :ok
        end

      nil ->
        :ok
    end

    state
  end

  defp maybe_steer_guidance(state, _result), do: state

  defp active_iteration(state, attempt_id) do
    work = %Work{
      role_id: "iteration",
      kind: :attempt,
      id: attempt_id,
      campaign_id: state.campaign_id
    }

    with {:ok, actor} <- Directory.lookup_work(work, state.directory),
         %{session_id: session_id} when is_binary(session_id) <- Actor.status(actor) do
      %{actor: actor, session_id: session_id}
    else
      _ -> nil
    end
  catch
    :exit, _reason -> nil
  end

  defp steer_campaign_event(state, event) do
    message =
      "Pika #{event.event_type}: " <>
        Jason.encode!(Pika.JSONSafe.json_safe(event.payload)) <>
        ". Keep the current Attempt's fixed Best and Sampling Revision; acknowledge via Mailbox when convenient."

    state.symphony
    |> Symphony.active()
    |> Enum.each(fn
      {%{role_id: role_id, campaign_id: campaign_id}, actor}
      when role_id in ["plan", "iteration"] and campaign_id == state.campaign_id ->
        _ = Actor.steer(actor, message)

      _ ->
        :ok
    end)

    state
  catch
    :exit, _reason -> state
  end

  defp track_agent_event(state, work, event) do
    status = actor_status(work, state)

    entry = %{
      session_id: event.session_id,
      attempt_id: work.id,
      role: role_atom(work.role_id),
      active_turn_id: event.turn_id,
      required_operations: required_from_status(status),
      last_event: event.type,
      last_event_at: DateTime.utc_now() |> DateTime.to_iso8601(),
      last_event_summary: backend_event_summary(event)
    }

    %{state | sessions: Map.put(state.sessions, event.session_id, entry)}
  end

  defp actor_status(work, state) do
    with {:ok, actor} <- Directory.lookup_work(work, state.directory), do: Actor.status(actor)
  catch
    :exit, _reason -> nil
  end

  defp required_from_status(%{progress: %{required_operations: required}}), do: required
  defp required_from_status(_status), do: []

  defp backend_event_summary(event) do
    data = event.data |> then(&(&1 || %{})) |> JSONLWriter.redact() |> Pika.JSONSafe.json_safe()

    value =
      first_progress_value(data) ||
        if(data == %{}, do: to_string(event.type), else: Jason.encode!(data))

    value |> to_string() |> String.slice(0, 500)
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
      sessions: Map.values(state.sessions),
      recovery_count: recovery_count(state.campaign_id),
      last_error: state.last_error,
      level: state.progress_log_level,
      probe: state.progress_probe
    }

    {:ok, pid} = Task.start(fn -> log_work_snapshot(input) end)
    %{state | progress_log_task: %{pid: pid, monitor: Process.monitor(pid)}}
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
        changed_paths: changes |> Enum.take(8) |> Enum.map(&String.slice(&1, 0, 160)),
        changed_paths_truncated: length(changes) > 8
      }
    else
      false -> %{available: false, reason: "worktree_missing"}
      {:error, reason} -> %{available: false, reason: truncate(inspect(reason), 500)}
    end
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
      recovery_count: recovery_count(state.campaign_id),
      last_error: state.last_error
    }
  end

  defp recovery_count(campaign_id) do
    Repo.query!(
      "SELECT attempt_id, COUNT(*) FROM agent_sessions WHERE campaign_id = ? AND role IN ('plan', 'iteration') AND session_mode = 'recovering' AND attempt_id IS NOT NULL GROUP BY attempt_id",
      [campaign_id]
    ).rows
    |> Map.new(fn [attempt_id, count] -> {attempt_id, count} end)
  rescue
    _error -> %{}
  end

  defp read_plan_for(attempt_id, campaign_id, workspace) do
    with {:ok, attempt} <- Store.attempt(attempt_id),
         true <- not is_nil(attempt.plan_artifact_id),
         relative_path = "artifacts/plans/#{attempt_id}/plan.md",
         {:ok, artifact} <- Store.artifact(campaign_id, relative_path),
         true <- artifact.id == attempt.plan_artifact_id,
         {:ok, path} <- Pika.ArtifactStore.resolve(workspace, relative_path),
         {:ok, body} <- File.read(path) do
      {:ok, %{attempt_id: attempt_id, text: body}}
    else
      false -> {:error, :plan_not_found}
      {:error, _reason} = error -> error
    end
  end

  defp workspace_for(server) do
    case GenServer.whereis(server) do
      nil -> Pika.WorkspaceLock.workspace()
      _pid -> :sys.get_state(server).workspace
    end
  catch
    :exit, _reason -> Pika.WorkspaceLock.workspace()
  end

  defp reload_profiles(%{reload_profiles: false} = state), do: state

  defp reload_profiles(state) do
    case Pika.RuntimeConfig.mutable(state.workspace) do
      {:ok, %{"iteration_agents" => profiles}} when is_list(profiles) and profiles != [] ->
        %{state | profiles: profiles, last_error: nil}

      {:ok, _mutable} ->
        state

      {:error, reason} ->
        %{state | last_error: inspect(reason)}
    end
  end

  defp configured_profiles(workspace) do
    get_in(workspace.snapshot, ["mutable", "iteration_agents"]) || [default_profile(workspace)]
  end

  defp configured_max_unverified(workspace),
    do: get_in(workspace.snapshot, ["mutable", "max_unverified_attempts"]) || 0

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

  defp legacy_actor_call(actor, tool, args) do
    case Actor.invoke(actor, tool, args) do
      {:ok, outcome} ->
        {:ok, legacy_value(outcome.value)}

      {:error, %Pika.Agent.Role.Error{} = error} ->
        {:error, Atom.to_string(error.code), error.message, error.details}
    end
  catch
    :exit, reason -> {:error, "actor_unavailable", inspect(reason), %{}}
  end

  defp legacy_value(%_{} = struct), do: struct

  defp legacy_value(map) when is_map(map),
    do: Map.new(map, fn {key, value} -> {legacy_key(key), value} end)

  defp legacy_value(value), do: value

  defp legacy_key(key) when is_binary(key) do
    String.to_existing_atom(key)
  rescue
    ArgumentError -> key
  end

  defp legacy_key(key), do: key

  defp request_hash(tool, args) do
    :crypto.hash(:sha256, :erlang.term_to_binary({tool, args}))
    |> Base.encode16(case: :lower)
  end

  defp enrich_attempt(attempt),
    do: Map.put(attempt, :metrics, Store.metrics_for_attempt(attempt.id))

  defp role_atom("plan"), do: :plan
  defp role_atom("iteration"), do: :iteration
  defp short_sha(nil), do: nil
  defp short_sha(sha), do: String.slice(sha, 0, 12)
  defp truncate(nil, _max), do: nil
  defp truncate(value, max), do: value |> to_string() |> String.slice(0, max)

  defp unverified_dispatch_allowed?(0, _limit), do: true
  defp unverified_dispatch_allowed?(count, limit), do: count < limit

  defp active_attempt_statuses,
    do:
      ~w(queued running awaiting_report refreshing integrating interrupted ready_for_integration)

  defp integration_owned_attempt?(%{status: status}),
    do: status in ~w(ready_for_integration refreshing integrating)

  defp subscribe(campaign_id) do
    if Process.whereis(Pika.PubSub) do
      Phoenix.PubSub.subscribe(Pika.PubSub, Pika.Persistence.topic(campaign_id))
      Phoenix.PubSub.subscribe(Pika.PubSub, Pika.Alignment.Campaign.topic())
      Phoenix.PubSub.subscribe(Pika.PubSub, "pika:optimization:events")
      Phoenix.PubSub.subscribe(Pika.PubSub, "pika:agent:#{campaign_id}")
    end
  end

  defp call(server, message) do
    case GenServer.whereis(server) do
      nil -> {:error, :not_started}
      _pid -> GenServer.call(server, message, 120_000)
    end
  end
end
