defmodule Pika.SyncCoordinator do
  @moduledoc "Sync domain lifecycle coordinator; Agent execution is delegated to Symphony."

  use GenServer

  alias Pika.Agent.{Actor, Directory, Symphony}
  alias Pika.Agent.Role.Work
  alias Pika.Agent.Roles.Sync.Domain
  alias Pika.{AttemptStore, SyncStore, SyncWorkspace}

  def start_link(opts \\ []) do
    case Keyword.get(opts, :name, __MODULE__) do
      nil -> GenServer.start_link(__MODULE__, opts)
      name -> GenServer.start_link(__MODULE__, opts, name: name)
    end
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

  def authorize(token, server \\ __MODULE__) do
    case Directory.authorize(token) do
      :ok -> :ok
      _ -> call(server, {:authorize, token})
    end
  end

  def mcp_call(token, tool, args, server \\ __MODULE__),
    do: call(server, {:mcp, token, tool, args})

  @impl true
  def init(opts) do
    workspace = Keyword.get_lazy(opts, :workspace, &Pika.WorkspaceLock.workspace/0)
    campaign = Keyword.get_lazy(opts, :campaign, &Pika.Persistence.current_campaign/0)

    subscribe(campaign.id)

    state = %{
      workspace: workspace,
      campaign_id: campaign.id,
      recovery_enabled: campaign.status not in ~w(stopped blocked completed),
      after_remote_push: Keyword.get(opts, :after_remote_push),
      directory: Keyword.get(opts, :directory, Directory),
      symphony: Keyword.get(opts, :symphony),
      owns_symphony: false,
      actor_supervisor: Keyword.get(opts, :actor_supervisor, Pika.Agent.ActorSupervisor),
      actor_opts: actor_opts(opts),
      last_error: nil
    }

    with {:ok, state} <- ensure_symphony(state) do
      send(self(), :scan)
      {:ok, state}
    end
  end

  @impl true
  def handle_call(:snapshot, _from, state) do
    {:reply,
     %{
       preview: configured_preview(state),
       run: optional(&SyncStore.latest_run/1, state.campaign_id),
       session: active_actor_status(state),
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
        {:error, _reason} = error -> error
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

  def handle_call(:stop_now, _from, state) do
    stop_active_actor(state)
    {:reply, :ok, %{state | recovery_enabled: false}}
  end

  def handle_call(:resume, _from, state) do
    send(self(), :scan)
    {:reply, :ok, %{state | recovery_enabled: true}}
  end

  def handle_call({:authorize, _token}, _from, state),
    do: {:reply, {:error, :unauthorized}, state}

  def handle_call({:mcp, token, tool, args}, _from, state) do
    response =
      with {:ok, binding} <- Directory.lookup(token, state.directory),
           true <- binding.role_id == "sync" do
        case Actor.invoke(binding.actor, tool, args) do
          {:ok, outcome} -> {:ok, outcome.value}
          {:error, reason} -> {:error, "role_operation_failed", inspect(reason), %{}}
        end
      else
        _ -> {:error, "unauthorized", "invalid Sync Session token", %{}}
      end

    {:reply, response, state}
  end

  @impl true
  def handle_info(:scan, %{recovery_enabled: true} = state), do: {:noreply, scan(state)}
  def handle_info(:scan, state), do: {:noreply, state}

  def handle_info({:domain_event, _event}, state) do
    if state.recovery_enabled, do: send(self(), :scan)
    {:noreply, state}
  end

  def handle_info({:sync_event, _event}, state) do
    if state.recovery_enabled, do: send(self(), :scan)
    {:noreply, state}
  end

  def handle_info({event, _work, _details}, state)
      when event in [
             :agent_actor_completed,
             :agent_actor_skipped,
             :agent_actor_failed,
             :agent_actor_interrupted,
             :agent_actor_stopped
           ] do
    if state.recovery_enabled, do: send(self(), :scan)
    {:noreply, state}
  end

  def handle_info(_message, state), do: {:noreply, state}

  @impl true
  def terminate(_reason, state) do
    stop_active_actor(state)

    if state.owns_symphony and is_pid(state.symphony) and Process.alive?(state.symphony),
      do: GenServer.stop(state.symphony, :normal)

    :ok
  end

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

      %{status: "advancing_best"} = run ->
        recover_external(state, run)

      %{status: "pushing"} = run ->
        case SyncWorkspace.remote_sha(state.workspace.repo, run.remote, run.branch) do
          {:ok, sha} when sha == run.remote_before_sha -> reconcile_actor(state)
          _ -> recover_external(state, run)
        end

      _run ->
        reconcile_actor(state)
    end
  end

  defp prepare(state, run) do
    with {:ok, _} <- SyncStore.begin_prepare(run.id),
         {:ok, context} <- AttemptStore.campaign_context(state.campaign_id),
         {:ok, _workspace} <-
           SyncWorkspace.prepare(state.workspace, run, context.target_snapshot),
         {:ok, _run} <- SyncStore.mark_merging(run.id) do
      reconcile_actor(state)
    else
      {:error, reason} ->
        _ = SyncStore.block(run.id, reason)
        %{state | last_error: inspect(reason)}
    end
  end

  defp recover_external(state, run) do
    case Domain.recover_external(state.workspace, run) do
      {:ok, _completed} -> %{state | last_error: nil}
      {:pending, _run} -> reconcile_actor(state)
      {:error, reason} -> %{state | last_error: inspect(reason)}
    end
  end

  defp reconcile_actor(state) do
    case Symphony.reconcile(state.symphony) do
      :ok -> %{state | last_error: nil}
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
          sources: [Pika.Agent.WorkSources.Sync],
          role_owners: %{"sync" => :actor},
          actor_opts: state.actor_opts,
          reconcile_interval_ms: 250
        ]

        case Symphony.start_link(opts) do
          {:ok, pid} -> {:ok, %{state | symphony: pid, owns_symphony: true}}
          {:error, reason} -> {:stop, {:sync_symphony_start_failed, reason}}
        end
    end
  end

  defp actor_opts(opts) do
    base = [
      backend_modules: Keyword.get(opts, :backend_modules, %{}),
      mcp_url: Keyword.get(opts, :mcp_url),
      domain_options: %{after_remote_push: Keyword.get(opts, :after_remote_push)},
      max_followups: 6
    ]

    case Keyword.fetch(opts, :profile) do
      {:ok, profile} -> Keyword.put(base, :profile, profile)
      :error -> base
    end
  end

  defp active_actor_status(state) do
    with %{} = run <- SyncStore.active_run(state.campaign_id),
         work <- work(run),
         {:ok, actor} <- Directory.lookup_work(work, state.directory) do
      Actor.status(actor)
    else
      _ -> nil
    end
  catch
    :exit, _reason -> nil
  end

  defp stop_active_actor(state) do
    with %{} = run <- SyncStore.active_run(state.campaign_id),
         {:ok, actor} <- Directory.lookup_work(work(run), state.directory) do
      Actor.stop(actor)
    else
      _ -> :ok
    end
  catch
    :exit, _reason -> :ok
  end

  defp work(run) do
    %Work{role_id: "sync", kind: :sync, id: run.id, campaign_id: state_campaign(run)}
  end

  defp state_campaign(run), do: run.campaign_id

  defp configured_preview(state) do
    sync = get_in(state.workspace.snapshot, ["mutable", "sync"]) || %{}
    remote = sync["remote"]
    branch = sync["branch"]

    if is_binary(remote) and is_binary(branch),
      do: optional(fn _ -> SyncWorkspace.preview(state.workspace, remote, branch) end, nil),
      else: nil
  end

  defp subscribe(campaign_id) do
    if Process.whereis(Pika.PubSub) do
      Phoenix.PubSub.subscribe(Pika.PubSub, Pika.Persistence.topic(campaign_id))
      Phoenix.PubSub.subscribe(Pika.PubSub, "pika:sync:events")
      Phoenix.PubSub.subscribe(Pika.PubSub, "pika:agent-lifecycle:#{campaign_id}")
    end
  end

  defp optional(fun, arg) do
    case fun.(arg) do
      {:ok, value} -> value
      _ -> nil
    end
  end

  defp call(server, message) do
    case GenServer.whereis(server) do
      nil -> {:error, :not_started}
      _ -> GenServer.call(server, message, 120_000)
    end
  end
end
