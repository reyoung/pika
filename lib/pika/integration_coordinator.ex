defmodule Pika.IntegrationCoordinator do
  @moduledoc "Integration domain lifecycle coordinator; Agent execution is delegated to Symphony."

  use GenServer

  alias Pika.Agent.{Actor, Directory, Symphony}
  alias Pika.Agent.Roles.Integration.Domain
  alias Pika.{AttemptStore, IntegrationStore, IntegrationWorkspace, Repo}

  def start_link(opts \\ []) do
    case Keyword.get(opts, :name, __MODULE__) do
      nil -> GenServer.start_link(__MODULE__, opts)
      name -> GenServer.start_link(__MODULE__, opts, name: name)
    end
  end

  def snapshot(server \\ __MODULE__), do: call(server, :snapshot)
  def stop_now(server \\ __MODULE__), do: call(server, :stop_now)
  def resume(server \\ __MODULE__), do: call(server, :resume)

  def authorize(token, server \\ __MODULE__) do
    case Directory.authorize(token) do
      :ok -> :ok
      _ -> call(server, {:authorize, token})
    end
  end

  def mcp_call(token, tool, args, server \\ __MODULE__) do
    case Directory.lookup(token) do
      {:ok, %{role_id: "integration", actor: actor}} -> legacy_actor_call(actor, tool, args)
      _ -> call(server, {:mcp, token, tool, args})
    end
  end

  @impl true
  def init(opts) do
    workspace = Keyword.get_lazy(opts, :workspace, &Pika.WorkspaceLock.workspace/0)
    campaign = Keyword.get_lazy(opts, :campaign, &Pika.Persistence.current_campaign/0)

    subscribe(campaign.id)

    state = %{
      workspace: workspace,
      campaign_id: campaign.id,
      recovery_enabled: campaign.status not in ~w(stopped blocked completed),
      directory: Keyword.get(opts, :directory, Directory),
      symphony: Keyword.get(opts, :symphony),
      owns_symphony: false,
      actor_supervisor: Keyword.get(opts, :actor_supervisor, Pika.Agent.ActorSupervisor),
      actor_opts: actor_opts(opts),
      last_error: nil
    }

    state =
      state
      |> repair_legacy_target_gate_rejections()
      |> interrupt_orphaned_sessions()
      |> cleanup_terminal_worktrees()

    with {:ok, state} <- ensure_symphony(state) do
      send(self(), :scan)
      {:ok, state}
    end
  end

  @impl true
  def handle_call(:snapshot, _from, state) do
    {:reply,
     %{
       queue_head: optional(&IntegrationStore.queue_head/1, state.campaign_id),
       lease: optional(&IntegrationStore.lease/1, state.campaign_id),
       session: active_actor_status(state),
       recovery_count: recovery_count(state.campaign_id),
       last_error: state.last_error
     }, state}
  end

  def handle_call(:stop_now, _from, state) do
    stop_active_actors(state)
    {:reply, :ok, %{state | recovery_enabled: false}}
  end

  def handle_call(:resume, _from, state) do
    send(self(), :scan)
    {:reply, :ok, %{state | recovery_enabled: true}}
  end

  def handle_call({:authorize, token}, _from, state) do
    result =
      case Directory.lookup(token, state.directory) do
        {:ok, %{role_id: "integration"}} -> :ok
        _ -> {:error, :unauthorized}
      end

    {:reply, result, state}
  end

  def handle_call({:mcp, token, tool, args}, _from, state) do
    response =
      case Directory.lookup(token, state.directory) do
        {:ok, %{role_id: "integration", actor: actor}} -> legacy_actor_call(actor, tool, args)
        _ -> {:error, "unauthorized", "invalid Integration Session token", %{}}
      end

    {:reply, response, state}
  end

  @impl true
  def handle_info(:scan, %{recovery_enabled: true} = state), do: {:noreply, scan(state)}
  def handle_info(:scan, state), do: {:noreply, state}

  def handle_info(
        {event_kind, %{event_type: "integration_blocked", payload: payload}},
        state
      )
      when event_kind in [:domain_event, :integration_event] do
    {:noreply,
     %{
       state
       | recovery_enabled: false,
         last_error: Map.get(payload, :reason) || Map.get(payload, "reason")
     }}
  end

  def handle_info({:domain_event, _event}, state), do: {:noreply, trigger_scan(state)}
  def handle_info({:integration_event, _event}, state), do: {:noreply, trigger_scan(state)}

  def handle_info({event, _work, _details}, state)
      when event in [
             :agent_actor_completed,
             :agent_actor_skipped,
             :agent_actor_failed,
             :agent_actor_interrupted,
             :agent_actor_stopped
           ],
      do: {:noreply, trigger_scan(state)}

  def handle_info(_message, state), do: {:noreply, state}

  @impl true
  def terminate(_reason, state) do
    stop_active_actors(state)

    if state.owns_symphony and is_pid(state.symphony) and Process.alive?(state.symphony),
      do: GenServer.stop(state.symphony, :normal)

    :ok
  end

  defp scan(state) do
    if Pika.Persistence.current_campaign().status in ~w(stopped blocked completed) do
      state
    else
      case IntegrationStore.queue_head(state.campaign_id) do
        {:ok, attempt} ->
          case Domain.ensure_recoverable_best(state.workspace, attempt) do
            :ok ->
              reconcile_actor(%{state | last_error: nil})

            {:error, reason} ->
              _ = IntegrationStore.block(state.campaign_id, attempt.id, reason)
              %{state | recovery_enabled: false, last_error: inspect(reason)}
          end

        {:error, :integration_queue_empty} ->
          reconcile_actor(%{state | last_error: nil})

        {:error, reason} ->
          %{state | last_error: inspect(reason)}
      end
    end
  end

  defp reconcile_actor(state) do
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
          sources: [Pika.Agent.WorkSources.Integration],
          role_owners: %{"integration" => :actor},
          actor_opts: state.actor_opts,
          capacity: 1,
          reconcile_interval_ms: 250
        ]

        case Symphony.start_link(opts) do
          {:ok, pid} -> {:ok, %{state | symphony: pid, owns_symphony: true}}
          {:error, reason} -> {:stop, {:integration_symphony_start_failed, reason}}
        end
    end
  end

  defp actor_opts(opts) do
    base = [
      backend_modules: Keyword.get(opts, :backend_modules, %{}),
      max_followups: 50
    ]

    base =
      case Keyword.fetch(opts, :mcp_url) do
        {:ok, mcp_url} -> Keyword.put(base, :mcp_url, mcp_url)
        :error -> base
      end

    case Keyword.fetch(opts, :profile) do
      {:ok, profile} -> Keyword.put(base, :profile, profile)
      :error -> base
    end
  end

  defp active_actor_status(state) do
    state.symphony
    |> Symphony.active()
    |> Enum.find_value(fn
      {%{role_id: "integration", campaign_id: campaign_id}, actor}
      when campaign_id == state.campaign_id ->
        Actor.status(actor)

      _ ->
        nil
    end)
  catch
    :exit, _reason -> nil
  end

  defp stop_active_actors(state) do
    state.symphony
    |> Symphony.active()
    |> Enum.each(fn
      {%{role_id: "integration", campaign_id: campaign_id}, actor}
      when campaign_id == state.campaign_id ->
        Actor.stop(actor)

      _ ->
        :ok
    end)
  catch
    :exit, _reason -> :ok
  end

  defp recovery_count(campaign_id) do
    case Repo.query!(
           "SELECT COUNT(*) FROM agent_sessions WHERE campaign_id = ? AND role = 'integration' AND session_mode = 'recovering'",
           [campaign_id]
         ).rows do
      [[count]] -> count
      _ -> 0
    end
  rescue
    _error -> 0
  end

  defp cleanup_terminal_worktrees(state) do
    AttemptStore.attempts(state.campaign_id, limit: 10_000)
    |> Enum.filter(&(&1.status in ~w(accepted rejected cancelled)))
    |> Enum.each(&IntegrationWorkspace.cleanup(state.workspace, &1))

    state
  end

  defp interrupt_orphaned_sessions(state) do
    _ = AttemptStore.interrupt_active_sessions_for_role(state.campaign_id, :integration)
    state
  end

  defp repair_legacy_target_gate_rejections(state) do
    case IntegrationStore.repair_legacy_target_gate_rejections(state.campaign_id) do
      :ok -> state
      {:error, reason} -> %{state | last_error: inspect(reason)}
    end
  end

  defp trigger_scan(%{recovery_enabled: true} = state) do
    send(self(), :scan)
    state
  end

  defp trigger_scan(state), do: state

  defp legacy_actor_call(actor, tool, args) do
    case Actor.invoke(actor, tool, args) do
      {:ok, outcome} ->
        {:ok, legacy_value(outcome.value)}

      {:error, %Pika.Agent.Role.Error{} = error} ->
        {:error, Atom.to_string(error.code), error.message, error.details}

      {:error, reason} ->
        {:error, "role_operation_failed", inspect(reason), %{}}
    end
  catch
    :exit, reason -> {:error, "actor_unavailable", inspect(reason), %{}}
  end

  defp legacy_value(%_{} = struct), do: struct

  defp legacy_value(map) when is_map(map) do
    Map.new(map, fn {key, value} -> {legacy_key(key), value} end)
  end

  defp legacy_value(value), do: value

  defp legacy_key(key) when is_binary(key) do
    String.to_existing_atom(key)
  rescue
    ArgumentError -> key
  end

  defp legacy_key(key), do: key

  defp subscribe(campaign_id) do
    if Process.whereis(Pika.PubSub) do
      Phoenix.PubSub.subscribe(Pika.PubSub, Pika.Persistence.topic(campaign_id))
      Phoenix.PubSub.subscribe(Pika.PubSub, "pika:optimization:events")
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
