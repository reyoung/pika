defmodule Pika.Agent.Symphony do
  @moduledoc "Reconciles domain-approved Agent Work into one ephemeral Actor per Work."

  use GenServer
  alias Pika.Agent.{Actor, Directory, RoleOwnership}
  alias Pika.Agent.Role.Work
  alias Pika.Repo

  @default_sources [
    Pika.Agent.WorkSources.AgentFollowup,
    Pika.Agent.WorkSources.ProgressSummary,
    Pika.Agent.WorkSources.Sync,
    Pika.Agent.WorkSources.Integration,
    Pika.Agent.WorkSources.Plan,
    Pika.Agent.WorkSources.Iteration,
    Pika.Agent.WorkSources.Alignment
  ]

  def start_link(opts \\ []) do
    case Keyword.get(opts, :name, __MODULE__) do
      nil -> GenServer.start_link(__MODULE__, opts)
      name -> GenServer.start_link(__MODULE__, opts, name: name)
    end
  end

  def reconcile(server \\ __MODULE__), do: GenServer.call(server, :reconcile, :infinity)
  def active(server \\ __MODULE__), do: GenServer.call(server, :active)

  @impl true
  def init(opts) do
    workspace = Keyword.get_lazy(opts, :workspace, &Pika.WorkspaceLock.workspace/0)

    campaign_id =
      Keyword.get_lazy(opts, :campaign_id, fn -> Pika.Persistence.current_campaign().id end)

    subscribe(campaign_id)

    state = %{
      workspace: workspace,
      campaign_id: campaign_id,
      directory: Keyword.get(opts, :directory, Directory),
      actor_supervisor: Keyword.get(opts, :actor_supervisor, Pika.Agent.ActorSupervisor),
      actor_module: Keyword.get(opts, :actor_module, Actor),
      actor_opts: Keyword.get(opts, :actor_opts, []),
      sources: Keyword.get(opts, :sources, @default_sources),
      role_owners: Keyword.get(opts, :role_owners, RoleOwnership.all()),
      capacity: Keyword.get(opts, :capacity, 32),
      interval: Keyword.get(opts, :reconcile_interval_ms, 1_000),
      timer: nil,
      actors: %{},
      errors: []
    }

    {:ok, schedule(state, 0)}
  end

  @impl true
  def handle_call(:reconcile, _from, state) do
    {:reply, :ok, do_reconcile(%{state | timer: nil}) |> schedule()}
  end

  def handle_call(:active, _from, state) do
    active =
      state.actors
      |> Map.values()
      |> Enum.filter(&Process.alive?(&1.pid))
      |> Enum.map(&{&1.work, &1.pid})
      |> Enum.sort_by(fn {work, _pid} -> {work.role_id, work.id} end)

    {:reply, active, state}
  end

  @impl true
  def handle_info(:reconcile, state),
    do: {:noreply, do_reconcile(%{state | timer: nil}) |> schedule()}

  def handle_info({:domain_event, _event}, state), do: {:noreply, trigger(state)}
  def handle_info({:optimization_event, _event}, state), do: {:noreply, trigger(state)}
  def handle_info({:campaign_updated, _snapshot}, state), do: {:noreply, trigger(state)}

  def handle_info({:DOWN, monitor, :process, _pid, reason}, state) do
    {removed, actors} = pop_monitor(state.actors, monitor)

    state =
      if removed do
        %{state | actors: actors, errors: record_exit(state.errors, removed.work, reason)}
      else
        state
      end

    {:noreply, trigger(state)}
  end

  def handle_info({event, _work, _details}, state)
      when event in [
             :agent_actor_completed,
             :agent_actor_skipped,
             :agent_actor_failed,
             :agent_actor_interrupted
           ],
      do: {:noreply, trigger(state)}

  def handle_info(_message, state), do: {:noreply, state}

  defp do_reconcile(state) do
    state = prune_dead(state)
    state = adopt_directory_actors(state)
    works = runnable_work(state)
    state = retire_ineligible(state, works)
    available = max(state.capacity - map_size(state.actors), 0)

    works
    |> Enum.reject(&active?(&1, state))
    |> Enum.take(available)
    |> Enum.reduce(state, &start_actor(&1, &2))
  rescue
    error -> %{state | errors: [Exception.message(error) | Enum.take(state.errors, 19)]}
  end

  defp adopt_directory_actors(state) do
    state.directory
    |> Directory.active()
    |> Enum.reduce(state, fn {work, pid}, acc ->
      key = work_key(work)

      if work.campaign_id == state.campaign_id and actor_owned?(work, state.role_owners) and
           Process.alive?(pid) and not Map.has_key?(acc.actors, key) do
        put_actor(acc, work, pid)
      else
        acc
      end
    end)
  catch
    :exit, _reason -> state
  end

  defp runnable_work(state) do
    state.sources
    |> Enum.flat_map(& &1.runnable_work(state.campaign_id, state.workspace))
    |> Enum.filter(&actor_owned?(&1, state.role_owners))
    |> Enum.uniq_by(&work_key/1)
    |> Enum.sort_by(&{&1.role_id, &1.id})
  end

  defp active?(work, state) do
    case Map.get(state.actors, work_key(work)) do
      %{pid: pid} -> Process.alive?(pid)
      nil -> match?({:ok, _pid}, Directory.lookup_work(work, state.directory))
    end
  end

  defp start_actor(%Work{} = work, state) do
    session_mode = session_mode(work)

    opts =
      Keyword.merge(state.actor_opts,
        work: work,
        workspace: state.workspace,
        session_mode: session_mode,
        directory: state.directory,
        notify: self()
      )

    case DynamicSupervisor.start_child(state.actor_supervisor, {state.actor_module, opts}) do
      {:ok, pid} ->
        put_actor(state, work, pid)

      {:error, {:already_started, pid}} ->
        put_actor(state, work, pid)

      {:error, reason} ->
        %{state | errors: [{work, reason} | Enum.take(state.errors, 19)]}
    end
  end

  defp put_actor(state, work, pid) do
    monitor = Process.monitor(pid)
    entry = %{work: work, pid: pid, monitor: monitor}
    %{state | actors: Map.put(state.actors, work_key(work), entry)}
  end

  defp session_mode(work) do
    case Repo.query!(
           "SELECT 1 FROM agent_sessions WHERE campaign_id = ? AND role = ? AND work_kind = ? AND work_id = ? LIMIT 1",
           [work.campaign_id, work.role_id, Atom.to_string(work.kind), work.id]
         ).rows do
      [] -> if(legacy_alignment_session?(work), do: :recovering, else: :fresh)
      _ -> :recovering
    end
  end

  defp legacy_alignment_session?(%Work{role_id: role_id, campaign_id: campaign_id})
       when role_id in ~w(alignment setup_merge baseline) do
    Repo.query!(
      "SELECT 1 FROM agent_sessions WHERE campaign_id = ? AND role = 'boundary' LIMIT 1",
      [campaign_id]
    ).rows != []
  end

  defp legacy_alignment_session?(_work), do: false

  defp prune_dead(state) do
    actors = Map.reject(state.actors, fn {_key, entry} -> not Process.alive?(entry.pid) end)
    %{state | actors: actors}
  end

  defp retire_ineligible(state, works) do
    unless campaign_paused?(state.campaign_id) do
      desired = MapSet.new(Enum.map(works, &work_key/1))

      Enum.each(state.actors, fn {key, entry} ->
        if Process.alive?(entry.pid) and not MapSet.member?(desired, key) do
          case refresh_actor(entry.pid, state.actor_module) do
            {:ok, %{state: {:terminal, _outcome}}} -> :ok
            _other -> stop_actor(entry.pid, state.actor_module)
          end
        end
      end)
    end

    state
  end

  # Pause is a dispatch gate, not a cancellation signal. Existing Actors retain their
  # Backend Sessions and in-flight turns until the campaign is resumed or explicitly stopped.
  defp campaign_paused?(campaign_id) do
    case Repo.query!("SELECT status FROM campaigns WHERE id = ?", [campaign_id]).rows do
      [["paused"]] -> true
      _ -> false
    end
  end

  defp refresh_actor(pid, module) do
    if function_exported?(module, :refresh, 1),
      do: module.refresh(pid),
      else: {:error, :unsupported}
  catch
    :exit, _reason -> {:error, :actor_unavailable}
  end

  defp stop_actor(pid, module) do
    if function_exported?(module, :stop, 1),
      do: module.stop(pid),
      else: Process.exit(pid, :shutdown)
  catch
    :exit, _reason -> :ok
  end

  defp pop_monitor(actors, monitor) do
    case Enum.find(actors, fn {_key, entry} -> entry.monitor == monitor end) do
      {key, entry} -> {entry, Map.delete(actors, key)}
      nil -> {nil, actors}
    end
  end

  defp actor_owned?(work, owners) do
    owner =
      Map.get(owners, work.role_id) ||
        Enum.find_value(owners, :legacy, fn {key, value} ->
          if is_atom(key) and Atom.to_string(key) == work.role_id, do: value
        end)

    owner in [:actor, "actor"]
  end

  defp trigger(state) do
    if state.timer, do: Process.cancel_timer(state.timer)
    %{state | timer: Process.send_after(self(), :reconcile, 50)}
  end

  defp schedule(state, delay \\ nil)
  defp schedule(%{interval: :infinity} = state, _delay), do: state

  defp schedule(state, delay) do
    if state.timer do
      state
    else
      timeout = if is_integer(delay), do: delay, else: state.interval
      %{state | timer: Process.send_after(self(), :reconcile, timeout)}
    end
  end

  defp subscribe(campaign_id) do
    if Process.whereis(Pika.PubSub) do
      Phoenix.PubSub.subscribe(Pika.PubSub, Pika.Persistence.topic(campaign_id))
      Phoenix.PubSub.subscribe(Pika.PubSub, "pika:optimization:#{campaign_id}")
      Phoenix.PubSub.subscribe(Pika.PubSub, Pika.Alignment.Campaign.topic())
    end
  end

  defp record_exit(errors, _work, :normal), do: errors
  defp record_exit(errors, work, reason), do: [{work, reason} | Enum.take(errors, 19)]
  defp work_key(work), do: {work.role_id, work.kind, work.id, work.campaign_id}
end
