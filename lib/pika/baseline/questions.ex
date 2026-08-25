defmodule Pika.Baseline.Questions do
  @moduledoc "Durable batch question bridge between Baseline Alignment Agent and the user UI."

  use GenServer

  alias Pika.Agent.SessionBinding
  alias Pika.Repo

  @optimization_id "optimization"

  def start_link(opts \\ []) do
    case Keyword.get(opts, :name, __MODULE__) do
      nil -> GenServer.start_link(__MODULE__, %{})
      name -> GenServer.start_link(__MODULE__, %{}, name: name)
    end
  end

  @spec ask(SessionBinding.t(), [map()], GenServer.server()) :: {:ok, [map()]} | {:error, term()}
  def ask(binding, questions, server \\ __MODULE__),
    do: GenServer.call(server, {:ask, binding, questions}, :infinity)

  def pending(server \\ __MODULE__), do: GenServer.call(server, :pending)

  def answer(batch_id, answers, server \\ __MODULE__),
    do: GenServer.call(server, {:answer, batch_id, answers})

  def cancel_session(session_id, server \\ __MODULE__) when is_binary(session_id),
    do: GenServer.call(server, {:cancel_session, session_id})

  @spec answered_for_revision?(pos_integer()) :: boolean()
  def answered_for_revision?(baseline_revision_id)
      when is_integer(baseline_revision_id) and baseline_revision_id > 0 do
    case Repo.query!(
           """
           SELECT 1 FROM baseline_question_batches
           WHERE optimization_id = ? AND baseline_revision_id = ? AND status = 'answered'
           LIMIT 1
           """,
           [@optimization_id, baseline_revision_id]
         ).rows do
      [[1]] -> true
      [] -> false
    end
  end

  @impl true
  def init(_state) do
    cancel_orphaned_pending()
    {:ok, %{waiters: %{}}}
  end

  @impl true
  def handle_call({:ask, binding, questions}, from, state) do
    with :ok <- require_alignment(binding),
         {:ok, baseline_revision_id} <- integer_id(binding.work_id),
         {:ok, batch} <- insert_batch(baseline_revision_id, binding.session_id, questions) do
      {:noreply, %{state | waiters: Map.put(state.waiters, batch.id, from)}}
    else
      {:error, reason} -> {:reply, {:error, reason}, state}
    end
  end

  def handle_call(:pending, _from, state), do: {:reply, latest_pending(), state}

  def handle_call({:answer, batch_id, answers}, _from, state) do
    with {:ok, batch} <- fetch_pending(batch_id),
         :ok <- validate_answers(batch.questions, answers),
         {:ok, completed} <- persist_answers(batch, answers) do
      case Map.pop(state.waiters, batch_id) do
        {nil, waiters} ->
          {:reply, {:ok, completed}, %{state | waiters: waiters}}

        {from, waiters} ->
          GenServer.reply(from, {:ok, answers})
          {:reply, {:ok, completed}, %{state | waiters: waiters}}
      end
    else
      {:error, reason} -> {:reply, {:error, reason}, state}
    end
  end

  def handle_call({:cancel_session, session_id}, _from, state) do
    case latest_pending() do
      %{session_id: ^session_id} = batch ->
        with {:ok, _cancelled} <- cancel_batch(batch, "session_restarted") do
          case Map.pop(state.waiters, batch.id) do
            {nil, waiters} ->
              {:reply, :ok, %{state | waiters: waiters}}

            {from, waiters} ->
              GenServer.reply(from, {:error, :baseline_questions_cancelled})
              {:reply, :ok, %{state | waiters: waiters}}
          end
        else
          {:error, reason} -> {:reply, {:error, reason}, state}
        end

      _other ->
        {:reply, :ok, state}
    end
  end

  defp insert_batch(baseline_revision_id, session_id, questions) do
    now = now_us()
    id = Ecto.UUID.generate()

    case Repo.transaction(fn ->
           if latest_pending() do
             Repo.rollback(:baseline_question_batch_already_pending)
           end

           Repo.query!(
             """
             INSERT INTO baseline_question_batches(
               id, optimization_id, baseline_revision_id, session_id, status,
               questions_json, created_at
             ) VALUES (?, ?, ?, ?, 'pending', ?, ?)
             """,
             [
               id,
               @optimization_id,
               baseline_revision_id,
               session_id,
               Jason.encode!(questions),
               now
             ]
           )

           append_event(id, "baseline_questions_asked", %{count: length(questions)}, now)
           fetch!(id)
         end) do
      {:ok, batch} -> {:ok, batch}
      {:error, reason} -> {:error, {:baseline_questions_failed, reason}}
    end
  end

  defp validate_answers(questions, answers) when is_list(answers) do
    by_id = Map.new(questions, &{&1["id"], &1})
    answer_ids = Enum.map(answers, & &1["id"])

    valid? =
      MapSet.new(answer_ids) == MapSet.new(Map.keys(by_id)) and
        Enum.uniq(answer_ids) == answer_ids and
        Enum.all?(answers, fn answer ->
          question = by_id[answer["id"]]
          labels = Enum.map(question["options"], & &1["label"])

          is_binary(answer["answer"]) and String.trim(answer["answer"]) != "" and
            (answer["answer"] in labels or answer["custom"] == true)
        end)

    if valid?, do: :ok, else: {:error, :invalid_baseline_question_answers}
  end

  defp validate_answers(_questions, _answers), do: {:error, :invalid_baseline_question_answers}

  defp persist_answers(batch, answers) do
    now = now_us()

    case Repo.transaction(fn ->
           Repo.query!(
             """
             UPDATE baseline_question_batches
             SET status = 'answered', answers_json = ?, answered_at = ?
             WHERE id = ? AND status = 'pending'
             """,
             [Jason.encode!(answers), now, batch.id]
           )

           append_event(batch.id, "baseline_questions_answered", %{count: length(answers)}, now)
           fetch!(batch.id)
         end) do
      {:ok, completed} -> {:ok, completed}
      {:error, reason} -> {:error, {:baseline_answers_failed, reason}}
    end
  end

  defp cancel_batch(batch, reason) do
    now = now_us()

    case Repo.transaction(fn ->
           Repo.query!(
             """
             UPDATE baseline_question_batches
             SET status = 'cancelled'
             WHERE id = ? AND status = 'pending'
             """,
             [batch.id]
           )

           append_event(batch.id, "baseline_questions_cancelled", %{reason: reason}, now)
           fetch!(batch.id)
         end) do
      {:ok, cancelled} -> {:ok, cancelled}
      {:error, error} -> {:error, {:baseline_questions_cancel_failed, error}}
    end
  end

  defp latest_pending do
    case Repo.query!(
           "SELECT id FROM baseline_question_batches WHERE optimization_id = ? AND status = 'pending' ORDER BY created_at LIMIT 1",
           [@optimization_id]
         ).rows do
      [[id]] -> fetch!(id)
      [] -> nil
    end
  end

  defp fetch_pending(id) do
    case fetch(id) do
      %{status: "pending"} = batch -> {:ok, batch}
      nil -> {:error, :baseline_question_batch_not_found}
      batch -> {:error, {:baseline_question_batch_not_pending, batch.status}}
    end
  end

  defp fetch!(id), do: fetch(id) || raise("Baseline Question Batch #{id} is missing")

  defp fetch(id) do
    case Repo.query!(
           """
           SELECT id, baseline_revision_id, session_id, status, questions_json,
                  answers_json, created_at, answered_at
           FROM baseline_question_batches WHERE id = ?
           """,
           [id]
         ).rows do
      [[id, revision_id, session_id, status, questions, answers, created_at, answered_at]] ->
        %{
          id: id,
          baseline_revision_id: revision_id,
          session_id: session_id,
          status: status,
          questions: Jason.decode!(questions),
          answers: answers && Jason.decode!(answers),
          created_at: created_at,
          answered_at: answered_at
        }

      [] ->
        nil
    end
  end

  defp cancel_orphaned_pending do
    Repo.query!(
      """
      UPDATE baseline_question_batches SET status = 'cancelled'
      WHERE optimization_id = ? AND status = 'pending'
        AND (session_id IS NULL OR session_id NOT IN (
          SELECT id FROM agent_sessions WHERE status IN ('running', 'awaiting_report')
        ))
      """,
      [@optimization_id]
    )

    :ok
  rescue
    _error -> :ok
  end

  defp require_alignment(%SessionBinding{role_id: "baseline_alignment"}), do: :ok
  defp require_alignment(_binding), do: {:error, :ask_questions_forbidden}

  defp integer_id(value) do
    case Integer.parse(value) do
      {id, ""} when id > 0 -> {:ok, id}
      _other -> {:error, {:invalid_baseline_revision_id, value}}
    end
  end

  defp append_event(batch_id, event_type, payload, now) do
    Repo.query!(
      """
      INSERT INTO domain_events(
        event_id, optimization_id, aggregate_type, aggregate_id,
        event_type, payload_json, created_at
      ) VALUES (?, ?, 'baseline_question_batch', ?, ?, ?, ?)
      """,
      [Ecto.UUID.generate(), @optimization_id, batch_id, event_type, Jason.encode!(payload), now]
    )
  end

  defp now_us, do: System.system_time(:microsecond)
end
