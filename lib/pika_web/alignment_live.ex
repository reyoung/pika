defmodule PikaWeb.AlignmentLive do
  use PikaWeb, :live_view

  alias Pika.Stage0.{ArtifactStore, Campaign}
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
      |> assign(:snapshot, snapshot)
      |> assign(:message_form, message_form())
      |> assign(:show_diff, false)
      |> assign(:flash_message, nil)
      |> allow_upload(:inputs, accept: :any, max_entries: 5, max_file_size: 1_073_741_824)

    {:ok, socket}
  end

  @impl true
  def handle_info({:stage0_updated, snapshot}, socket),
    do: {:noreply, assign(socket, :snapshot, snapshot)}

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
    {:noreply, assign_result(socket, Campaign.toggle_reference(id))}
  end

  def handle_event("confirm_spec", _params, socket) do
    {:noreply, assign_result(socket, Campaign.confirm_spec())}
  end

  def handle_event("toggle_diff", _params, socket) do
    {:noreply, update(socket, :show_diff, &not/1)}
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

  @impl true
  def render(assigns) do
    ~H"""
    <main class="stage0-shell">
      <header class="topbar">
        <div class="brand">
          <div class="brand-mark">P</div>
          <div><strong>{product_name()}</strong><small>{product_subtitle()}</small></div>
        </div>
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
            <.pill kind="active">Spec v1</.pill>
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
                <div class="message-label">{message_label(message.role)} · {format_time(message.at)}</div>
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
              </article>
            <% end %>
          </div>

          <div class="composer">
            <.form
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
                  <div><dt>Reference</dt><dd>{nested(@snapshot.spec, ~w(computation reference_path))}</dd></div>
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
                  <article :for={case_ <- cases(@snapshot)} class="case-row">
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
                  <div><span>1</span><strong>全量 Baseline</strong><small>全部 Cases × Metrics · 30 Pair</small></div>
                  <div><span>2</span><strong>Iteration Sample</strong><small>初始最多 10 Cases · 30 Pair</small></div>
                  <div><span>3</span><strong>归并前全量回归</strong><small>全量 5 Pair · 异常升级 30 Pair</small></div>
                </div>
                <dl class="measurement-grid">
                  <div><dt>Harness</dt><dd>{nested(@snapshot.spec, ~w(benchmark harness_path))}</dd></div>
                  <div><dt>Warmup</dt><dd>{nested(@snapshot.spec, ~w(benchmark warmup))}</dd></div>
                  <div><dt>Baseline Pair</dt><dd>{nested(@snapshot.spec, ~w(benchmark pair_count))}</dd></div>
                  <div><dt>Valid Pair</dt><dd>{nested(@snapshot.spec, ~w(benchmark min_valid_pairs))}</dd></div>
                  <div><dt>Retry</dt><dd>{nested(@snapshot.spec, ~w(benchmark retry_limit))}</dd></div>
                  <div><dt>Stopping</dt><dd>{stopping_summary(@snapshot.spec)}</dd></div>
                </dl>
                <div :if={@snapshot.status in [:building_baseline, :selecting_iteration_sample, :optimizing]} class="baseline-progress">
                  <span class={progress_class(not is_nil(@snapshot.baseline))}>全量 Baseline</span>
                  <span class={progress_class(@snapshot.status == :selecting_iteration_sample)}>Agent 选择采样</span>
                  <span class={progress_class(@snapshot.status == :optimizing)}>Optimizing</span>
                </div>
                <table :if={@snapshot.baseline} class="baseline-table">
                  <thead><tr><th>Case / Metric</th><th>Value</th><th>Noise</th><th>Pairs</th></tr></thead>
                  <tbody>
                    <tr :for={metric <- @snapshot.baseline.metrics}>
                      <td>{metric.case_id} / {metric.metric_id}</td>
                      <td>{format_number(metric.value)} {metric.unit}</td>
                      <td>{format_percent(metric.noise_tolerance)}</td>
                      <td>{metric.valid_pair_count}/{metric.pair_count}</td>
                    </tr>
                  </tbody>
                </table>
                <p :if={@snapshot.baseline_error} class="missing">{@snapshot.baseline_error}</p>
              </div>
            </details>

            <details class="review-section">
              <summary>
                <span class="review-step">5</span>
                <span><strong>Reference Projects</strong><small>{selected_reference_count(@snapshot)} / {length(@snapshot.references)} selected</small></span>
                <span class="review-check">{progress_mark(selected_reference_count(@snapshot) > 0)}</span>
              </summary>
              <div class="review-content">
                <div class="reference-list">
                  <label :for={reference <- @snapshot.references} class="reference-row">
                    <input
                      type="checkbox"
                      checked={reference.selected}
                      phx-click="toggle_reference"
                      phx-value-id={reference.id}
                      disabled={@snapshot.status not in [:drafting_spec, :awaiting_confirmation]}
                    />
                    <span><strong>{reference.id}</strong><small>{reference.description}</small></span>
                    <code>{reference_status(reference)}</code>
                  </label>
                </div>
              </div>
            </details>

            <section :if={@show_diff && @snapshot.spec_diff} class="spec-diff-panel">
              <div><strong>Spec diff</strong><button class="row-action" phx-click="toggle_diff">关闭</button></div>
              <p>变化字段：{Enum.join(@snapshot.spec_diff.changed_paths, "、")}</p>
              <pre>{Jason.encode!(@snapshot.spec_diff.to, pretty: true)}</pre>
            </section>
          </div>

          <div class="review-actions">
            <button class="secondary" phx-click="toggle_diff" disabled={is_nil(@snapshot.spec_diff)}>
              {if @show_diff, do: "收起 Spec diff", else: "查看 Spec diff"}
            </button>
            <button
              class="primary"
              phx-click="confirm_spec"
              disabled={@snapshot.status != :awaiting_confirmation}
            >确认并建立 Baseline</button>
          </div>
        </aside>
      </div>
    </main>
    """
  end

  defp message_form(body \\ "", intent \\ "conversation"),
    do: to_form(%{"body" => body, "intent" => intent}, as: :message)

  defp uploads_ready?(entries), do: Enum.all?(entries, &(&1.progress == 100))

  defp assign_result(socket, :ok), do: assign(socket, :flash_message, nil)
  defp assign_result(socket, {:ok, _}), do: assign(socket, :flash_message, nil)
  defp assign_result(socket, error), do: assign(socket, :flash_message, inspect(error))

  defp empty_snapshot do
    snapshot = %{
      campaign_id: nil,
      status: :initializing,
      backend: :none,
      messages: [],
      spec: %{},
      spec_diff: nil,
      spec_ready: false,
      missing: [],
      spec_errors: [],
      references: [],
      harness: nil,
      baseline: nil,
      baseline_retry_count: 0,
      baseline_error: nil,
      iteration_sampling: nil,
      sampling_revisions: [],
      best_sha: nil,
      last_error: "Stage0 Campaign 尚未启动。",
      workspace: %{root: System.tmp_dir!()}
    }

    if Application.get_env(:pika, :runtime_mode, :stage0) == :serve and
         Process.whereis(Pika.Runtime) do
      runtime = Pika.Runtime.snapshot()

      bootstrap_status =
        if Process.whereis(Pika.Phase2.Bootstrap),
          do: Pika.Phase2.Bootstrap.status(),
          else: :starting

      message =
        case bootstrap_status do
          {:error, reason} -> "Alignment Campaign 初始化失败：#{inspect(reason)}"
          _ -> "Alignment Campaign 正在解析固定的 Reference 与 Skill 版本。"
        end

      %{
        snapshot
        | campaign_id: runtime.campaign.id,
          workspace: %{root: runtime.workspace.root},
          last_error: message
      }
    else
      snapshot
    end
  end

  defp authenticated?(session) do
    if Application.get_env(:pika, :runtime_mode, :stage0) == :serve do
      Pika.Auth.authenticated_marker?(session["pika_auth"])
    else
      Pika.Stage0.Auth.authenticated_marker?(session["stage0_auth"])
    end
  end

  defp product_name do
    if Application.get_env(:pika, :runtime_mode, :stage0) == :serve,
      do: "Pika",
      else: "Pika Stage0"
  end

  defp product_subtitle do
    if Application.get_env(:pika, :runtime_mode, :stage0) == :serve,
      do: "Alignment → GPU Baseline",
      else: "Alignment → GPU Baseline Preview"
  end

  defp change_prompt("boundary"), do: "请修改目标边界："
  defp change_prompt("metrics"), do: "请重新检查并修改 Metrics："
  defp change_prompt("cases"), do: "请重新检查并修改 Benchmark Cases："
  defp change_prompt("measurement"), do: "请修改测量、正确性或停止规则："
  defp change_prompt("metric:" <> id), do: "请修改 Metric `#{id}`："
  defp change_prompt("case:" <> id), do: "请修改 Benchmark Case `#{id}`："
  defp change_prompt(_target), do: "请修改 Campaign Spec："

  defp spec_title(spec), do: spec["title"] || "定义 Kernel 优化边界"
  defp nested(map, keys), do: get_in(map, keys) || "待确认"
  defp message_label(:user), do: "你"
  defp message_label(:agent), do: "Boundary Agent"
  defp message_label(_), do: "Pika"
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
  defp progress_class(true), do: "done"
  defp progress_class(false), do: "pending"

  defp computation_ready?(spec) do
    is_binary(get_in(spec, ["computation", "reference_path"])) and
      is_binary(get_in(spec, ["computation", "fusion_scope"])) and
      is_binary(spec["target_hardware"])
  end

  defp metrics(snapshot), do: List.wrap(snapshot.spec["metrics"])
  defp cases(snapshot), do: List.wrap(snapshot.spec["benchmark_cases"])
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
  defp spec_editable?(snapshot), do: snapshot.status in [:drafting_spec, :awaiting_confirmation]

  defp reference_status(%{status: :resolved, sha: sha}) when is_binary(sha),
    do: String.slice(sha, 0, 8)

  defp reference_status(%{status: status}), do: to_string(status)
end
