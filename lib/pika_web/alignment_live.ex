defmodule PikaWeb.AlignmentLive do
  use PikaWeb, :live_view

  @benchmark_case_display_limit 10
  @baseline_flow_steps [
    {:references, "准备 References"},
    {:setup_merge, "核验 Setup Merge"},
    {:measure, "采集全量 Baseline"},
    {:validate, "校验 Baseline"},
    {:iteration_sample, "选择 Iteration Sample"}
  ]
  @reference_languages %{
    ".bash" => "bash",
    ".c" => "c",
    ".cc" => "cpp",
    ".cjs" => "javascript",
    ".cmake" => "cmake",
    ".cpp" => "cpp",
    ".cts" => "typescript",
    ".cu" => "cpp",
    ".cuh" => "cpp",
    ".cxx" => "cpp",
    ".ex" => "elixir",
    ".exs" => "elixir",
    ".go" => "go",
    ".h" => "c",
    ".hpp" => "cpp",
    ".hxx" => "cpp",
    ".ini" => "ini",
    ".java" => "java",
    ".js" => "javascript",
    ".json" => "json",
    ".jsonl" => "json",
    ".jsx" => "javascript",
    ".mjs" => "javascript",
    ".mts" => "typescript",
    ".py" => "python",
    ".pyw" => "python",
    ".rs" => "rust",
    ".sh" => "bash",
    ".toml" => "toml",
    ".ts" => "typescript",
    ".tsx" => "typescript",
    ".yaml" => "yaml",
    ".yml" => "yaml",
    ".zsh" => "bash"
  }
  @reference_language_filenames %{
    "cmakelists.txt" => "cmake",
    "dockerfile" => "bash",
    "makefile" => "makefile"
  }

  alias Pika.Alignment.{ArtifactStore, Campaign}
  alias PikaWeb.Markdown

  @impl true
  def mount(_params, session, socket) do
    if authenticated?(session) do
      mount_authenticated(socket)
    else
      {:ok, redirect(socket, to: "/")}
    end
  end

  defp mount_authenticated(socket) do
    if connected?(socket), do: Phoenix.PubSub.subscribe(Pika.PubSub, Campaign.topic())

    snapshot =
      case Campaign.snapshot() do
        {:error, _} -> empty_snapshot()
        value -> value
      end

    socket =
      socket
      |> assign(:message_form, message_form())
      |> assign(:reference_project_form, reference_project_form())
      |> assign(:show_diff, false)
      |> assign(:flash_message, nil)
      |> assign(:implementation_review, nil)
      |> assign(:implementation_review_error, nil)
      |> assign(:implementation_review_identity, nil)
      |> assign(:reviewed_target_digest, nil)
      |> assign(:reviewed_development_sha, nil)
      |> assign(:reviewed_implementation_evidence_digest, nil)
      |> assign_campaign_snapshot(snapshot)
      |> allow_upload(:inputs, accept: :any, max_entries: 5, max_file_size: 1_073_741_824)

    {:ok, socket}
  end

  @impl true
  def handle_info({:campaign_updated, snapshot}, socket),
    do: {:noreply, assign_campaign_snapshot(socket, snapshot)}

  @impl true
  def handle_event("send_message", %{"message" => params}, socket) do
    body = String.trim(params["body"] || "")
    intent = params["intent"] || "conversation"
    entries = socket.assigns.uploads.inputs.entries
    socket = assign(socket, :message_form, message_form(body, intent))

    cond do
      body == "" and entries == [] ->
        {:noreply, assign_result(socket, {:error, :empty_message})}

      not uploads_ready?(entries) ->
        {:noreply, assign_result(socket, {:error, :uploads_in_progress})}

      true ->
        submit_message(socket, body, intent)
    end
  end

  def handle_event("cancel_upload", %{"ref" => ref}, socket) do
    {:noreply, cancel_upload(socket, :inputs, ref)}
  end

  def handle_event("prepare_change", %{"target" => target}, socket) do
    prompt = change_prompt(target)

    {:noreply,
     socket
     |> assign(:message_form, message_form(prompt, "request_changes"))
     |> push_event("composer:focus", %{})}
  end

  def handle_event("cancel_change", _params, socket) do
    {:noreply,
     socket
     |> assign(:message_form, message_form())
     |> push_event("composer:focus", %{})}
  end

  def handle_event("toggle_reference", %{"id" => id}, socket) do
    result = Campaign.toggle_reference(id)

    socket =
      if result == :ok,
        do: assign_campaign_snapshot(socket, Campaign.snapshot()),
        else: socket

    {:noreply, assign_result(socket, result)}
  end

  def handle_event("add_reference_project", %{"reference_project" => params}, socket) do
    socket = assign(socket, :reference_project_form, reference_project_form(params))

    case Campaign.add_reference_project(params) do
      {:ok, _entry} ->
        {:noreply,
         socket
         |> assign_campaign_snapshot(Campaign.snapshot())
         |> assign(:reference_project_form, reference_project_form())
         |> assign_result(:ok)}

      error ->
        {:noreply, assign_reference_project_result(socket, error)}
    end
  end

  def handle_event("remove_reference_project", %{"id" => id}, socket) do
    result = Campaign.remove_reference_project(id)

    socket =
      if result == :ok,
        do: assign_campaign_snapshot(socket, Campaign.snapshot()),
        else: socket

    {:noreply, assign_reference_project_result(socket, result)}
  end

  def handle_event("confirm_spec", _params, socket) do
    result =
      Campaign.confirm_spec(
        socket.assigns.reviewed_target_digest,
        socket.assigns.reviewed_development_sha,
        socket.assigns.reviewed_implementation_evidence_digest
      )

    socket =
      case result do
        :ok -> assign_campaign_snapshot(socket, Campaign.snapshot())
        _error -> assign_campaign_snapshot(socket, Campaign.snapshot(), force_review: true)
      end

    {:noreply, assign_result(socket, result)}
  end

  def handle_event("toggle_implementation_review", _params, socket) do
    {reviewed_target_digest, reviewed_development_sha, reviewed_implementation_evidence_digest} =
      case {
        socket.assigns.snapshot.status,
        socket.assigns.implementation_review,
        socket.assigns.snapshot.implementation_review_evidence
      } do
        {:awaiting_confirmation, %{target_snapshot: %{digest: target_digest}},
         %{digest: evidence_digest, development_sha: development_sha}} ->
          if socket.assigns.reviewed_target_digest == target_digest and
               socket.assigns.reviewed_development_sha == development_sha and
               socket.assigns.reviewed_implementation_evidence_digest == evidence_digest do
            {nil, nil, nil}
          else
            {target_digest, development_sha, evidence_digest}
          end

        _ ->
          {
            socket.assigns.reviewed_target_digest,
            socket.assigns.reviewed_development_sha,
            socket.assigns.reviewed_implementation_evidence_digest
          }
      end

    {:noreply,
     socket
     |> assign(:reviewed_target_digest, reviewed_target_digest)
     |> assign(:reviewed_development_sha, reviewed_development_sha)
     |> assign(
       :reviewed_implementation_evidence_digest,
       reviewed_implementation_evidence_digest
     )}
  end

  def handle_event("toggle_diff", _params, socket) do
    {:noreply, update(socket, :show_diff, &not/1)}
  end

  def handle_event("answer_question", params, socket) do
    question = socket.assigns.snapshot.pending_question
    answer = String.trim(params["answer"] || "")
    option_id = params["option_id"]
    question_id = params["question_id"] || (question && question.id)

    result =
      if question,
        do: Campaign.answer_question(question_id, answer, option_id),
        else: {:error, :no_pending_question}

    {:noreply, assign_result(socket, result)}
  end

  defp submit_message(socket, body, intent) do
    root = socket.assigns.snapshot.workspace.root

    results =
      consume_uploaded_entries(socket, :inputs, fn %{path: path}, entry ->
        {:postpone, ArtifactStore.copy_upload(root, path, entry.client_name)}
      end)

    {artifacts, errors} =
      Enum.reduce(results, {[], []}, fn
        {:ok, artifact}, {artifacts, errors} -> {[artifact | artifacts], errors}
        {:error, reason}, {artifacts, errors} -> {artifacts, [reason | errors]}
      end)

    artifacts = Enum.reverse(artifacts)

    result =
      cond do
        errors != [] -> {:error, {:upload_failed, Enum.reverse(errors)}}
        intent == "request_changes" -> Campaign.request_changes(body, artifacts)
        true -> Campaign.send_message(body, artifacts)
      end

    case result do
      :ok ->
        _consumed =
          consume_uploaded_entries(socket, :inputs, fn _meta, _entry -> {:ok, :consumed} end)

        {:noreply,
         socket
         |> assign(:message_form, message_form())
         |> assign_result(:ok)
         |> push_event("composer:clear", %{})}

      error ->
        ArtifactStore.delete_uploads(root, artifacts)
        {:noreply, assign_result(socket, error)}
    end
  end

  defp assign_campaign_snapshot(socket, snapshot, opts \\ []) do
    identity = implementation_review_identity(snapshot)
    force? = Keyword.get(opts, :force_review, false)

    if not force? and socket.assigns.implementation_review_identity == identity do
      assign(socket, :snapshot, snapshot)
    else
      {implementation_review, implementation_review_error} =
        if identity do
          case Campaign.implementation_review() do
            {:ok, review} -> {review, nil}
            {:error, reason} -> {nil, inspect(reason)}
          end
        else
          {nil, nil}
        end

      socket
      |> assign(:snapshot, snapshot)
      |> assign(:implementation_review, implementation_review)
      |> assign(:implementation_review_error, implementation_review_error)
      |> assign(:implementation_review_identity, identity)
      |> assign(:reviewed_target_digest, nil)
      |> assign(:reviewed_development_sha, nil)
      |> assign(:reviewed_implementation_evidence_digest, nil)
    end
  end

  defp implementation_review_identity(
         %{harness: %{digest: harness_digest}, target_snapshot: %{id: target_id}} = snapshot
       ),
       do:
         {harness_digest, target_id, snapshot.prepared_setup_sha,
          snapshot.implementation_review_evidence &&
            snapshot.implementation_review_evidence.digest}

  defp implementation_review_identity(_snapshot), do: nil

  @impl true
  def render(assigns) do
    ~H"""
    <main class="alignment-shell">
      <header class="topbar">
        <div class="brand">
          <div class="brand-mark">P</div>
          <div><strong>{product_name()}</strong><small>{product_subtitle()}</small></div>
        </div>
        <nav id="alignment-nav" class="alignment-nav" aria-label="主导航">
          <a class="active" href="/alignment" aria-current="page">目标对齐</a>
          <a href="/control?tab=attempts">Attempts</a>
          <a href="/control?tab=metrics">Metrics</a>
          <a href="/control?tab=sync">Sync</a>
          <a href="/control?tab=audit">Audit</a>
        </nav>
        <div class="campaign-status">
          <.pill kind={status_kind(@snapshot.status)}>{status_label(@snapshot.status)}</.pill>
          <span class="muted"> · {@snapshot.backend}</span>
          <span :if={@snapshot.campaign_id} class="muted"> · {@snapshot.campaign_id}</span>
        </div>
      </header>

      <div class="alignment-grid">
        <section class="conversation-panel panel">
          <div class="panel-heading">
            <div><p class="eyebrow">目标对齐对话</p><h1>{spec_title(@snapshot.spec)}</h1></div>
            <.pill kind="active">Spec v{spec_revision(@snapshot)}</.pill>
          </div>

          <p :if={@snapshot.last_error || @flash_message} class="flash">
            {@snapshot.last_error || @flash_message}
          </p>

          <div id="messages" class="conversation-scroll" phx-hook="ConversationScroll">
            <%= for message <- @snapshot.messages do %>
              <details :if={message.role == :activity} id={message.id} class="activity-row">
                <summary>
                  <span class="activity-icon" aria-hidden="true">⌘</span>
                  <span class="activity-summary">{message.content}</span>
                  <span :if={message.activity.running} class="activity-running">运行中</span>
                  <time>{format_time(message.at)}</time>
                  <span class="activity-chevron" aria-hidden="true">›</span>
                </summary>
                <div class="activity-details">
                  <div :for={detail <- message.activity.details} class="activity-detail">
                    <span class={"activity-status activity-status-#{detail.status}"}>
                      {activity_status_icon(detail.status)}
                    </span>
                    <div>
                      <strong>{detail.label}</strong>
                      <code :if={detail.detail not in [nil, ""]}>{detail.detail}</code>
                    </div>
                  </div>
                </div>
              </details>

              <article
                :if={message.role != :activity}
                id={message.id}
                class={"message message-#{message.role}"}
              >
                <div class="message-meta">
                  <div class="message-label">{message_label(message.role)} · {format_time(message.at)}</div>
                  <button
                    :if={message.role == :agent}
                    id={"copy-#{message.id}"}
                    type="button"
                    class="copy-markdown"
                    phx-hook="CopyMarkdown"
                    data-markdown={message.content}
                    aria-label="复制 Markdown"
                    title="复制 Markdown"
                  >复制 Markdown</button>
                </div>
                <div :if={message.role == :agent} class="message-body markdown-body">
                  {Markdown.render(message.content)}
                </div>
                <div :if={message.role != :agent && message.content != ""} class="message-body">
                  {message.content}
                </div>
                <div :if={Map.get(message, :attachments, []) != []} class="message-attachments">
                  <span :for={artifact <- message.attachments} class="attachment-chip">
                    <span aria-hidden="true">↗</span>
                    {Path.basename(artifact.relative_path)}
                    <small>{format_bytes(artifact.size)}</small>
                  </span>
                </div>
                <div
                  :if={pending_question?(@snapshot.pending_question, message)}
                  class="question-card"
                >
                  <div class="question-progress">
                    问题 {@snapshot.pending_question.position} / {@snapshot.pending_question.total}
                  </div>
                  <p>请选择一个答案，或在下方输入你的回答。</p>
                  <div class="question-options">
                    <button
                      :for={option <- @snapshot.pending_question.options}
                      type="button"
                      phx-click="answer_question"
                      phx-value-question_id={@snapshot.pending_question.id}
                      phx-value-answer={option.label}
                      phx-value-option_id={option.id}
                    >
                      <strong>{option.label}</strong>
                      <small :if={option.description != ""}>{option.description}</small>
                    </button>
                  </div>
                  <form class="question-custom" phx-submit="answer_question">
                    <input type="hidden" name="question_id" value={@snapshot.pending_question.id} />
                    <input
                      type="text"
                      name="answer"
                      autocomplete="off"
                      placeholder="输入自己的回答…"
                      aria-label="自定义回答"
                    />
                    <button type="submit" class="secondary">回答</button>
                  </form>
                </div>
              </article>
            <% end %>
            <div
              :if={@snapshot.agent_responding && is_nil(@snapshot.pending_question)}
              id="agent-typing"
              class="agent-typing"
              role="status"
              aria-live="polite"
            >
              <span class="typing-dots" aria-hidden="true"><i></i><i></i><i></i></span>
              <span>Agent 正在输入</span>
            </div>
          </div>

          <div class="composer">
            <div :if={@snapshot.pending_question} class="composer-paused">
              Agent 正在等待你的选择；请依次回答上方问题（{@snapshot.pending_question.position}/{@snapshot.pending_question.total}）。
            </div>
            <.form
              :if={is_nil(@snapshot.pending_question)}
              for={@message_form}
              id="message-form"
              phx-submit="send_message"
              phx-hook="Composer"
            >
              <input type="hidden" name="message[intent]" value={@message_form[:intent].value} />
              <div :if={@message_form[:intent].value == "request_changes"} class="composer-intent">
                <span>将要求 Agent 修改 Campaign Spec</span>
                <button type="button" phx-click="cancel_change" aria-label="取消修改模式">×</button>
              </div>
              <.input
                field={@message_form[:body]}
                type="textarea"
                placeholder="描述计算边界、Shape、Metrics，或要求 Agent 生成采集脚本…"
              />
              <div class="pending-attachments">
                <span :for={entry <- @uploads.inputs.entries} class="pending-attachment">
                  <span class="attachment-name">{entry.client_name}</span>
                  <span>{entry.progress}%</span>
                  <button
                    type="button"
                    phx-click="cancel_upload"
                    phx-value-ref={entry.ref}
                    aria-label={"移除 #{entry.client_name}"}
                  >×</button>
                </span>
              </div>
              <div class="composer-row">
                <label class="upload-trigger">
                  <.live_file_input upload={@uploads.inputs} class="upload-input" />
                  <span>＋ 添加输入文件</span>
                </label>
                <span class="composer-hint">Enter 发送 · Shift+Enter 换行</span>
                <button class="primary send-button" disabled={not uploads_ready?(@uploads.inputs.entries)}>
                  发送
                </button>
              </div>
            </.form>
          </div>
        </section>

        <aside class="review-panel panel">
          <div class="review-title">
            <div>
              <p class="eyebrow">Campaign Spec · Review</p>
              <h2>优化验收单</h2>
              <small>{review_subtitle(@snapshot)}</small>
            </div>
            <.pill kind={review_status_kind(@snapshot.status)}>
              {review_status_label(@snapshot.status)}
            </.pill>
          </div>

          <div class="review-scroll">
            <section
              :if={baseline_flow_visible?(@snapshot)}
              id="baseline-workflow"
              class="baseline-workflow"
              aria-label="Baseline 建立流程"
            >
              <div class="baseline-workflow-heading">
                <div>
                  <p class="eyebrow">执行进度</p>
                  <strong>Baseline 建立流程</strong>
                </div>
                <span>{baseline_flow_summary(@snapshot)}</span>
              </div>
              <progress
                max={length(baseline_flow_steps())}
                value={baseline_flow_completed_steps(@snapshot)}
                aria-label={baseline_flow_summary(@snapshot)}
              >
                {baseline_flow_completed_steps(@snapshot)} / {length(baseline_flow_steps())}
              </progress>
              <ol class="baseline-flow-steps">
                <li
                  :for={{{id, title}, index} <- Enum.with_index(baseline_flow_steps(), 1)}
                  id={"baseline-flow-step-#{id}"}
                  class={"baseline-flow-step baseline-flow-step-#{baseline_flow_step_state(@snapshot, index)}"}
                  aria-current={if baseline_flow_step_state(@snapshot, index) == "current", do: "step"}
                >
                  <span class="baseline-flow-marker">
                    {baseline_flow_step_mark(@snapshot, index)}
                  </span>
                  <span class="baseline-flow-copy">
                    <strong>{title}</strong>
                    <small>{baseline_flow_step_detail(id, @snapshot)}</small>
                  </span>
                  <span class="baseline-flow-state">
                    {baseline_flow_step_state_label(@snapshot, index)}
                  </span>
                </li>
              </ol>
            </section>

            <details class="review-section" open>
              <summary>
                <span class="review-step">1</span>
                <span><strong>目标边界</strong><small>计算什么、在哪里运行</small></span>
                <span class="review-check">{progress_mark(computation_ready?(@snapshot.spec))}</span>
              </summary>
              <div class="review-content">
                <button :if={spec_editable?(@snapshot)} class="text-action" phx-click="prepare_change" phx-value-target="boundary">要求 Agent 修改</button>
                <dl class="boundary-list">
                  <div><dt>Hardware</dt><dd>{@snapshot.spec["target_hardware"] || "待确认"}</dd></div>
                  <div><dt>Correctness Oracle</dt><dd>{oracle_label(@snapshot.spec)}</dd></div>
                  <div><dt>Optimization Target</dt><dd>{target_label(@snapshot.spec)}</dd></div>
                  <div><dt>Development</dt><dd>{nested(@snapshot.spec, ~w(implementations development entrypoint))}</dd></div>
                  <div class="wide"><dt>Semantics</dt><dd>{nested(@snapshot.spec, ~w(computation semantics))}</dd></div>
                  <div class="wide"><dt>Fusion</dt><dd>{nested(@snapshot.spec, ~w(computation fusion_scope))}</dd></div>
                  <div class="wide"><dt>Inputs</dt><dd>{format_contract(get_in(@snapshot.spec, ~w(computation inputs)))}</dd></div>
                  <div class="wide"><dt>Outputs</dt><dd>{format_contract(get_in(@snapshot.spec, ~w(computation outputs)))}</dd></div>
                  <div>
                    <dt>Correctness</dt>
                    <dd>{correctness_summary(@snapshot.spec)}</dd>
                  </div>
                </dl>
                <div :if={is_nil(@snapshot.spec_diff)} class="alignment-progress">
                  <p class="muted">Boundary Agent 正在整理。未完成项不是错误。</p>
                </div>
                <p :if={@snapshot.spec_diff && @snapshot.missing != []} class="missing">
                  仍缺失：{Enum.join(@snapshot.missing, "、")}
                </p>
                <p :for={error <- if(@snapshot.spec_diff, do: @snapshot.spec_errors, else: [])} class="missing">
                  {error}
                </p>
              </div>
            </details>

            <details class="review-section" open>
              <summary>
                <span class="review-step">2</span>
                <span><strong>Metrics</strong><small>{length(metrics(@snapshot))} 项自由指标</small></span>
                <span class="review-check">{progress_mark(metrics(@snapshot) != [])}</span>
              </summary>
              <div class="review-content">
                <button :if={spec_editable?(@snapshot)} class="text-action" phx-click="prepare_change" phx-value-target="metrics">要求 Agent 修改</button>
                <div class="metric-list">
                  <article :for={metric <- metrics(@snapshot)} class="metric-row">
                    <span class={"role-badge role-#{metric["role"]}"}>{role_label(metric["role"])}</span>
                    <div>
                      <strong>{metric["name"] || metric["id"]}</strong>
                      <small><code>{metric["id"]}</code> · {direction_label(metric["direction"])} · {metric["unit"]}</small>
                    </div>
                    <span class="metric-threshold">{format_percent(metric["min_improvement_ratio"])}</span>
                    <button :if={spec_editable?(@snapshot)} class="row-action" phx-click="prepare_change" phx-value-target={"metric:#{metric["id"]}"}>修改</button>
                  </article>
                </div>
                <p :if={metrics(@snapshot) == []} class="empty-copy">等待 Agent 与你确认衡量方式。</p>
              </div>
            </details>

            <details class="review-section" open>
              <summary>
                <span class="review-step">3</span>
                <span>
                  <strong>Benchmark Cases</strong>
                  <small>{case_count_label(@snapshot)}</small>
                </span>
                <span class="review-check">{progress_mark(cases(@snapshot) != [])}</span>
              </summary>
              <div class="review-content">
                <button :if={spec_editable?(@snapshot)} class="text-action" phx-click="prepare_change" phx-value-target="cases">要求 Agent 修改</button>
                <div :if={shape_source(@snapshot)} class="shape-source">
                  <strong>Shape 来源</strong>
                  <span>{shape_source_label(shape_source(@snapshot), length(cases(@snapshot)))}</span>
                </div>
                <div class="case-list">
                  <article :for={case_ <- visible_cases(@snapshot)} class="case-row">
                    <div class="case-heading">
                      <div>
                        <span class={"role-badge role-#{case_["kind"]}"}>{role_label(case_["kind"])}</span>
                        <span class={if sampled?(@snapshot, case_["id"]), do: "sample-badge", else: "full-badge"}>
                          {if sampled?(@snapshot, case_["id"]), do: "ITERATION SAMPLE", else: "FULL REGRESSION"}
                        </span>
                      </div>
                      <button :if={spec_editable?(@snapshot)} class="row-action" phx-click="prepare_change" phx-value-target={"case:#{case_["id"]}"}>修改</button>
                    </div>
                    <strong>{case_["name"] || case_["id"]}</strong>
                    <code class="case-id">{case_["id"]}</code>
                    <div class="shape-chips">
                      <span :for={{key, value} <- shape_pairs(case_["shape"])}>{human_key(key)} {format_value(value)}</span>
                    </div>
                    <div class="case-meta">
                      <span>DType · {format_value(case_["dtype"])}</span>
                      <span>Layout · {format_value(case_["layout"])}</span>
                      <span :if={is_number(case_["frequency_weight"])}>Weight · {format_weight(case_["frequency_weight"])}</span>
                    </div>
                    <p :if={sampling_reason(@snapshot, case_["id"])} class="sample-reason">
                      采样理由：{sampling_reason(@snapshot, case_["id"])}
                    </p>
                  </article>
                </div>
                <p :if={hidden_case_count(@snapshot) > 0} class="case-list-note">
                  为保持界面流畅，仅显示前 {benchmark_case_display_limit()} 个；完整 Campaign Spec 共 {length(cases(@snapshot))} 个 Cases。
                </p>
                <p :if={cases(@snapshot) == []} class="empty-copy">等待 Agent 提交具体、可执行的 Cases。</p>
              </div>
            </details>

            <details class="review-section">
              <summary>
                <span class="review-step">4</span>
                <span><strong>测量与采样规则</strong><small>Baseline、Iteration、Integration</small></span>
                <span class="review-check">{progress_mark(not is_nil(@snapshot.harness))}</span>
              </summary>
              <div class="review-content">
                <button :if={spec_editable?(@snapshot)} class="text-action" phx-click="prepare_change" phx-value-target="measurement">要求 Agent 修改</button>
                <div class="measurement-flow">
                  <div><span>1</span><strong>全量 Baseline</strong><small>全部 Cases × Metrics · {formal_pair_label(@snapshot.spec)}</small></div>
                  <div><span>2</span><strong>Iteration Sample</strong><small>初始最多 10 Cases · {formal_pair_label(@snapshot.spec)}</small></div>
                  <div><span>3</span><strong>归并前全量回归</strong><small>全量 5 Pair · 异常升级 {formal_pair_label(@snapshot.spec)}</small></div>
                </div>
                <dl class="measurement-grid">
                  <div><dt>Harness</dt><dd>{nested(@snapshot.spec, ~w(benchmark harness_path))}</dd></div>
                  <div><dt>Warmup</dt><dd>{nested(@snapshot.spec, ~w(benchmark warmup))}</dd></div>
                  <div><dt>Baseline Pair</dt><dd>{nested(@snapshot.spec, ~w(benchmark pair_count))}</dd></div>
                  <div><dt>Valid Pair</dt><dd>{nested(@snapshot.spec, ~w(benchmark min_valid_pairs))}</dd></div>
                  <div><dt>Retry</dt><dd>{nested(@snapshot.spec, ~w(benchmark retry_limit))}</dd></div>
                  <div><dt>Stopping</dt><dd>{stopping_summary(@snapshot.spec)}</dd></div>
                </dl>
                <div :if={@snapshot.baseline_progress} class="baseline-validation-progress">
                  <div>
                    <strong>{baseline_phase_label(@snapshot.baseline_progress.phase)}</strong>
                    <span>{format_percent_number(baseline_progress_percent(@snapshot.baseline_progress))}</span>
                  </div>
                  <progress max="100" value={baseline_progress_percent(@snapshot.baseline_progress)}>
                    {format_percent_number(baseline_progress_percent(@snapshot.baseline_progress))}
                  </progress>
                  <small>
                    {format_count(@snapshot.baseline_progress.processed_records)} / {format_count(@snapshot.baseline_progress.total_records)} Pair 记录
                    · {@snapshot.baseline_progress.completed_groups} / {@snapshot.baseline_progress.total_groups} Case/Metric 组
                  </small>
                  <small :if={Map.get(@snapshot.baseline_progress, :records_per_second)}>
                    {format_rate(Map.get(@snapshot.baseline_progress, :records_per_second))}
                    · 预计剩余 {format_duration(Map.get(@snapshot.baseline_progress, :eta_seconds))}
                    · {Map.get(@snapshot.baseline_progress, :max_concurrency, 1)} 路并发
                  </small>
                  <code :if={@snapshot.baseline_progress.case_id}>
                    {@snapshot.baseline_progress.case_id} / {@snapshot.baseline_progress.metric_id}
                  </code>
                </div>
                <table :if={@snapshot.baseline} class="baseline-table">
                  <thead><tr><th>Case / Metric</th><th>Target</th><th>Development</th><th>vs Target</th><th>Noise</th><th>Pairs</th></tr></thead>
                  <tbody>
                    <tr :for={metric <- visible_baseline_metrics(@snapshot.baseline.metrics)}>
                      <td>{metric.case_id} / {metric.metric_id}</td>
                      <td>{format_number(Map.get(metric, :target_value, Map.get(metric, :baseline_value)))} {metric.unit}</td>
                      <td>{format_number(metric.value)} {metric.unit}</td>
                      <td>{format_percent(Map.get(metric, :target_relative_improvement, Map.get(metric, :improvement_ratio)))}</td>
                      <td>{format_percent(metric.noise_tolerance)}</td>
                      <td>{metric.valid_pair_count}/{metric.pair_count}</td>
                    </tr>
                  </tbody>
                </table>
                <p :if={@snapshot.baseline && hidden_baseline_metric_count(@snapshot.baseline.metrics) > 0} class="case-list-note">
                  为保持界面流畅，仅显示前 {benchmark_case_display_limit()} 个 Cases 的 Baseline；另有 {hidden_baseline_metric_count(@snapshot.baseline.metrics)} 条 Metric 结果未展开。
                </p>
                <p :if={@snapshot.baseline_error} class="missing">{@snapshot.baseline_error}</p>
              </div>
            </details>

            <details class="review-section" open>
              <summary>
                <span class="review-step">5</span>
                <span>
                  <strong>实现角色、源码与运行证据</strong>
                  <small>{implementation_review_path(@snapshot, @implementation_review)}</small>
                </span>
                <span class="review-check">
                  {progress_mark(implementation_review_complete?(@snapshot, @implementation_review, @snapshot.implementation_review_evidence, @reviewed_target_digest, @reviewed_development_sha, @reviewed_implementation_evidence_digest))}
                </span>
              </summary>
              <div class="review-content">
                <button
                  :if={@snapshot.status in [:drafting_spec, :awaiting_confirmation]}
                  class="text-action"
                  phx-click="prepare_change"
                  phx-value-target="implementation_review"
                >要求 Agent 修改或重跑</button>
                <div :if={@snapshot.target_progress} class="reference-run-pending">
                  正在准备 Optimization Target：{target_progress_label(@snapshot.target_progress)}
                </div>
                <div :if={@implementation_review} id="implementation-source-review" class="reference-source-review">
                  <section
                    :for={source <- implementation_sources(@implementation_review)}
                    id={"implementation-source-#{source_key(source)}"}
                    class="implementation-source"
                  >
                    <div class="reference-source-meta">
                      <strong>{implementation_role_label(source.role)}</strong>
                      <code>{source.path}</code>
                      <span>{format_bytes(source.size)}</span>
                      <span>SHA-256 {short_sha(source.sha256)}</span>
                    </div>
                    <pre
                      id={"implementation-source-code-#{source_key(source)}"}
                      phx-hook="ReferenceSyntaxHighlight"
                      data-language={reference_language(source.path)}
                    ><code>{source.content}</code></pre>
                    <p :if={source.truncated} class="reference-source-warning">
                      页面仅预览前 {format_bytes(source.preview_bytes)}；勾选前请在 Workspace 中审阅完整文件。
                    </p>
                  </section>
                  <section
                    :if={@snapshot.implementation_review_evidence}
                    id="implementation-run-evidence"
                    class="reference-run-evidence"
                  >
                    <header>
                      <div>
                        <p class="eyebrow">Implementation Review Evidence</p>
                        <strong>{@snapshot.implementation_review_evidence.case_name}</strong>
                      </div>
                      <span>
                        exit {@snapshot.implementation_review_evidence.exit_code} · {short_sha(@snapshot.implementation_review_evidence.digest)}
                      </span>
                    </header>
                    <dl>
                      <div>
                        <dt>Case</dt>
                        <dd><code>{@snapshot.implementation_review_evidence.case_id}</code></dd>
                      </div>
                      <div>
                        <dt>Environment</dt>
                        <dd>{@snapshot.implementation_review_evidence.environment}</dd>
                      </div>
                      <div>
                        <dt>Output</dt>
                        <dd><code>{@snapshot.implementation_review_evidence.output_artifact}</code></dd>
                      </div>
                      <div><dt>Correctness</dt><dd>Target ✓ · Development ✓</dd></div>
                    </dl>
                    <div class="reference-run-command">
                      <span>实际执行命令</span>
                      <code>{@snapshot.implementation_review_evidence.command}</code>
                    </div>
                    <table class="reference-run-metrics">
                      <thead>
                        <tr><th>Metric</th><th>Target</th><th>Development</th><th>Samples</th></tr>
                      </thead>
                      <tbody>
                        <tr :for={metric <- @snapshot.implementation_review_evidence.metrics}>
                          <td>
                            <strong>{metric.name}</strong>
                            <small><code>{metric.metric_id}</code> · {direction_label(metric.direction)}</small>
                          </td>
                          <td>{format_number(metric.target_value)} {metric.unit}</td>
                          <td>{format_number(metric.development_value)} {metric.unit}</td>
                          <td>{metric.sample_count}</td>
                        </tr>
                      </tbody>
                    </table>
                    <p>{@snapshot.implementation_review_evidence.summary}</p>
                  </section>
                  <p
                    :if={is_nil(@snapshot.implementation_review_evidence)}
                    id="implementation-run-pending"
                    class="reference-run-pending"
                  >
                    等待 Agent 在同一个 Benchmark Case 上运行 Target 与 Development，校验两者正确性并提交配对性能 Metric。
                  </p>
                  <label
                    :if={@snapshot.status == :awaiting_confirmation && @snapshot.implementation_review_evidence}
                    class="reference-review-ack"
                  >
                    <input
                      id="implementation-review-ack"
                      type="checkbox"
                      checked={implementation_reviewed?(@implementation_review, @snapshot.implementation_review_evidence, @reviewed_target_digest, @reviewed_development_sha, @reviewed_implementation_evidence_digest)}
                      phx-click="toggle_implementation_review"
                    />
                    <span>我已审阅 Oracle、固定 Target、可变 Development 的源码和配对证据，并同意以该 Target 建立 Baseline</span>
                  </label>
                  <p
                    :if={@snapshot.implementation_review_evidence && @snapshot.status in [:resolving_references, :building_baseline, :selecting_iteration_sample, :optimizing]}
                    class="reference-reviewed-status"
                  >Optimization Target 与审阅证据已冻结；Development 将继续优化。</p>
                </div>
                <p :if={@implementation_review_error} class="missing">
                  实现源码无法安全读取：{@implementation_review_error}
                </p>
                <p :if={is_nil(@snapshot.harness)} class="empty-copy">
                  等待 Agent 提交 Oracle 与 Harness。
                </p>
                <p
                  :if={not is_nil(@snapshot.harness) && is_nil(@snapshot.target_snapshot)}
                  id="implementation-bundle-pending"
                  class="empty-copy"
                >
                  等待 Agent 固化 Optimization Target、绑定 Development 提交并提交运行证据。
                </p>
              </div>
            </details>

            <details class="review-section">
              <summary>
                <span class="review-step">6</span>
                <span><strong>Reference Projects</strong><small>{selected_reference_count(@snapshot)} / {length(@snapshot.references)} selected</small></span>
                <span class="review-check">{progress_mark(selected_reference_count(@snapshot) > 0)}</span>
              </summary>
              <div class="review-content">
                <div class="reference-list">
                  <div
                    :for={reference <- @snapshot.references}
                    id={"reference-project-#{reference.id}"}
                    class={["reference-row", user_reference_project?(reference) && "reference-row-user"]}
                  >
                    <input
                      id={"reference-project-toggle-#{reference.id}"}
                      type="checkbox"
                      checked={reference.selected}
                      phx-click="toggle_reference"
                      phx-value-id={reference.id}
                      disabled={@snapshot.status not in [:drafting_spec, :awaiting_confirmation]}
                    />
                    <label for={"reference-project-toggle-#{reference.id}"}>
                      <strong>
                        {reference.id}
                        <span :if={user_reference_project?(reference)} class="reference-user-badge">用户添加</span>
                      </strong>
                      <small>{reference.description}</small>
                      <small :if={user_reference_project?(reference)} class="reference-repo-url" title={reference.url}>
                        {reference.url}
                      </small>
                    </label>
                    <span class="reference-row-actions">
                      <code>{reference_status(reference)}</code>
                      <button
                        :if={user_reference_project?(reference) && @snapshot.status in [:drafting_spec, :awaiting_confirmation]}
                        type="button"
                        class="row-action danger"
                        phx-click="remove_reference_project"
                        phx-value-id={reference.id}
                        aria-label={"删除 Reference Project #{reference.id}"}
                      >删除</button>
                    </span>
                  </div>
                </div>
                <.form
                  :if={@snapshot.status in [:drafting_spec, :awaiting_confirmation]}
                  for={@reference_project_form}
                  id="reference-project-form"
                  phx-submit="add_reference_project"
                  class="reference-project-form"
                >
                  <header>
                    <strong>添加 Git Repository</strong>
                    <small>确认 Campaign Spec 时会解析默认分支，并冻结到具体 commit SHA。</small>
                  </header>
                  <label class="wide">
                    <span>Git URL 或绝对路径</span>
                    <input
                      type="text"
                      name={@reference_project_form[:url].name}
                      value={@reference_project_form[:url].value}
                      placeholder="https://github.com/org/repo.git 或 git@host:org/repo.git"
                      autocomplete="off"
                      required
                    />
                  </label>
                  <label>
                    <span>Project ID（可选）</span>
                    <input
                      type="text"
                      name={@reference_project_form[:id].name}
                      value={@reference_project_form[:id].value}
                      placeholder="留空则从仓库名生成"
                      autocomplete="off"
                    />
                  </label>
                  <label>
                    <span>说明（可选）</span>
                    <input
                      type="text"
                      name={@reference_project_form[:description].name}
                      value={@reference_project_form[:description].value}
                      placeholder="这个项目可提供什么参考"
                      autocomplete="off"
                    />
                  </label>
                  <button class="secondary" type="submit">添加并选中</button>
                </.form>
              </div>
            </details>

            <section :if={@show_diff && @snapshot.spec_diff} class="spec-diff-panel">
              <div><strong>Spec diff</strong><button class="row-action" phx-click="toggle_diff">关闭</button></div>
              <p>变化字段：{Enum.join(@snapshot.spec_diff.changed_paths, "、")}</p>
              <pre>{Jason.encode!(@snapshot.spec_diff.to, pretty: true)}</pre>
            </section>
          </div>

          <div class="review-actions">
            <div :if={@snapshot.status == :resolving_references} class="review-action-status">
              <strong>正在准备 Reference 仓库</strong>
              <span>{reference_progress_label(@snapshot)}</span>
            </div>
            <div
              :if={confirmation_blocker(@snapshot, @implementation_review, @implementation_review_error, @reviewed_target_digest, @reviewed_development_sha, @reviewed_implementation_evidence_digest)}
              class="review-action-error"
            >
              {confirmation_blocker(@snapshot, @implementation_review, @implementation_review_error, @reviewed_target_digest, @reviewed_development_sha, @reviewed_implementation_evidence_digest)}
            </div>
            <div
              :if={@snapshot.status == :awaiting_confirmation && @snapshot.last_error}
              class="review-action-error"
            >
              {@snapshot.last_error}
            </div>
            <button class="secondary" phx-click="toggle_diff" disabled={is_nil(@snapshot.spec_diff)}>
              {if @show_diff, do: "收起 Spec diff", else: "查看 Spec diff"}
            </button>
            <button
              :if={@snapshot.status == :building_baseline}
              class="secondary"
              phx-click="prepare_change"
              phx-value-target="baseline"
            >返回修改 Baseline 定义</button>
            <button
              class="primary"
              phx-click="confirm_spec"
              disabled={not confirmable?(@snapshot, @implementation_review, @reviewed_target_digest, @reviewed_development_sha, @reviewed_implementation_evidence_digest)}
            >{confirmation_button_label(@snapshot)}</button>
          </div>
        </aside>
      </div>
    </main>
    """
  end

  defp message_form(body \\ "", intent \\ "conversation"),
    do: to_form(%{"body" => body, "intent" => intent}, as: :message)

  defp reference_project_form(params \\ %{}) do
    defaults = %{"id" => "", "url" => "", "description" => ""}
    to_form(Map.merge(defaults, params), as: :reference_project)
  end

  defp uploads_ready?(entries), do: Enum.all?(entries, &(&1.progress == 100))

  defp assign_result(socket, :ok), do: assign(socket, :flash_message, nil)
  defp assign_result(socket, {:ok, _}), do: assign(socket, :flash_message, nil)
  defp assign_result(socket, error), do: assign(socket, :flash_message, inspect(error))

  defp assign_reference_project_result(socket, :ok), do: assign_result(socket, :ok)

  defp assign_reference_project_result(
         socket,
         {:error, {:invalid_reference_project, field, reason}}
       ) do
    assign(socket, :flash_message, reference_project_error(field, reason))
  end

  defp assign_reference_project_result(
         socket,
         {:error, {:duplicate_reference_project, field, value}}
       ) do
    label = if field == :id, do: "Project ID", else: "Git URL"
    assign(socket, :flash_message, "#{label} 已存在：#{value}")
  end

  defp assign_reference_project_result(socket, error), do: assign_result(socket, error)

  defp empty_snapshot do
    snapshot = %{
      campaign_id: nil,
      status: :initializing,
      backend: :none,
      agent_responding: false,
      pending_question: nil,
      messages: [],
      spec: %{},
      spec_diff: nil,
      spec_ready: false,
      missing: [],
      spec_errors: [],
      references: [],
      reference_progress: nil,
      harness: nil,
      reference_review_evidence: nil,
      implementation_review_evidence: nil,
      prepared_setup_sha: nil,
      target_snapshot: nil,
      target_progress: nil,
      baseline: nil,
      baseline_retry_count: 0,
      baseline_error: nil,
      baseline_progress: nil,
      iteration_sampling: nil,
      sampling_revisions: [],
      required_operations: [],
      best_sha: nil,
      last_error: "Alignment Campaign 尚未启动。",
      workspace: %{root: System.tmp_dir!()}
    }

    if Application.get_env(:pika, :runtime_mode, :preview) == :serve and
         Process.whereis(Pika.Runtime) do
      runtime = Pika.Runtime.snapshot()

      bootstrap_status =
        if Process.whereis(Pika.CampaignBootstrap),
          do: Pika.CampaignBootstrap.status(),
          else: :starting

      {status, message} =
        case {runtime.recovery, bootstrap_status} do
          {%{"status" => "blocked", "reason" => reason}, _} ->
            {:blocked, "Alignment Campaign 恢复已阻止：#{reason}"}

          {_, {:blocked, reason}} ->
            {:blocked, "Alignment Campaign 恢复已阻止：#{reason}"}

          {_, {:error, reason}} ->
            {:initializing, "Alignment Campaign 初始化失败：#{inspect(reason)}"}

          _ ->
            {:initializing, "Alignment Campaign 正在解析固定的 Reference 与 Skill 版本。"}
        end

      %{
        snapshot
        | status: status,
          campaign_id: runtime.campaign.id,
          workspace: %{root: runtime.workspace.root},
          last_error: message
      }
    else
      snapshot
    end
  end

  defp authenticated?(session) do
    if Application.get_env(:pika, :runtime_mode, :preview) == :serve do
      Pika.Auth.authenticated_marker?(session["pika_auth"])
    else
      Pika.PreviewAuth.authenticated_marker?(session["preview_auth"])
    end
  end

  defp product_name do
    if Application.get_env(:pika, :runtime_mode, :preview) == :serve,
      do: "Pika",
      else: "Pika Preview"
  end

  defp product_subtitle do
    if Application.get_env(:pika, :runtime_mode, :preview) == :serve,
      do: "Alignment → GPU Baseline",
      else: "Alignment → GPU Baseline Preview"
  end

  defp change_prompt("boundary"), do: "请修改目标边界："
  defp change_prompt("metrics"), do: "请重新检查并修改 Metrics："
  defp change_prompt("cases"), do: "请重新检查并修改 Benchmark Cases："
  defp change_prompt("measurement"), do: "请修改测量、正确性或停止规则："

  defp change_prompt("baseline"),
    do: "请返回上一步并修改 Oracle、Optimization Target、Development 或 Harness："

  defp change_prompt("implementation_review"),
    do: "请修改实现角色/Harness 或重新运行 Review Case，并提交新的配对性能证据："

  defp change_prompt("metric:" <> id), do: "请修改 Metric `#{id}`："
  defp change_prompt("case:" <> id), do: "请修改 Benchmark Case `#{id}`："
  defp change_prompt(_target), do: "请修改 Campaign Spec："

  defp spec_title(spec), do: spec["title"] || "定义 Kernel 优化边界"
  defp nested(map, keys), do: get_in(map, keys) || "待确认"

  defp formal_pair_label(spec) do
    case get_in(spec, ["benchmark", "pair_count"]) do
      count when is_integer(count) and count > 0 -> "#{count} Pair"
      _ -> "等待用户指定"
    end
  end

  defp message_label(:user), do: "你"
  defp message_label(:agent), do: "Boundary Agent"
  defp message_label(_), do: "Pika"
  defp pending_question?(nil, _message), do: false

  defp pending_question?(question, message),
    do: Map.get(message, :question_id) == question.id

  defp activity_status_icon("running"), do: "●"
  defp activity_status_icon("failed"), do: "×"
  defp activity_status_icon(_), do: "✓"
  defp format_time(%DateTime{} = at), do: Calendar.strftime(at, "%H:%M:%S")
  defp format_time(_), do: ""

  defp format_number(value) when is_number(value),
    do: :erlang.float_to_binary(value * 1.0, decimals: 4)

  defp format_number(value), do: to_string(value)

  defp format_percent(value) when is_number(value),
    do: :erlang.float_to_binary(value * 100.0, decimals: 2) <> "%"

  defp format_percent(_), do: "—"

  defp format_percent_number(value) when is_number(value),
    do: :erlang.float_to_binary(value * 1.0, decimals: 1) <> "%"

  defp format_percent_number(_), do: "—"

  defp format_count(value) when is_integer(value) and value >= 1_000_000_000,
    do: :erlang.float_to_binary(value / 1_000_000_000, decimals: 2) <> "B"

  defp format_count(value) when is_integer(value) and value >= 1_000_000,
    do: :erlang.float_to_binary(value / 1_000_000, decimals: 1) <> "M"

  defp format_count(value) when is_integer(value) and value >= 1_000,
    do: :erlang.float_to_binary(value / 1_000, decimals: 1) <> "K"

  defp format_count(value) when is_integer(value), do: Integer.to_string(value)
  defp format_count(_), do: "—"

  defp format_rate(value) when is_number(value) and value >= 0,
    do: "#{format_count(round(value))} 条/秒"

  defp format_rate(_), do: "—"

  defp format_duration(value) when is_number(value) and value >= 0 do
    seconds = round(value)
    hours = div(seconds, 3_600)
    minutes = div(rem(seconds, 3_600), 60)
    seconds = rem(seconds, 60)

    cond do
      hours > 0 -> "#{hours}小时#{minutes}分"
      minutes > 0 -> "#{minutes}分#{seconds}秒"
      true -> "#{seconds}秒"
    end
  end

  defp format_duration(_), do: "—"

  defp format_bytes(bytes) when is_integer(bytes) and bytes >= 1_048_576,
    do: "#{Float.round(bytes / 1_048_576, 1)} MB"

  defp format_bytes(bytes) when is_integer(bytes) and bytes >= 1024,
    do: "#{Float.round(bytes / 1024, 1)} KB"

  defp format_bytes(bytes) when is_integer(bytes), do: "#{bytes} B"
  defp format_bytes(_), do: ""

  defp status_kind(status) when status in [:optimizing, :awaiting_confirmation], do: "active"
  defp status_kind(_), do: "neutral"

  defp status_label(status) do
    %{
      initializing: "Initializing",
      blocked: "Blocked",
      drafting_spec: "DraftingSpec",
      awaiting_confirmation: "AwaitingConfirmation",
      resolving_references: "Resolving References",
      building_baseline: "BuildingBaseline",
      selecting_iteration_sample: "SelectingIterationSample",
      optimizing: "Optimizing · Preview stops here"
    }[status] || to_string(status)
  end

  defp review_status_kind(status) when status in [:awaiting_confirmation, :optimizing],
    do: "active"

  defp review_status_kind(_), do: "neutral"

  defp review_status_label(:drafting_spec), do: "Agent 整理中"
  defp review_status_label(:awaiting_confirmation), do: "草稿可确认"
  defp review_status_label(:resolving_references), do: "解析 References"
  defp review_status_label(:building_baseline), do: "建立全量 Baseline"
  defp review_status_label(:selecting_iteration_sample), do: "选择 Iteration Sample"
  defp review_status_label(:optimizing), do: "Baseline 已完成"
  defp review_status_label(_), do: "准备中"

  defp review_subtitle(snapshot) do
    "#{length(cases(snapshot))} Cases · #{length(metrics(snapshot))} Metrics · #{selected_reference_count(snapshot)} References"
  end

  defp progress_mark(true), do: "✓"
  defp progress_mark(false), do: "○"
  defp baseline_flow_steps, do: @baseline_flow_steps

  defp baseline_flow_visible?(snapshot) do
    snapshot.status in [
      :resolving_references,
      :building_baseline,
      :selecting_iteration_sample,
      :optimizing
    ]
  end

  defp baseline_flow_current_step(%{status: :resolving_references}), do: 1

  defp baseline_flow_current_step(%{status: :building_baseline, baseline_progress: progress})
       when not is_nil(progress),
       do: 4

  defp baseline_flow_current_step(%{status: :building_baseline} = snapshot) do
    if "submit_baseline" in Map.get(snapshot, :required_operations, []), do: 3, else: 2
  end

  defp baseline_flow_current_step(%{status: :selecting_iteration_sample}), do: 5
  defp baseline_flow_current_step(%{status: :optimizing}), do: length(@baseline_flow_steps) + 1
  defp baseline_flow_current_step(_snapshot), do: 1

  defp baseline_flow_completed_steps(snapshot) do
    snapshot
    |> baseline_flow_current_step()
    |> Kernel.-(1)
    |> max(0)
    |> min(length(@baseline_flow_steps))
  end

  defp baseline_flow_summary(snapshot) do
    completed = baseline_flow_completed_steps(snapshot)
    total = length(@baseline_flow_steps)

    if completed == total,
      do: "全部 #{total} 步已完成",
      else: "已完成 #{completed} / #{total} · 还剩 #{total - completed} 步"
  end

  defp baseline_flow_step_state(snapshot, index) do
    current = baseline_flow_current_step(snapshot)

    cond do
      index < current -> "done"
      index == current -> "current"
      true -> "pending"
    end
  end

  defp baseline_flow_step_state_label(snapshot, index) do
    case baseline_flow_step_state(snapshot, index) do
      "done" -> "已完成"
      "current" -> "进行中"
      "pending" -> "待执行"
    end
  end

  defp baseline_flow_step_mark(snapshot, index) do
    if baseline_flow_step_state(snapshot, index) == "done", do: "✓", else: index
  end

  defp baseline_flow_step_detail(:references, %{status: :resolving_references} = snapshot),
    do: reference_progress_label(snapshot)

  defp baseline_flow_step_detail(:references, _snapshot),
    do: "解析固定版本，并发准备所选 Reference 仓库"

  defp baseline_flow_step_detail(:setup_merge, _snapshot),
    do: "核验已审阅的 Development tree 与固定 Target 身份，并合入 pika/best"

  defp baseline_flow_step_detail(:measure, %{baseline_error: error}) when is_binary(error),
    do: "上次提交未通过；Agent 正在修正 Artifact 并重新测量"

  defp baseline_flow_step_detail(:measure, _snapshot),
    do: "执行全部 Cases × Metrics，并在本地生成 Pair Artifact"

  defp baseline_flow_step_detail(:validate, %{baseline_progress: progress})
       when not is_nil(progress) do
    "#{baseline_phase_label(progress.phase)} · #{format_percent_number(baseline_progress_percent(progress))}"
  end

  defp baseline_flow_step_detail(:validate, _snapshot),
    do: "流式读取 Pair，依次校验指标、Correctness 与 Profiler"

  defp baseline_flow_step_detail(:iteration_sample, snapshot),
    do: "从 #{length(cases(snapshot))} 个 Cases 中选择首轮优化样本"

  defp computation_ready?(spec) do
    is_map(spec["implementations"]) and
      is_binary(get_in(spec, ["computation", "fusion_scope"])) and
      is_binary(spec["target_hardware"])
  end

  defp oracle_label(spec) do
    case get_in(spec, ["implementations", "oracle"]) do
      %{"kind" => "optimization_target"} -> "Optimization Target"
      %{"kind" => "repository_path", "entrypoint" => path} -> path
      _ -> "待确认"
    end
  end

  defp target_label(spec) do
    case get_in(spec, ["implementations", "optimization_target"]) do
      %{"source" => %{"kind" => "development_snapshot"}, "entrypoint" => path} ->
        "已审阅 Development 提交快照 · #{path}"

      %{
        "source" => %{"kind" => "reference_project", "reference_id" => id},
        "entrypoint" => path
      } ->
        "#{id} · #{path}"

      _ ->
        "等待 Optimization Target"
    end
  end

  defp metrics(snapshot), do: List.wrap(snapshot.spec["metrics"])
  defp cases(snapshot), do: List.wrap(snapshot.spec["benchmark_cases"])
  defp benchmark_case_display_limit, do: @benchmark_case_display_limit
  defp visible_cases(snapshot), do: Enum.take(cases(snapshot), @benchmark_case_display_limit)

  defp hidden_case_count(snapshot),
    do: max(length(cases(snapshot)) - @benchmark_case_display_limit, 0)

  defp visible_baseline_metrics(metrics) do
    visible_case_ids =
      metrics
      |> Enum.map(& &1.case_id)
      |> Enum.uniq()
      |> Enum.take(@benchmark_case_display_limit)
      |> MapSet.new()

    Enum.filter(metrics, &MapSet.member?(visible_case_ids, &1.case_id))
  end

  defp hidden_baseline_metric_count(metrics),
    do: max(length(metrics) - length(visible_baseline_metrics(metrics)), 0)

  defp baseline_progress_percent(%{phase: phase})
       when phase in [:validating_correctness, :validating_profiler, :completed],
       do: 100.0

  defp baseline_progress_percent(%{processed_records: processed, total_records: total})
       when is_integer(processed) and is_integer(total) and total > 0,
       do: min(processed / total * 100.0, 100.0)

  defp baseline_progress_percent(_progress), do: 0.0

  defp baseline_phase_label(:queued), do: "准备校验 Baseline"
  defp baseline_phase_label(:registering_artifacts), do: "登记本地 Artifact Manifest"
  defp baseline_phase_label(:reading_samples), do: "流式校验 Pair JSONL"
  defp baseline_phase_label(:validating_correctness), do: "校验 Correctness"
  defp baseline_phase_label(:validating_profiler), do: "校验 Profiler"
  defp baseline_phase_label(:completed), do: "Baseline 校验完成"
  defp baseline_phase_label(_phase), do: "校验 Baseline"

  defp shape_source(snapshot), do: get_in(snapshot.spec, ["benchmark", "shape_source"])

  defp case_count_label(snapshot) do
    full = length(cases(snapshot))
    sampled = snapshot.iteration_sampling && length(snapshot.iteration_sampling.case_ids)

    if sampled, do: "全量 #{full} · Iteration #{sampled}", else: "全量 #{full} · Baseline 后自动采样"
  end

  defp sampled?(%{iteration_sampling: nil}, _case_id), do: false
  defp sampled?(snapshot, case_id), do: case_id in snapshot.iteration_sampling.case_ids

  defp sampling_reason(%{iteration_sampling: nil}, _case_id), do: nil
  defp sampling_reason(snapshot, case_id), do: snapshot.iteration_sampling.reasons[case_id]

  defp shape_source_label(source, selected_count) do
    count = source["record_count"] || "未知数量"
    path = source["path"] || source["kind"]
    "#{path} · 共 #{count} 条 · Campaign 确认 #{selected_count} 个具体 Cases"
  end

  defp shape_pairs(shape) when is_map(shape), do: Enum.sort_by(shape, &elem(&1, 0))
  defp shape_pairs(_shape), do: []

  defp human_key(key) do
    key
    |> to_string()
    |> String.replace("_", " ")
  end

  defp format_value(value) when is_binary(value), do: value
  defp format_value(value) when is_number(value) or is_boolean(value), do: to_string(value)
  defp format_value(nil), do: "—"

  defp format_value(value) when is_list(value) do
    preview = value |> Enum.take(4) |> Enum.map_join(", ", &format_value/1)
    if length(value) > 4, do: "[#{preview}, …] (#{length(value)})", else: "[#{preview}]"
  end

  defp format_value(value) when is_map(value) do
    value
    |> Enum.sort_by(&elem(&1, 0))
    |> Enum.map_join(" · ", fn {key, item} -> "#{human_key(key)} #{format_value(item)}" end)
  end

  defp format_value(value), do: inspect(value)

  defp format_contract(value) when is_list(value) and value != [] do
    Enum.map_join(value, " · ", fn item ->
      name = item["name"] || "value"
      details = Map.drop(item, ["name"]) |> format_value()
      "#{name}: #{details}"
    end)
  end

  defp format_contract(_value), do: "待确认"

  defp correctness_summary(spec) do
    correctness = get_in(spec, ["computation", "correctness"]) || %{}

    if is_number(correctness["rtol"]) and is_number(correctness["atol"]) do
      "rtol #{correctness["rtol"]} · atol #{correctness["atol"]}"
    else
      "待确认"
    end
  end

  defp role_label("target"), do: "TARGET"
  defp role_label("guard"), do: "GUARD"
  defp role_label("informational"), do: "INFO"
  defp role_label(value), do: value || "DRAFT"

  defp direction_label("minimize"), do: "↓ 越低越好"
  defp direction_label("maximize"), do: "↑ 越高越好"
  defp direction_label(value), do: value || "待确认"

  defp format_weight(value) when is_number(value) and value <= 1,
    do: format_percent(value)

  defp format_weight(value), do: to_string(value)

  defp stopping_summary(spec) do
    stopping = spec["stopping"] || %{}

    cond do
      is_integer(stopping["max_attempts"]) ->
        "最多 #{stopping["max_attempts"]} Attempts · #{stopping["mode"]}"

      List.wrap(stopping["metric_goals"]) != [] ->
        "#{length(stopping["metric_goals"])} Metric goals · #{stopping["mode"]}"

      true ->
        "待确认"
    end
  end

  defp selected_reference_count(snapshot), do: Enum.count(snapshot.references, & &1.selected)

  defp spec_editable?(snapshot),
    do: snapshot.status in [:drafting_spec, :awaiting_confirmation, :building_baseline]

  defp spec_revision(snapshot) do
    case get_in(snapshot, [:spec, "revision"]) do
      revision when is_integer(revision) and revision > 0 -> revision
      _ -> 1
    end
  end

  defp confirmable?(
         snapshot,
         implementation_review,
         reviewed_target_digest,
         reviewed_development_sha,
         reviewed_evidence_digest
       ),
       do:
         snapshot.status == :awaiting_confirmation and snapshot.spec_ready and
           not is_nil(snapshot.harness) and alignment_idle?(snapshot) and
           implementation_reviewed?(
             implementation_review,
             snapshot.implementation_review_evidence,
             reviewed_target_digest,
             reviewed_development_sha,
             reviewed_evidence_digest
           )

  defp confirmation_button_label(%{status: :resolving_references}),
    do: "正在准备 References…"

  defp confirmation_button_label(%{status: status})
       when status in [:building_baseline, :selecting_iteration_sample, :optimizing],
       do: "已确认，正在建立 Baseline"

  defp confirmation_button_label(%{status: :awaiting_confirmation} = snapshot) do
    if alignment_idle?(snapshot), do: "确认并建立 Baseline", else: "等待 Agent 完成本轮…"
  end

  defp confirmation_button_label(_snapshot), do: "确认并建立 Baseline"

  defp confirmation_blocker(
         snapshot,
         implementation_review,
         implementation_review_error,
         reviewed_target_digest,
         reviewed_development_sha,
         reviewed_evidence_digest
       ) do
    cond do
      snapshot.status != :awaiting_confirmation ->
        nil

      not is_nil(snapshot.pending_question) ->
        "Agent 正在等待你的回答；完成当前问题后才能确认 Campaign Spec。"

      snapshot.agent_responding or is_binary(snapshot.active_turn_id) ->
        "Agent 仍在生成或执行本轮工作；请等待本轮结束，再审阅最终内容并确认。"

      not snapshot.spec_ready ->
        "Campaign Spec 尚未通过校验，修正右侧未完成项后才能确认。"

      is_nil(snapshot.harness) ->
        "Benchmark Harness 尚未就绪，暂时不能建立 Baseline。"

      not is_nil(snapshot.target_progress) ->
        "Optimization Target 正在固化，完成后才能审阅和确认。"

      is_nil(snapshot.target_snapshot) ->
        "Optimization Target 尚未固化，暂时不能建立 Baseline。"

      is_binary(implementation_review_error) ->
        "实现源码读取或身份校验失败，修复并重新提交后才能确认。"

      is_nil(implementation_review) ->
        "Oracle、Target 或 Development 源码尚未加载，暂时不能建立 Baseline。"

      is_nil(snapshot.implementation_review_evidence) ->
        "Target 与 Development 尚未同时通过正确性校验并提交配对性能证据。"

      not implementation_reviewed?(
        implementation_review,
        snapshot.implementation_review_evidence,
        reviewed_target_digest,
        reviewed_development_sha,
        reviewed_evidence_digest
      ) ->
        "请先审阅并确认 Oracle、固定 Target、Development 源码与配对性能指标。"

      true ->
        nil
    end
  end

  defp alignment_idle?(snapshot) do
    not snapshot.agent_responding and is_nil(snapshot.active_turn_id) and
      is_nil(snapshot.pending_question)
  end

  defp implementation_review_path(snapshot, nil),
    do: target_label(snapshot.spec)

  defp implementation_review_path(_snapshot, review) do
    "Target #{review.target.path} · Development #{review.development.path}"
  end

  defp reference_language(path) when is_binary(path) do
    filename = path |> Path.basename() |> String.downcase()

    Map.get(@reference_language_filenames, filename) ||
      Map.get(@reference_languages, path |> Path.extname() |> String.downcase(), "plaintext")
  end

  defp reference_language(_path), do: "plaintext"

  defp implementation_reviewed?(
         %{target_snapshot: %{digest: target_digest}},
         %{digest: evidence_digest, development_sha: development_sha},
         target_digest,
         development_sha,
         evidence_digest
       ),
       do: true

  defp implementation_reviewed?(
         _review,
         _evidence,
         _reviewed_target,
         _reviewed_development,
         _reviewed_evidence
       ),
       do: false

  defp implementation_review_complete?(
         snapshot,
         implementation_review,
         evidence,
         reviewed_target_digest,
         reviewed_development_sha,
         reviewed_evidence_digest
       ) do
    if snapshot.status in [
         :resolving_references,
         :building_baseline,
         :selecting_iteration_sample,
         :optimizing
       ] do
      not is_nil(implementation_review) and not is_nil(evidence)
    else
      implementation_reviewed?(
        implementation_review,
        evidence,
        reviewed_target_digest,
        reviewed_development_sha,
        reviewed_evidence_digest
      )
    end
  end

  defp implementation_sources(review) do
    if review.oracle.sha256 == review.target.sha256 and review.oracle.path == review.target.path do
      [Map.put(review.target, :role, :target_and_oracle), review.development]
    else
      [review.oracle, review.target, review.development]
    end
  end

  defp implementation_role_label(:target_and_oracle),
    do: "Optimization Target + Correctness Oracle"

  defp implementation_role_label(:optimization_target), do: "Optimization Target"
  defp implementation_role_label(:development), do: "Development Implementation"
  defp implementation_role_label(:oracle), do: "Correctness Oracle"
  defp implementation_role_label(role), do: to_string(role)

  defp source_key(%{role: role}), do: role |> to_string() |> String.replace("_", "-")

  defp short_sha(sha256) when is_binary(sha256), do: String.slice(sha256, 0, 16) <> "…"
  defp short_sha(_sha256), do: "—"

  defp reference_progress_label(%{
         reference_progress: %{id: id, completed: completed, total: total}
       })
       when is_binary(id),
       do: "#{completed}/#{total} 已完成 · 当前 #{id}"

  defp reference_progress_label(%{
         reference_progress: %{completed: completed, total: total}
       }),
       do: "#{completed}/#{total} 已完成"

  defp reference_progress_label(_snapshot), do: "正在恢复准备进度"

  defp target_progress_label(%{phase: :queued}), do: "等待后台任务"
  defp target_progress_label(%{phase: :resolving_source}), do: "解析固定源码版本"

  defp target_progress_label(%{phase: :materializing_source} = progress) do
    id = Map.get(progress, :id)
    completed = Map.get(progress, :completed, 0)
    total = Map.get(progress, :total, 1)
    "clone #{id || "repository"} · #{completed}/#{total}"
  end

  defp target_progress_label(%{phase: :freezing_snapshot}), do: "创建独立只读快照"
  defp target_progress_label(_progress), do: "准备中"

  defp reference_status(%{status: :resolved, sha: sha}) when is_binary(sha),
    do: String.slice(sha, 0, 8)

  defp reference_status(%{status: status}), do: to_string(status)

  defp user_reference_project?(reference), do: Map.get(reference, :origin) == :user

  defp reference_project_error(field, :required),
    do: "#{reference_project_field(field)} 不能为空。"

  defp reference_project_error(field, :too_long),
    do: "#{reference_project_field(field)} 太长。"

  defp reference_project_error(:id, :cannot_derive),
    do: "无法从 Git URL 生成 Project ID，请手动填写。"

  defp reference_project_error(field, :invalid_format),
    do: "#{reference_project_field(field)} 格式无效。"

  defp reference_project_error(field, reason),
    do: "#{reference_project_field(field)} 无效：#{reason}"

  defp reference_project_field(:id), do: "Project ID"
  defp reference_project_field(:url), do: "Git URL"
  defp reference_project_field(:description), do: "说明"
  defp reference_project_field(:repository), do: "Git Repository"
  defp reference_project_field(field), do: to_string(field)
end
