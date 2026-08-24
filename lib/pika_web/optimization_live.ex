defmodule PikaWeb.OptimizationLive do
  use PikaWeb, :live_view

  alias Pika.Agent.{ConversationJournal, Symphony}
  alias Pika.Attempt.Scheduler
  alias Pika.Baseline.Lifecycle, as: BaselineLifecycle
  alias Pika.Baseline.Questions
  alias Pika.Optimization.{Persistence, Runtime}
  alias Pika.ProgressSummary.Lifecycle, as: ProgressLifecycle
  alias Pika.ProgressSummary.Snapshot
  alias Pika.Repo

  @refresh_ms 1_000

  @impl true
  def mount(_params, session, socket) do
    if Pika.Auth.authenticated_marker?(session["pika_auth"]) do
      if connected?(socket), do: Process.send_after(self(), :refresh, @refresh_ms)

      {:ok,
       socket
       |> assign(:message_form, to_form(%{"body" => ""}, as: :message))
       |> assign(:review_form, to_form(%{"feedback" => ""}, as: :review))
       |> assign(:guidance_form, to_form(%{"body" => ""}, as: :guidance))
       |> refresh()}
    else
      {:ok, redirect(socket, to: "/")}
    end
  end

  @impl true
  def handle_info(:refresh, socket) do
    Process.send_after(self(), :refresh, @refresh_ms)
    {:noreply, refresh(socket)}
  end

  @impl true
  def handle_event("send_message", %{"message" => %{"body" => body}}, socket) do
    body = String.trim(body)

    result =
      with false <- body == "",
           %{id: baseline_id} <- BaselineLifecycle.latest_revision(),
           :ok <- Symphony.kickoff("baseline_alignment", to_string(baseline_id), body) do
        :ok
      else
        true -> {:error, :message_required}
        nil -> {:error, :baseline_missing}
        {:error, _reason} = error -> error
      end

    {:noreply,
     socket
     |> put_result(result, "消息已发送给 Baseline Alignment Agent。")
     |> assign(:message_form, to_form(%{"body" => ""}, as: :message))
     |> refresh()}
  end

  def handle_event("answer_questions", %{"answers" => answers}, socket) do
    result =
      with batch when is_map(batch) <- Questions.pending(),
           values <-
             Enum.map(batch.questions, fn question ->
               %{"id" => question["id"], "answer" => answers[question["id"]] || ""}
             end),
           {:ok, _batch} <- Questions.answer(batch.id, values) do
        :ok
      else
        nil -> {:error, :question_batch_missing}
        {:error, _reason} = error -> error
      end

    {:noreply, socket |> put_result(result, "问题答案已返回 Agent。") |> refresh()}
  end

  def handle_event("approve_baseline", _params, socket) do
    result = review(:approve, nil)
    if result == :ok, do: Symphony.reconcile()
    {:noreply, socket |> put_result(result, "Baseline Review 已通过，开始全量验证。") |> refresh()}
  end

  def handle_event("request_changes", %{"review" => %{"feedback" => feedback}}, socket) do
    feedback = String.trim(feedback)

    result =
      if feedback == "",
        do: {:error, :review_feedback_required},
        else: review(:request_changes, feedback)

    if result == :ok, do: Symphony.reconcile()
    {:noreply, socket |> put_result(result, "Baseline 已退回修改。") |> refresh()}
  end

  def handle_event("add_guidance", %{"guidance" => %{"body" => body}}, socket) do
    result = Scheduler.add_guidance(body)

    {:noreply,
     socket
     |> put_result(result, "新的 Iteration Guidance 将对之后创建的 Attempt 生效。")
     |> assign(:guidance_form, to_form(%{"body" => ""}, as: :guidance))
     |> refresh()}
  end

  def handle_event("pause", _params, socket), do: control(socket, &Runtime.pause/0, "已暂停新工作。")
  def handle_event("resume", _params, socket), do: control(socket, &Runtime.resume/0, "已恢复调度。")

  def handle_event("drain", _params, socket),
    do: control(socket, fn -> Runtime.drain("user_requested") end, "进入 Draining。")

  def handle_event("stop", _params, socket),
    do: control(socket, fn -> Runtime.stop_now("user_requested") end, "已停止 Optimization。")

  @impl true
  def render(assigns) do
    ~H"""
    <main class="control-shell">
      <header class="topbar">
        <div class="brand">
          <div class="brand-mark">P</div>
          <div><strong>Pika v2</strong><small>single Optimization · internal Best only · no remote writes</small></div>
        </div>
        <.pill kind={if @snapshot.optimization.status == "failed", do: "error", else: "active"}>
          {@snapshot.optimization.status}
        </.pill>
      </header>

      <p :if={@flash_message} class="diagnostic-note">{@flash_message}</p>

      <section class="panel diagnostic-hero">
        <div>
          <p class="eyebrow">Current Best</p>
          <h1><code>{short_sha(@snapshot.optimization.best_sha)}</code></h1>
          <p class="muted">{@workspace}</p>
        </div>
        <div class="control-actions">
          <button phx-click="pause">Pause</button>
          <button phx-click="resume">Resume</button>
          <button phx-click="drain">Drain</button>
          <button phx-click="stop" class="danger">Stop now</button>
        </div>
      </section>

      <div class="diagnostic-grid">
        <section class="panel">
          <div class="panel-heading"><div><p class="eyebrow">Baseline</p><h2>Alignment &amp; Review</h2></div></div>
          <dl class="sqlite-list" :if={@baseline}>
            <div><dt>Revision</dt><dd>{@baseline.revision}</dd></div>
            <div><dt>Status</dt><dd>{@baseline.status}</dd></div>
            <div><dt>Development</dt><dd><code>{short_sha(@baseline.development_sha)}</code></dd></div>
          </dl>

          <.form :if={@baseline && @baseline.status == "drafting"} for={@message_form} phx-submit="send_message">
            <label for={@message_form[:body].id}>给 Baseline Agent 的消息</label>
            <.input field={@message_form[:body]} type="textarea" />
            <button type="submit">发送</button>
          </.form>

          <div :if={@questions} class="question-batch">
            <h3>Agent 需要你的决定</h3>
            <form phx-submit="answer_questions">
              <fieldset :for={question <- @questions.questions}>
                <legend>{question["question"]}</legend>
                <label :for={option <- question["options"]}>
                  <input type="radio" name={"answers[#{question["id"]}]"} value={option["label"]} required />
                  <strong>{option["label"]}</strong> — {option["description"]}
                </label>
              </fieldset>
              <button type="submit">提交整批答案</button>
            </form>
          </div>

          <div :if={@baseline && @baseline.status == "awaiting_review"}>
            <p>Definition 已冻结，只有用户 Review 可以推进到 Baseline Verify。</p>
            <details open>
              <summary>Baseline Definition</summary>
              <pre>{@review_bundle.definition}</pre>
            </details>
            <details>
              <summary>Smoke Verify</summary>
              <pre>{@review_bundle.smoke_verify}</pre>
            </details>
            <details>
              <summary>Smoke Benchmark</summary>
              <pre>{@review_bundle.smoke_benchmark}</pre>
            </details>
            <button phx-click="approve_baseline">Review 通过</button>
            <.form for={@review_form} phx-submit="request_changes">
              <label for={@review_form[:feedback].id}>退回原因</label>
              <.input field={@review_form[:feedback]} type="textarea" />
              <button type="submit">退回修改</button>
            </.form>
          </div>
        </section>

        <section class="panel">
          <div class="panel-heading"><div><p class="eyebrow">User-friendly</p><h2>Latest Progress Summary</h2></div></div>
          <pre class="diagnostic-note">{@summary || "尚无 Progress Summary。"}</pre>
          <.form for={@guidance_form} phx-submit="add_guidance">
            <label for={@guidance_form[:body].id}>注入之后的 Iteration Guidance</label>
            <.input field={@guidance_form[:body]} type="textarea" />
            <button type="submit">保存 Guidance</button>
          </.form>
        </section>
      </div>

      <section class="panel">
        <div class="panel-heading"><div><p class="eyebrow">Attempts</p><h2>Optimization history</h2></div></div>
        <table>
          <thead><tr><th>ID</th><th>Status</th><th>Round</th><th>Base</th><th>Summary / reason</th></tr></thead>
          <tbody>
            <tr :for={attempt <- @snapshot.attempts}>
              <td>Attempt {attempt.id}</td>
              <td>{attempt.status}</td>
              <td>{attempt.iteration_round}</td>
              <td><code>{short_sha(attempt.base_sha)}</code></td>
              <td>{attempt.summary || attempt.failure_reason || "—"}</td>
            </tr>
          </tbody>
        </table>
      </section>

      <section class="panel">
        <div class="panel-heading"><div><p class="eyebrow">Conversation</p><h2>Normalized Turns</h2></div></div>
        <article :for={turn <- @turns} class="diagnostic-row">
          <div>
            <strong>{turn.session_sequence}.{turn.turn}</strong>
            <small>{messages_text(turn.input_messages)}{messages_text(turn.output_messages)}</small>
          </div>
          <span class="scope-label">{turn.ended_reason || "running"}</span>
        </article>
      </section>
    </main>
    """
  end

  defp refresh(socket) do
    cursor =
      Repo.query!("SELECT COALESCE(MAX(id), 0) FROM conversation_turns").rows |> hd() |> hd()

    snapshot = Snapshot.build(cursor, DateTime.utc_now())
    baseline = BaselineLifecycle.latest_revision()
    questions = if Process.whereis(Questions), do: Questions.pending(), else: nil

    turns =
      if baseline do
        ConversationJournal.work_turns(
          if(baseline.status == "verifying", do: "baseline_verify", else: "baseline_alignment"),
          "baseline_revision",
          to_string(baseline.id)
        )
      else
        []
      end

    socket
    |> assign(:snapshot, snapshot)
    |> assign(:baseline, baseline)
    |> assign(:questions, questions)
    |> assign(:turns, turns)
    |> assign(:review_bundle, review_bundle(baseline))
    |> assign(:summary, latest_summary())
    |> assign(:workspace, Persistence.current().workspace_canonical_path)
    |> assign_new(:flash_message, fn -> nil end)
  end

  defp review_bundle(%{status: "awaiting_review"} = baseline) do
    root =
      Path.join(Persistence.current().workspace_canonical_path, baseline.work_relative_path)

    definition = read_json_pretty(Path.join(root, "baseline-definition.json"))

    with {:ok, definition_value} <- Jason.decode(definition) do
      %{
        definition: definition,
        smoke_verify: read_json_pretty(Path.join(root, definition_value["smoke_verify_path"])),
        smoke_benchmark: read_text(Path.join(root, definition_value["smoke_benchmark_path"]))
      }
    else
      _error ->
        %{definition: definition, smoke_verify: "unavailable", smoke_benchmark: "unavailable"}
    end
  end

  defp review_bundle(_baseline),
    do: %{definition: "", smoke_verify: "", smoke_benchmark: ""}

  defp read_json_pretty(path) do
    with {:ok, contents} <- File.read(path),
         {:ok, value} <- Jason.decode(contents) do
      Jason.encode!(value, pretty: true)
    else
      _error -> "unavailable"
    end
  end

  defp read_text(path) do
    case File.read(path) do
      {:ok, contents} -> contents
      {:error, _reason} -> "unavailable"
    end
  end

  defp latest_summary do
    case ProgressLifecycle.latest() do
      nil ->
        nil

      summary ->
        path =
          Path.join(Persistence.current().workspace_canonical_path, summary.summary_relative_path)

        case File.read(path) do
          {:ok, contents} -> contents
          {:error, _reason} -> nil
        end
    end
  end

  defp review(decision, feedback) do
    with %{status: "awaiting_review"} = baseline <- BaselineLifecycle.latest_revision(),
         workspace <- Persistence.current().workspace_canonical_path,
         root <- Path.join(workspace, baseline.work_relative_path),
         {:ok, _updated} <-
           BaselineLifecycle.review(
             baseline.revision,
             decision,
             root,
             "baseline-definition.json",
             feedback
           ) do
      :ok
    else
      nil -> {:error, :baseline_missing}
      baseline when is_map(baseline) -> {:error, {:baseline_not_awaiting_review, baseline.status}}
      {:error, _reason} = error -> error
    end
  end

  defp control(socket, callback, success) do
    result = callback.()
    {:noreply, socket |> put_result(result, success) |> refresh()}
  end

  defp put_result(socket, :ok, success), do: assign(socket, :flash_message, success)
  defp put_result(socket, {:ok, _value}, success), do: assign(socket, :flash_message, success)

  defp put_result(socket, {:error, reason}, _success),
    do: assign(socket, :flash_message, inspect(reason))

  defp messages_text(messages) do
    Enum.map_join(messages, "\n", fn message -> message["content"] || inspect(message) end)
  end

  defp short_sha(nil), do: "—"
  defp short_sha(value) when byte_size(value) > 12, do: String.slice(value, 0, 12)
  defp short_sha(value), do: value
end
