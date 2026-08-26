defmodule Pika.Agent.Symphony do
  @moduledoc "Reconciles v2 domain Work into one Actor per Work with only per-Role concurrency."

  use GenServer

  require Logger

  alias Pika.Agent.{Actor, ConversationJournal, Directory, Work, WorkProjector}
  alias Pika.Baseline.Questions
  alias Pika.Optimization.{RoleRegistry, RuntimeConfig}
  alias Pika.Optimization.Persistence

  @manual_restart_message """
  这是用户触发的手动重启。请读取 recovery context，继续当前工作，不要重复已经完成的步骤。需要用户决定时，使用 ask_questions 一次批量提交当前所有相互独立的问题。
  """

  def start_link(opts \\ []) do
    case Keyword.get(opts, :name, __MODULE__) do
      nil -> GenServer.start_link(__MODULE__, opts)
      name -> GenServer.start_link(__MODULE__, opts, name: name)
    end
  end

  def reconcile(server \\ __MODULE__), do: GenServer.call(server, :reconcile, :infinity)
  def active(server \\ __MODULE__), do: GenServer.call(server, :active)

  def kickoff(role_id, work_id, message, server \\ __MODULE__) do
    GenServer.call(server, {:kickoff, role_id, work_id, message}, :infinity)
  end

  def restart_session(session_id, server \\ __MODULE__) when is_binary(session_id) do
    GenServer.call(server, {:restart_session, session_id}, :infinity)
  end

  @impl true
  def init(opts) do
    state = %{
      workspace: Keyword.fetch!(opts, :workspace),
      directory: Keyword.get(opts, :directory, Directory),
      actor_supervisor: Keyword.get(opts, :actor_supervisor, Pika.Agent.ActorSupervisor),
      actor_module: Keyword.get(opts, :actor_module, Actor),
      actor_opts: Keyword.get(opts, :actor_opts, []),
      interval: Keyword.get(opts, :reconcile_interval_ms, 1_000),
      timer: nil,
      actors: %{},
      errors: []
    }

    {:ok, schedule(state, 0)}
  end

  @impl true
  def handle_call(:reconcile, _from, state) do
    state = state |> Map.put(:timer, nil) |> do_reconcile() |> schedule()
    {:reply, :ok, state}
  end

  def handle_call(:active, _from, state) do
    active =
      state.actors
      |> Map.values()
      |> Enum.filter(&Process.alive?(&1.pid))
      |> Enum.map(&{&1.work, &1.pid})
      |> Enum.sort_by(fn {work, _pid} -> Work.key(work) end)

    {:reply, active, state}
  end

  def handle_call({:kickoff, role_id, work_id, message}, _from, state) do
    actor =
      state.actors
      |> Map.values()
      |> Enum.find_value(fn entry ->
        if entry.work.role_id == role_id and entry.work.id == work_id and
             Process.alive?(entry.pid),
           do: entry.pid
      end)

    reply =
      if actor, do: state.actor_module.kickoff(actor, message), else: {:error, :work_not_active}

    {:reply, reply, state}
  end

  def handle_call({:restart_session, session_id}, _from, state) do
    case find_session_actor(state, session_id) do
      {key, entry} ->
        Process.demonitor(entry.monitor, [:flush])
        :ok = cancel_pending_questions(session_id)
        :ok = stop_actor(state.actor_module, entry.pid)

        state =
          state
          |> Map.update!(:actors, &Map.delete(&1, key))
          |> do_reconcile()

        reply =
          case state.actors[key] do
            %{pid: pid} -> state.actor_module.kickoff(pid, String.trim(@manual_restart_message))
            nil -> {:error, :session_restart_not_runnable}
          end

        {:reply, reply, state}

      nil ->
        {:reply, {:error, :session_not_active}, state}
    end
  end

  @impl true
  def handle_info(:reconcile, state),
    do: {:noreply, state |> Map.put(:timer, nil) |> do_reconcile() |> schedule()}

  def handle_info({:DOWN, monitor, :process, _pid, reason}, state) do
    {removed, actors} = pop_monitor(state.actors, monitor)

    if removed && reason not in [:normal, :shutdown] do
      Logger.error(
        "Agent actor exited for #{inspect(Work.key(removed.work))}: #{inspect(reason)}"
      )
    end

    errors =
      if removed && reason not in [:normal, :shutdown],
        do: [{removed.work, reason} | Enum.take(state.errors, 19)],
        else: state.errors

    {:noreply, trigger(%{state | actors: actors, errors: errors})}
  end

  def handle_info({event, _work, _details}, state)
      when event in [
             :agent_actor_started,
             :agent_actor_completed,
             :agent_actor_failed,
             :agent_actor_interrupted,
             :followup_delivery_failed
           ],
      do: {:noreply, trigger(state)}

  def handle_info({event, _work}, state) when event == :agent_actor_completed,
    do: {:noreply, trigger(state)}

  def handle_info(_message, state), do: {:noreply, state}

  defp do_reconcile(state) do
    state = prune_dead(state)

    with {:ok, config} <- RuntimeConfig.load(state.workspace),
         {:ok, works} <- WorkProjector.reconcile(config) do
      state
      |> retire_ineligible(works)
      |> start_eligible(works, config)
      |> maybe_complete_draining()
    else
      {:error, reason} ->
        Logger.error("Agent reconciliation failed: #{inspect(reason)}")
        %{state | errors: [reason | Enum.take(state.errors, 19)]}
    end
  rescue
    error ->
      Logger.error("Agent reconciliation raised: #{Exception.format(:error, error, __STACKTRACE__)}")
      %{state | errors: [Exception.message(error) | Enum.take(state.errors, 19)]}
  end

  defp start_eligible(state, works, config) do
    Enum.reduce(works, state, fn work, state ->
      cond do
        active?(work, state) -> state
        role_active_count(work.role_id, state) >= role_concurrency(work.role_id, config) -> state
        true -> start_actor(work, state)
      end
    end)
  end

  defp start_actor(work, state) do
    opts =
      Keyword.merge(state.actor_opts,
        work: work,
        workspace: state.workspace,
        directory: state.directory,
        notify: self()
      )

    result =
      if is_nil(state.actor_supervisor) do
        state.actor_module.start_link(opts)
      else
        DynamicSupervisor.start_child(state.actor_supervisor, {state.actor_module, opts})
      end

    case result do
      {:ok, pid} -> put_actor(state, work, pid)
      {:error, {:already_started, pid}} -> put_actor(state, work, pid)
      {:error, reason} ->
        Logger.error("Agent actor failed to start for #{inspect(Work.key(work))}: #{inspect(reason)}")
        %{state | errors: [{work, reason} | Enum.take(state.errors, 19)]}
    end
  end

  defp retire_ineligible(state, works) do
    if Persistence.current().status == "paused" do
      state
    else
      desired = MapSet.new(Enum.map(works, &Work.key/1))

      Enum.each(state.actors, fn {key, entry} ->
        if Process.alive?(entry.pid) and not MapSet.member?(desired, key) do
          stop_actor(state.actor_module, entry.pid)
        end
      end)

      state
    end
  end

  defp active?(work, state) do
    case state.actors[Work.key(work)] do
      %{pid: pid} ->
        Process.alive?(pid)

      nil ->
        match?(
          {:ok, _pid},
          Directory.lookup_work(work.role_id, work.kind, work.id, state.directory)
        )
    end
  end

  defp role_active_count(role_id, state) do
    Enum.count(state.actors, fn {_key, entry} ->
      entry.work.role_id == role_id and Process.alive?(entry.pid)
    end)
  end

  defp role_concurrency(role_id, config) do
    with {:ok, definition} <- RoleRegistry.fetch(role_id),
         do: RoleRegistry.concurrency(definition, config)
  end

  defp maybe_complete_draining(state) do
    if Persistence.current().status == "draining" and Process.whereis(Pika.Optimization.Runtime) do
      _ = Pika.Optimization.Runtime.complete_if_drained()
    end

    state
  end

  defp put_actor(state, work, pid) do
    monitor = Process.monitor(pid)
    entry = %{work: work, pid: pid, monitor: monitor}
    %{state | actors: Map.put(state.actors, Work.key(work), entry)}
  end

  defp prune_dead(state) do
    %{
      state
      | actors: Map.reject(state.actors, fn {_key, entry} -> not Process.alive?(entry.pid) end)
    }
  end

  defp pop_monitor(actors, monitor) do
    case Enum.find(actors, fn {_key, entry} -> entry.monitor == monitor end) do
      {key, entry} -> {entry, Map.delete(actors, key)}
      nil -> {nil, actors}
    end
  end

  defp find_session_actor(state, session_id) do
    case ConversationJournal.session(session_id) do
      %{status: status, role: role, work_kind: work_kind, work_id: work_id}
      when status in ["running", "awaiting_report", "awaiting_followup"] ->
        Enum.find(state.actors, fn {_key, entry} ->
          entry.work.role_id == role and to_string(entry.work.kind) == work_kind and
            entry.work.id == work_id
        end)

      _other ->
        nil
    end
  end

  defp cancel_pending_questions(session_id) do
    if Process.whereis(Questions), do: Questions.cancel_session(session_id), else: :ok
  catch
    :exit, _reason -> :ok
  end

  defp stop_actor(module, pid) do
    if function_exported?(module, :stop, 1),
      do: module.stop(pid),
      else: Process.exit(pid, :shutdown)
  catch
    :exit, _reason -> :ok
  end

  defp trigger(state) do
    if state.timer, do: Process.cancel_timer(state.timer)
    %{state | timer: Process.send_after(self(), :reconcile, 25)}
  end

  defp schedule(state) do
    cond do
      state.interval == :infinity -> state
      state.timer -> state
      true -> %{state | timer: Process.send_after(self(), :reconcile, state.interval)}
    end
  end

  defp schedule(state, delay) do
    if state.interval == :infinity,
      do: state,
      else: %{state | timer: Process.send_after(self(), :reconcile, delay)}
  end
end
