defmodule PikaWeb.OptimizationLive do
  use PikaWeb, :live_view

  alias Pika.Agent.{ConversationJournal, Symphony}
  alias Pika.Attempt.Scheduler
  alias Pika.Baseline.Lifecycle, as: BaselineLifecycle
  alias Pika.Baseline.Questions
  alias Pika.CommandConsole
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
       |> assign(:open_session_id, nil)
       |> assign(:command_console, nil)
       |> assign(:command_console_ref, nil)
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

  def handle_info(
        {:command_console_event, ref, record},
        %{assigns: %{command_console_ref: ref, command_console: console}} = socket
      )
      when is_map(console) do
    {:noreply, assign(socket, :command_console, CommandConsole.apply_record(console, record))}
  end

  def handle_info({:command_console_event, _ref, _record}, socket), do: {:noreply, socket}

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
     |> update_message_form(result, body)
     |> refresh()}
  end

  def handle_event("restart_agent_session", %{"session" => session_id}, socket) do
    result = Symphony.restart_session(session_id)

    {:noreply,
     socket
     |> put_result(result, "Agent Session 已中断并从 recovery context 重启。")
     |> assign(:open_session_id, nil)
     |> refresh()}
  end

  def handle_event("restart_agent_session", %{"session-id" => session_id}, socket),
    do: handle_event("restart_agent_session", %{"session" => session_id}, socket)

  def handle_event("toggle_agent_session", %{"session" => session_id}, socket) do
    open_session_id =
      if socket.assigns.open_session_id == session_id,
        do: nil,
        else: session_id

    {:noreply, assign(socket, :open_session_id, open_session_id)}
  end

  def handle_event("open_command_console", %{"ref" => ref}, socket) do
    case CommandConsole.load(ref) do
      {:ok, console} ->
        socket = switch_console_subscription(socket, ref)

        {:noreply,
         socket
         |> assign(:command_console_ref, ref)
         |> assign(:command_console, console)}

      {:error, reason} ->
        {:noreply, put_result(socket, {:error, reason}, "")}
    end
  end

  def handle_event("close_command_console", _params, socket) do
    socket = switch_console_subscription(socket, nil)

    {:noreply,
     socket
     |> assign(:command_console_ref, nil)
     |> assign(:command_console, nil)}
  end

  def handle_event("answer_questions", params, socket) do
    result =
      with batch when is_map(batch) <- Questions.pending(),
           values <- normalize_question_answers(batch.questions, Map.get(params, "answers", %{})),
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
    <main class="ops-shell">
      <header class="ops-header">
        <a class="ops-brand" href="/">
          <span class="ops-brand-mark">P</span>
          <span>
            <strong>Pika</strong>
            <small>Optimization Console</small>
          </span>
        </a>

        <div class="ops-header-context">
          <span>Workspace</span>
          <strong title={@workspace}>{Path.basename(@workspace)}</strong>
        </div>

        <.pill kind={status_kind(@snapshot.optimization.status)}>
          <span class="ops-status-dot"></span>
          {humanize_status(@snapshot.optimization.status)}
        </.pill>
      </header>

      <div class="ops-page">
        <div :if={@flash_message} class="ops-notice" role="status">
          <span>i</span>
          <p>{@flash_message}</p>
        </div>

        <section class="ops-hero">
          <div class="ops-hero-copy">
            <div class="ops-stage-kicker">
              <span class={"ops-stage-signal tone-#{status_kind(@snapshot.optimization.status)}"}></span>
              Live optimization
            </div>
            <h1>{stage_title(@snapshot.optimization.status)}</h1>
            <p>{stage_description(@snapshot.optimization.status)}</p>
            <div class="ops-hero-meta">
              <span>
                <small>Current Best</small>
                <code>{short_sha(@snapshot.optimization.best_sha)}</code>
              </span>
              <span>
                <small>Initial commit</small>
                <code>{short_sha(@snapshot.optimization.initial_sha)}</code>
              </span>
              <span>
                <small>Branch</small>
                <code>{@snapshot.optimization.best_branch}</code>
              </span>
            </div>
          </div>

          <div class="ops-actions">
            <span>Runtime controls</span>
            <div>
              <button
                :if={@snapshot.optimization.status in ["optimizing", "draining"]}
                phx-click="pause"
                class="ops-button ops-button-secondary"
              >
                Pause
              </button>
              <button
                :if={@snapshot.optimization.status == "paused"}
                phx-click="resume"
                class="ops-button ops-button-primary"
              >
                Resume
              </button>
              <button
                :if={@snapshot.optimization.status == "optimizing"}
                phx-click="drain"
                class="ops-button ops-button-secondary"
              >
                Stop dispatching
              </button>
              <button
                :if={not terminal_status?(@snapshot.optimization.status)}
                phx-click="stop"
                class="ops-button ops-button-danger"
              >
                Stop now
              </button>
              <span :if={terminal_status?(@snapshot.optimization.status)} class="ops-finished">
                This run is finished
              </span>
            </div>
            <small>Controls affect scheduling immediately. No remote writes are performed.</small>
          </div>
        </section>

        <section class="ops-stat-grid" aria-label="Optimization overview">
          <article class="ops-stat-card">
            <span>Baseline</span>
            <strong>{if @baseline, do: humanize_status(@baseline.status), else: "Not created"}</strong>
            <small>{if @baseline, do: "Revision #{@baseline.revision}", else: "Waiting for bootstrap"}</small>
          </article>
          <article class="ops-stat-card">
            <span>Attempts</span>
            <strong>{length(@snapshot.attempts)}</strong>
            <small>{attempt_activity(@snapshot.attempts)}</small>
          </article>
          <article class="ops-stat-card">
            <span>Active agents</span>
            <strong>{length(@snapshot.active_agent_sessions)}</strong>
            <small>{agent_activity(@snapshot.active_agent_sessions)}</small>
          </article>
          <article class="ops-stat-card">
            <span>Follow-ups</span>
            <strong>{length(@snapshot.active_followups)}</strong>
            <small>{followup_activity(@snapshot.active_followups)}</small>
          </article>
        </section>

        <div class="ops-primary-grid">
          <section class="ops-panel ops-baseline-panel">
            <header class="ops-panel-header">
              <div>
                <p class="ops-eyebrow">Baseline workspace</p>
                <h2>Alignment &amp; Review</h2>
                <p>Define what matters, answer blocking questions, and approve the measurement contract.</p>
              </div>
              <.pill :if={@baseline} kind={status_kind(@baseline.status)}>
                {humanize_status(@baseline.status)}
              </.pill>
            </header>

            <div :if={@baseline} class="ops-baseline-facts">
              <div><span>Revision</span><strong>v{@baseline.revision}</strong></div>
              <div><span>Development SHA</span><code>{short_sha(@baseline.development_sha)}</code></div>
              <div><span>Updated</span><strong>{format_timestamp(@baseline.updated_at)}</strong></div>
            </div>

            <section :if={@baseline} id="baseline-agent-details" class="ops-agent-inspector">
              <header>
                <div>
                  <p class="ops-eyebrow">Baseline agent</p>
                  <h3>Session details &amp; conversation</h3>
                  <p>Backend configuration, runtime identity, messages, and MCP activity.</p>
                </div>
                <span class="ops-count">{length(@baseline_sessions)} sessions</span>
              </header>

              <div :if={@baseline_sessions == []} class="ops-agent-empty">
                <span>···</span>
                <div>
                  <strong>Waiting for an agent session</strong>
                  <small>Details appear as soon as Baseline Alignment starts.</small>
                </div>
              </div>

              <section :for={session <- @baseline_sessions} class="ops-agent-session">
                <button
                  type="button"
                  class="ops-agent-session-toggle"
                  phx-click="toggle_agent_session"
                  phx-value-session={session.id}
                  aria-expanded={to_string(@open_session_id == session.id)}
                  aria-controls={"agent-session-#{session.id}"}
                >
                  <span class="ops-agent-avatar">AI</span>
                  <span class="ops-agent-summary-copy">
                    <strong>{role_name(session.role)}</strong>
                    <small>
                      {backend_name(session)} · Session {session.session_sequence} · {length(
                        session_turns(@turns, session.id)
                      )} turns
                    </small>
                  </span>
                  <.pill kind={status_kind(session.status)}>{humanize_status(session.status)}</.pill>
                  <span class="ops-chevron">›</span>
                </button>

                <div
                  id={"agent-session-#{session.id}"}
                  class="ops-agent-session-body"
                  hidden={@open_session_id != session.id}
                >
                  <dl class="ops-agent-config">
                    <div><dt>Backend</dt><dd>{backend_name(session)}</dd></div>
                    <div><dt>Model</dt><dd>{config_value(session, "model")}</dd></div>
                    <div><dt>Reasoning</dt><dd>{config_value(session, "reasoning_effort")}</dd></div>
                    <div><dt>Started</dt><dd>{format_timestamp(session.started_at)}</dd></div>
                  </dl>

                  <div class="ops-agent-identifiers">
                    <span>Session ID <code title={session.id}>{session.id}</code></span>
                    <span :if={session.provider_session_id}>
                      Provider ID <code title={session.provider_session_id}>{session.provider_session_id}</code>
                    </span>
                    <span :if={session.recovery_sequence > 0}>
                      Recovery <code>#{session.recovery_sequence}</code>
                    </span>
                  </div>

                  <div :if={active_session?(session)} class="ops-agent-restart">
                    <span>
                      <strong>Restart this agent</strong>
                      <small>
                        Interrupt the active backend, preserve recovery state, and continue in a new Session.
                      </small>
                    </span>
                    <button
                      type="button"
                      phx-click="restart_agent_session"
                      phx-value-session={session.id}
                      phx-disable-with="Restarting…"
                      class="ops-button ops-button-danger"
                    >
                      Interrupt &amp; restart
                    </button>
                  </div>

                  <div
                    :if={session_turns(@turns, session.id) == []}
                    class="ops-agent-conversation-empty"
                  >
                    No conversation turns have been recorded for this session yet.
                  </div>

                  <div
                    :if={session_turns(@turns, session.id) != []}
                    class="ops-agent-conversation"
                    id={"agent-conversation-#{session.id}"}
                    phx-hook="ConversationScroll"
                  >
                    <section
                      :for={turn <- session_turns(@turns, session.id)}
                      class="ops-chat-turn"
                    >
                      <div class="ops-chat-turn-divider">
                        <span>Turn {turn.turn}</span>
                        <.pill kind={status_kind(turn_status(turn))}>
                          {humanize_status(turn_status(turn))}
                        </.pill>
                      </div>

                      <article
                        :for={message <- turn.input_messages}
                        class={"ops-chat-message ops-chat-message-#{message_role(message, "user")}"}
                      >
                        <span class="ops-chat-avatar">{message_avatar(message, "user")}</span>
                        <div>
                          <header>
                            <strong>{message_author(message, "user")}</strong>
                            <small>Turn {turn.turn}</small>
                          </header>
                          <pre>{message_text(message)}</pre>
                        </div>
                      </article>

                      <details
                        :if={turn.mcp_calls != []}
                        id={"tool-activity-#{turn.id}"}
                        class="ops-chat-tools"
                        phx-hook="PersistDetails"
                      >
                        <summary>
                          <span class="ops-chat-tools-icon">⌕</span>
                          <div>
                            <strong>{tool_activity_title(turn.mcp_calls)}</strong>
                            <small>{tool_activity_meta(turn.mcp_calls)}</small>
                          </div>
                          <.pill kind={status_kind(tool_activity_status(turn.mcp_calls))}>
                            {humanize_status(tool_activity_status(turn.mcp_calls))}
                          </.pill>
                          <span class="ops-chevron">›</span>
                        </summary>
                        <div class="ops-chat-tools-list">
                          <article :for={call <- turn.mcp_calls} class="ops-chat-tool-row">
                            <span class={"ops-chat-tool-icon kind-#{tool_kind(call)}"}>
                              {tool_icon(call)}
                            </span>
                            <div>
                              <strong>{tool_name(call)}</strong>
                              <small :if={tool_detail(call)}>{tool_detail(call)}</small>
                            </div>
                            <.pill kind={status_kind(tool_call_status(call))}>
                              {humanize_status(tool_call_status(call))}
                            </.pill>
                            <button
                              :if={call["command_ref"]}
                              type="button"
                              class="ops-chat-tool-output"
                              phx-click="open_command_console"
                              phx-value-ref={call["command_ref"]}
                            >
                              View output
                            </button>
                          </article>
                        </div>
                      </details>

                      <article
                        :for={{message, message_index} <- Enum.with_index(turn.output_messages, 1)}
                        class={
                          "ops-chat-message ops-chat-message-#{message_role(message, "assistant")} ops-chat-message-#{message_phase(message)}"
                        }
                      >
                        <span class="ops-chat-avatar">{message_avatar(message, "assistant")}</span>
                        <div>
                          <header>
                            <strong>{message_author(message, "assistant")}</strong>
                            <small>{message_caption(turn, message, message_index)}</small>
                          </header>
                          <pre>{message_text(message)}</pre>
                          <i
                            :if={streaming_message?(turn, message, message_index)}
                            class="ops-stream-cursor"
                            aria-label="Generating"
                          >
                          </i>
                        </div>
                      </article>

                      <div
                        :if={turn.partial && turn.output_messages == []}
                        class="ops-chat-thinking"
                      >
                        <span class="ops-chat-avatar">AI</span>
                        <div>
                          <strong>Baseline Agent</strong>
                          <span><i></i><i></i><i></i></span>
                        </div>
                      </div>
                    </section>
                  </div>
                </div>
              </section>
            </section>

            <div :if={is_nil(@baseline)} class="ops-empty-state">
              <span>01</span>
              <strong>Preparing the baseline</strong>
              <p>The baseline agent has not created a revision yet.</p>
            </div>

            <.form
              :if={@baseline && @baseline.status == "drafting"}
              id="baseline-message-form"
              for={@message_form}
              phx-submit="send_message"
              class="ops-form ops-message-form"
              data-submit-on-enter="true"
            >
              <div class="ops-form-heading">
                <div>
                  <label for={@message_form[:body].id}>Message the Baseline Agent</label>
                  <small>Clarify intent, constraints, or the measurement approach.</small>
                </div>
              </div>
              <.input
                field={@message_form[:body]}
                type="textarea"
                placeholder="例如：优先关注 attention kernel latency，并保持数值误差低于…"
              />
              <div class="ops-form-actions">
                <span>Enter to send · ⌘/Ctrl/Shift+Enter for a new line</span>
                <button
                  type="submit"
                  class="ops-button ops-button-primary"
                  phx-disable-with="Sending…"
                >
                  Send message
                </button>
              </div>
            </.form>

            <div :if={@questions} class="ops-question-batch">
              <header>
                <span>Action required</span>
                <div>
                  <h3>Agent needs your decision</h3>
                  <p>Answer every question before the alignment can continue.</p>
                </div>
              </header>
              <form phx-submit="answer_questions">
                <fieldset :for={{question, index} <- Enum.with_index(@questions.questions, 1)}>
                  <legend><span>{index}</span>{question["question"]}</legend>
                  <label :for={option <- question["options"]}>
                    <input
                      type="radio"
                      name={"answers[#{question["id"]}][choice]"}
                      value={option["label"]}
                    />
                    <span><strong>{option["label"]}</strong><small>{option["description"]}</small></span>
                  </label>
                  <label class="ops-custom-answer">
                    <span>
                      <strong>Write your own answer</strong>
                      <small>This answer takes precedence over a selected option.</small>
                    </span>
                    <textarea
                      name={"answers[#{question["id"]}][custom]"}
                      placeholder="Type a specific answer, constraint, path, or measurement rule…"
                    ></textarea>
                  </label>
                </fieldset>
                <div class="ops-form-actions">
                  <span>{length(@questions.questions)} questions in this batch</span>
                  <button type="submit" class="ops-button ops-button-primary">Submit answers</button>
                </div>
              </form>
            </div>

            <div
              :if={@baseline && @baseline.status == "awaiting_review"}
              class="ops-review-workspace"
            >
              <div class="ops-review-callout">
                <span>Review required</span>
                <p>The definition is frozen. Approval advances the run to full Baseline Verify.</p>
              </div>
              <details open>
                <summary><span>01</span>Baseline Definition <b>JSON</b></summary>
                <pre>{@review_bundle.definition}</pre>
              </details>
              <details>
                <summary><span>02</span>Smoke Verify <b>JSON</b></summary>
                <pre>{@review_bundle.smoke_verify}</pre>
              </details>
              <details>
                <summary><span>03</span>Smoke Benchmark <b>LOG</b></summary>
                <pre>{@review_bundle.smoke_benchmark}</pre>
              </details>
              <div class="ops-review-actions">
                <button phx-click="approve_baseline" class="ops-button ops-button-primary">
                  Approve baseline
                </button>
                <.form for={@review_form} phx-submit="request_changes" class="ops-form">
                  <label for={@review_form[:feedback].id}>Request changes</label>
                  <.input
                    field={@review_form[:feedback]}
                    type="textarea"
                    placeholder="说明需要调整的定义、case 或 metric…"
                  />
                  <button type="submit" class="ops-button ops-button-secondary">Return with feedback</button>
                </.form>
              </div>
            </div>
          </section>

          <section class="ops-panel ops-summary-panel">
            <header class="ops-panel-header">
              <div>
                <p class="ops-eyebrow">Progress brief</p>
                <h2>Latest Summary</h2>
                <p>A compact, user-facing view generated from the event stream.</p>
              </div>
              <span class="ops-live-label"><i></i> Live</span>
            </header>

            <div :if={@summary} class="ops-summary-content markdown-body">
              {PikaWeb.Markdown.render(@summary)}
            </div>
            <div :if={is_nil(@summary)} class="ops-empty-state ops-summary-empty">
              <span>···</span>
              <strong>No progress summary yet</strong>
              <p>The first summary appears after enough optimization activity has accumulated.</p>
            </div>

            <.form for={@guidance_form} phx-submit="add_guidance" class="ops-form ops-guidance-form">
              <div class="ops-form-heading">
                <div>
                  <label for={@guidance_form[:body].id}>Iteration Guidance</label>
                  <small>Applied to attempts created after this point.</small>
                </div>
              </div>
              <.input
                field={@guidance_form[:body]}
                type="textarea"
                placeholder="例如：下一轮优先检查 memory coalescing，不要改变 public API…"
              />
              <div class="ops-form-actions">
                <span>Does not interrupt active attempts</span>
                <button type="submit" class="ops-button ops-button-secondary">Save guidance</button>
              </div>
            </.form>
          </section>
        </div>

        <section class="ops-panel ops-history-panel">
          <header class="ops-panel-header">
            <div>
              <p class="ops-eyebrow">Attempts</p>
              <h2>Optimization History</h2>
              <p>Every candidate, its base revision, and the resulting outcome.</p>
            </div>
            <span class="ops-count">{length(@snapshot.attempts)} total</span>
          </header>
          <div :if={@snapshot.attempts != []} class="ops-table-wrap">
            <table class="ops-table">
              <thead>
                <tr><th>Attempt</th><th>Status</th><th>Round</th><th>Base SHA</th><th>Summary / reason</th></tr>
              </thead>
              <tbody>
                <tr :for={attempt <- Enum.reverse(@snapshot.attempts)}>
                  <td><strong>#{attempt.id}</strong></td>
                  <td><.pill kind={status_kind(attempt.status)}>{humanize_status(attempt.status)}</.pill></td>
                  <td>{attempt.iteration_round || "—"}</td>
                  <td><code>{short_sha(attempt.base_sha)}</code></td>
                  <td>{attempt.summary || attempt.failure_reason || "No summary yet"}</td>
                </tr>
              </tbody>
            </table>
          </div>
          <div :if={@snapshot.attempts == []} class="ops-empty-state ops-history-empty">
            <span>00</span>
            <strong>No attempts yet</strong>
            <p>Attempts begin after the baseline definition and verification are accepted.</p>
          </div>
        </section>

      </div>
    </main>
    <.command_console console={@command_console} />
    """
  end

  defp refresh(socket) do
    cursor =
      Repo.query!("SELECT COALESCE(MAX(id), 0) FROM conversation_turns").rows |> hd() |> hd()

    snapshot = Snapshot.build(cursor, DateTime.utc_now())
    baseline = BaselineLifecycle.latest_revision()
    questions = if Process.whereis(Questions), do: Questions.pending(), else: nil
    {baseline_sessions, turns} = baseline_activity(baseline)

    socket
    |> assign(:snapshot, snapshot)
    |> assign(:baseline, baseline)
    |> assign(:baseline_sessions, baseline_sessions)
    |> assign_open_session(baseline_sessions)
    |> assign(:questions, questions)
    |> assign(:turns, turns)
    |> assign(:review_bundle, review_bundle(baseline))
    |> assign(:summary, latest_summary())
    |> assign(:workspace, Persistence.current().workspace_canonical_path)
    |> assign_new(:flash_message, fn -> nil end)
  end

  defp baseline_activity(nil), do: {[], []}

  defp baseline_activity(baseline) do
    roles = ["baseline_alignment", "baseline_verify"]
    work_id = to_string(baseline.id)

    sessions =
      roles
      |> Enum.flat_map(&ConversationJournal.work_sessions(&1, "baseline_revision", work_id))
      |> Enum.sort_by(&{&1.started_at, &1.session_sequence})

    turns =
      roles
      |> Enum.flat_map(&ConversationJournal.work_turns(&1, "baseline_revision", work_id))
      |> Enum.sort_by(&{&1.started_at, &1.id})

    {sessions, turns}
  end

  defp assign_open_session(socket, sessions) do
    open_session_id = socket.assigns[:open_session_id]

    if Enum.any?(sessions, &(&1.id == open_session_id)) do
      socket
    else
      assign(socket, :open_session_id, default_open_session_id(sessions))
    end
  end

  defp default_open_session_id(sessions) do
    case Enum.find(Enum.reverse(sessions), &active_session?/1) || List.last(sessions) do
      nil -> nil
      session -> session.id
    end
  end

  defp normalize_question_answers(questions, answers) when is_map(answers) do
    Enum.map(questions, fn question ->
      id = question["id"]

      case Map.get(answers, id, %{}) do
        %{} = values ->
          custom = values |> Map.get("custom", "") |> trim_param()
          choice = values |> Map.get("choice", "") |> trim_param()

          if custom == "",
            do: %{"id" => id, "answer" => choice},
            else: %{"id" => id, "answer" => custom, "custom" => true}

        choice when is_binary(choice) ->
          %{"id" => id, "answer" => String.trim(choice)}

        _other ->
          %{"id" => id, "answer" => ""}
      end
    end)
  end

  defp normalize_question_answers(questions, _answers),
    do: Enum.map(questions, &%{"id" => &1["id"], "answer" => ""})

  defp trim_param(value) when is_binary(value), do: String.trim(value)
  defp trim_param(_value), do: ""

  defp update_message_form(socket, :ok, _body) do
    socket
    |> assign(:message_form, to_form(%{"body" => ""}, as: :message))
    |> push_event("clear-form", %{id: "baseline-message-form"})
  end

  defp update_message_form(socket, _result, body),
    do: assign(socket, :message_form, to_form(%{"body" => body}, as: :message))

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

  defp switch_console_subscription(socket, ref) do
    previous_ref = socket.assigns[:command_console_ref]

    if connected?(socket) and is_binary(previous_ref) and previous_ref != ref,
      do: Phoenix.PubSub.unsubscribe(Pika.PubSub, CommandConsole.topic(previous_ref))

    if connected?(socket) and is_binary(ref) and previous_ref != ref,
      do: Phoenix.PubSub.subscribe(Pika.PubSub, CommandConsole.topic(ref))

    socket
  end

  defp control(socket, callback, success) do
    result = callback.()
    {:noreply, socket |> put_result(result, success) |> refresh()}
  end

  defp put_result(socket, :ok, success), do: assign(socket, :flash_message, success)
  defp put_result(socket, {:ok, _value}, success), do: assign(socket, :flash_message, success)

  defp put_result(socket, {:error, reason}, _success),
    do: assign(socket, :flash_message, inspect(reason))

  defp message_text(%{"content" => content}) when is_binary(content), do: content
  defp message_text(message) when is_map(message), do: pretty_json(message)
  defp message_text(message), do: inspect(message)

  defp message_role(%{"role" => role}, _fallback)
       when role in ["user", "assistant", "system"],
       do: role

  defp message_role(_message, fallback), do: fallback

  defp message_avatar(message, fallback) do
    case message_role(message, fallback) do
      "user" -> "You"
      "system" -> "SYS"
      _assistant -> "AI"
    end
  end

  defp message_author(message, fallback) do
    case message_role(message, fallback) do
      "user" -> "You"
      "system" -> "System"
      _assistant -> "Baseline Agent"
    end
  end

  defp message_phase(%{"phase" => "final_answer"}), do: "final"
  defp message_phase(%{"phase" => "commentary"}), do: "update"
  defp message_phase(_message), do: "default"

  defp message_caption(turn, message, index) do
    cond do
      streaming_message?(turn, message, index) -> "Streaming"
      message["phase"] == "final_answer" -> "Final answer"
      message["phase"] == "commentary" -> "Update #{index}"
      true -> "Turn #{turn.turn} · Message #{index}"
    end
  end

  defp streaming_message?(turn, message, index) do
    turn.partial && index == length(turn.output_messages) && message["complete"] != true
  end

  defp tool_activity_title(calls) do
    names = calls |> Enum.map(&tool_name/1) |> Enum.uniq()

    case names do
      [name] when length(calls) == 1 -> name
      [name] -> "#{name} · #{length(calls)}"
      _names -> "#{length(calls)} tool calls"
    end
  end

  defp tool_activity_meta(calls) do
    counts = Enum.frequencies_by(calls, &tool_call_status/1)

    [
      count_label(counts["running"], "running"),
      count_label(counts["completed"], "completed"),
      count_label(counts["failed"], "failed"),
      count_label(counts["cancelled"], "cancelled")
    ]
    |> Enum.reject(&is_nil/1)
    |> Enum.join(" · ")
  end

  defp count_label(nil, _label), do: nil
  defp count_label(0, _label), do: nil
  defp count_label(count, label), do: "#{count} #{label}"

  defp tool_activity_status(calls) do
    statuses = Enum.map(calls, &tool_call_status/1)

    cond do
      "running" in statuses -> "running"
      Enum.any?(statuses, &(&1 in ["failed", "cancelled"])) -> "failed"
      true -> "completed"
    end
  end

  defp tool_call_status(call) do
    case call["status"] |> to_string() |> Macro.underscore() do
      status when status in ["running", "in_progress", "started", "pending"] -> "running"
      status when status in ["failed", "error"] -> "failed"
      "cancelled" -> "cancelled"
      _status -> "completed"
    end
  end

  defp tool_name(call), do: call["name"] || "Used a tool"

  defp tool_detail(call) do
    case call["summary"] do
      summary when is_binary(summary) -> String.slice(summary, 0, 180)
      _summary -> nil
    end
  end

  defp tool_kind(call), do: call["kind"] || "mcp"
  defp tool_icon(%{"kind" => "command"}), do: ">_"
  defp tool_icon(%{"kind" => "file_change"}), do: "±"
  defp tool_icon(%{"kind" => "web_search"}), do: "⌕"
  defp tool_icon(%{"kind" => "image_view"}), do: "◫"
  defp tool_icon(%{"kind" => "mcp"}), do: "↳"
  defp tool_icon(_call), do: "·"

  defp session_turns(turns, session_id),
    do: Enum.filter(turns, &(&1.session_id == session_id))

  defp turn_status(%{ended_reason: reason}) when is_binary(reason), do: reason
  defp turn_status(%{partial: true}), do: "running"
  defp turn_status(_turn), do: "completed"

  defp active_session?(session),
    do: session.status in ["running", "awaiting_report", "awaiting_followup"]

  defp role_name("baseline_alignment"), do: "Baseline Alignment"
  defp role_name("baseline_verify"), do: "Baseline Verify"
  defp role_name(role), do: humanize_status(role)

  defp backend_name(session) do
    session
    |> config_value("backend")
    |> humanize_status()
  end

  defp config_value(session, key) do
    case session.backend_config[key] do
      nil -> "Backend default"
      "" -> "Backend default"
      value -> to_string(value)
    end
  end

  defp pretty_json(value) do
    case Jason.encode(value, pretty: true) do
      {:ok, encoded} -> encoded
      {:error, _reason} -> inspect(value)
    end
  end

  defp humanize_status(nil), do: "Unknown"

  defp humanize_status(status) do
    status
    |> to_string()
    |> String.replace("_", " ")
    |> String.capitalize()
  end

  defp status_kind(status)
       when status in ["failed", "rejected", "cancelled", "stopped", "error"],
       do: "error"

  defp status_kind(status) when status in ["completed", "accepted", "passed"], do: "success"

  defp status_kind(status)
       when status in ["paused", "draining", "awaiting_review", "awaiting_followup"],
       do: "warning"

  defp status_kind(status)
       when status in [
              "aligning_baseline",
              "drafting",
              "verifying",
              "optimizing",
              "integrating",
              "running",
              "ready_for_integration"
            ],
       do: "active"

  defp status_kind(_status), do: "neutral"

  defp stage_title("aligning_baseline"), do: "Defining a trustworthy baseline"
  defp stage_title("optimizing"), do: "Searching for a better implementation"
  defp stage_title("paused"), do: "Optimization paused"
  defp stage_title("draining"), do: "Finishing active work"
  defp stage_title("completed"), do: "Optimization complete"
  defp stage_title("stopped"), do: "Optimization stopped"
  defp stage_title("failed"), do: "Optimization needs attention"
  defp stage_title(_status), do: "Optimization in progress"

  defp stage_description("aligning_baseline"),
    do: "Pika is mapping cases, metrics, and verification rules before changing code."

  defp stage_description("optimizing"),
    do:
      "Agents are producing candidates, measuring them, and promoting only verified improvements."

  defp stage_description("paused"),
    do: "No new work will be scheduled until this optimization is resumed."

  defp stage_description("draining"),
    do: "New attempts are disabled while active work reaches a terminal state."

  defp stage_description("completed"),
    do: "All scheduled work has finished and the best verified revision is preserved locally."

  defp stage_description("stopped"),
    do: "The run was stopped and all active work was interrupted."

  defp stage_description("failed"),
    do:
      "The run encountered a terminal error. Review the latest activity for the failure context."

  defp stage_description(_status),
    do: "Pika is coordinating the optimization lifecycle and recording every decision."

  defp terminal_status?(status), do: status in ["completed", "stopped", "failed"]

  defp attempt_activity([]), do: "Begins after baseline approval"

  defp attempt_activity(attempts) do
    active = Enum.count(attempts, &(&1.status not in ["accepted", "rejected", "cancelled"]))
    if active == 0, do: "No active attempts", else: "#{active} currently active"
  end

  defp agent_activity([]), do: "No active sessions"

  defp agent_activity(sessions) do
    sessions
    |> Enum.map(&humanize_status(&1.role))
    |> Enum.uniq()
    |> Enum.join(", ")
  end

  defp followup_activity([]), do: "No pending follow-ups"
  defp followup_activity([followup | _rest]), do: humanize_status(followup.status)

  defp format_timestamp(nil), do: "—"

  defp format_timestamp(timestamp) when is_integer(timestamp) do
    case DateTime.from_unix(timestamp, :microsecond) do
      {:ok, datetime} -> Calendar.strftime(datetime, "%b %d · %H:%M UTC")
      {:error, _reason} -> "—"
    end
  end

  defp format_timestamp(value), do: to_string(value)

  defp short_sha(nil), do: "—"
  defp short_sha(value) when byte_size(value) > 12, do: String.slice(value, 0, 12)
  defp short_sha(value), do: value
end
