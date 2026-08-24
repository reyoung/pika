defmodule Pika.Optimization.Runtime do
  @moduledoc "Supervises the singleton v2 Optimization state without owning child lifecycle rules."

  use GenServer

  alias Pika.Optimization.Persistence
  alias Pika.Repo

  @optimization_id "optimization"
  @terminal ~w(completed stopped failed)

  def start_link(opts \\ []) do
    name = Keyword.get(opts, :name, __MODULE__)
    GenServer.start_link(__MODULE__, :ok, if(name, do: [name: name], else: []))
  end

  def snapshot(server \\ __MODULE__), do: GenServer.call(server, :snapshot)
  def pause(server \\ __MODULE__), do: GenServer.call(server, :pause)
  def resume(server \\ __MODULE__), do: GenServer.call(server, :resume)
  def drain(server \\ __MODULE__, reason), do: GenServer.call(server, {:drain, reason})
  def stop_now(server \\ __MODULE__, reason), do: GenServer.call(server, {:stop_now, reason})
  def fail(server \\ __MODULE__, reason), do: GenServer.call(server, {:fail, reason})
  def complete_if_drained(server \\ __MODULE__), do: GenServer.call(server, :complete_if_drained)

  @impl true
  def init(:ok) do
    case Persistence.current() do
      nil -> {:stop, :optimization_not_initialized}
      optimization -> {:ok, %{optimization: optimization}}
    end
  end

  @impl true
  def handle_call(:snapshot, _from, state) do
    optimization = Persistence.current()
    {:reply, optimization, %{state | optimization: optimization}}
  end

  def handle_call(:pause, _from, state) do
    transition(state, fn optimization ->
      if optimization.status in ["optimizing", "draining"] do
        {:ok, "paused", optimization.status, nil, "optimization_paused",
         %{from: optimization.status}}
      else
        {:error, {:cannot_pause, optimization.status}}
      end
    end)
  end

  def handle_call(:resume, _from, state) do
    transition(state, fn optimization ->
      if optimization.status == "paused" and
           optimization.resume_status in ["optimizing", "draining"] do
        {:ok, optimization.resume_status, nil, nil, "optimization_resumed",
         %{status: optimization.resume_status}}
      else
        {:error, {:cannot_resume, optimization.status, optimization.resume_status}}
      end
    end)
  end

  def handle_call({:drain, reason}, _from, state) do
    transition(state, fn optimization ->
      if optimization.status == "optimizing" do
        {:ok, "draining", nil, to_string(reason), "optimization_draining",
         %{reason: to_string(reason)}}
      else
        {:error, {:cannot_drain, optimization.status}}
      end
    end)
  end

  def handle_call({:stop_now, reason}, _from, state) do
    transition(state, fn optimization ->
      if optimization.status in @terminal do
        {:error, {:already_terminal, optimization.status}}
      else
        {:ok, "stopped", nil, to_string(reason), "optimization_stopped",
         %{reason: to_string(reason)}}
      end
    end)
  end

  def handle_call({:fail, reason}, _from, state) do
    transition(state, fn optimization ->
      if optimization.status in @terminal do
        {:error, {:already_terminal, optimization.status}}
      else
        {:ok, "failed", nil, to_string(reason), "optimization_failed",
         %{reason: to_string(reason)}}
      end
    end)
  end

  def handle_call(:complete_if_drained, _from, state) do
    transition(state, fn optimization ->
      cond do
        optimization.status != "draining" ->
          {:error, {:not_draining, optimization.status}}

        active_work?() ->
          {:error, :active_work_remains}

        true ->
          {:ok, "completed", nil, optimization.stop_reason, "optimization_completed", %{}}
      end
    end)
  end

  defp transition(state, decision) do
    optimization = Persistence.current()

    case decision.(optimization) do
      {:error, reason} ->
        {:reply, {:error, reason}, %{state | optimization: optimization}}

      {:ok, status, resume_status, stop_reason, event_type, payload} ->
        case persist_transition(status, resume_status, stop_reason, event_type, payload) do
          {:ok, updated} -> {:reply, {:ok, updated}, %{state | optimization: updated}}
          {:error, reason} -> {:reply, {:error, reason}, %{state | optimization: optimization}}
        end
    end
  end

  defp persist_transition(status, resume_status, stop_reason, event_type, payload) do
    now = System.system_time(:microsecond)

    case Repo.transaction(fn ->
           Repo.query!(
             """
             UPDATE optimizations
             SET status = ?, resume_status = ?, stop_reason = ?, updated_at = ?
             WHERE id = ?
             """,
             [status, resume_status, stop_reason, now, @optimization_id]
           )

           if status == "stopped" do
             Repo.query!(
               """
               UPDATE attempts
               SET status = 'cancelled', outcome = 'rejected', failure_reason = ?, updated_at = ?
               WHERE optimization_id = ? AND status NOT IN ('accepted', 'rejected', 'cancelled')
               """,
               [stop_reason || "stop_now", now, @optimization_id]
             )

             Repo.query!(
               """
               UPDATE integration_runs
               SET status = 'failed', outcome = 'rejected', updated_at = ?
               WHERE optimization_id = ? AND status NOT IN ('accepted', 'rejected', 'failed')
               """,
               [now, @optimization_id]
             )

             Repo.query!(
               "UPDATE operation_intents SET state = 'aborted', updated_at = ? WHERE optimization_id = ? AND state = 'pending'",
               [now, @optimization_id]
             )

             Repo.query!(
               """
               UPDATE agent_sessions
               SET status = 'interrupted', ended_reason = 'stop_now', ended_at = ?
               WHERE optimization_id = ? AND status IN ('running', 'awaiting_report', 'awaiting_followup')
               """,
               [now, @optimization_id]
             )
           end

           Repo.query!(
             """
             INSERT INTO domain_events(
               event_id, optimization_id, aggregate_type, aggregate_id,
               event_type, payload_json, created_at
             ) VALUES (?, ?, 'optimization', ?, ?, ?, ?)
             """,
             [
               Ecto.UUID.generate(),
               @optimization_id,
               @optimization_id,
               event_type,
               Jason.encode!(payload),
               now
             ]
           )

           Persistence.current()
         end) do
      {:ok, updated} -> {:ok, updated}
      {:error, reason} -> {:error, {:optimization_transition_failed, reason}}
    end
  end

  defp active_work? do
    [[attempts]] =
      Repo.query!("""
      SELECT COUNT(*) FROM attempts
      WHERE status NOT IN ('accepted', 'rejected', 'cancelled')
      """).rows

    [[integrations]] =
      Repo.query!("""
      SELECT COUNT(*) FROM integration_runs
      WHERE status NOT IN ('accepted', 'rejected', 'failed')
      """).rows

    attempts > 0 or integrations > 0
  end
end
