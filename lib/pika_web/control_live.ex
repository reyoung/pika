defmodule PikaWeb.ControlLive do
  use PikaWeb, :live_view

  @tabs ~w(attempts metrics sync audit)
  @refresh_debounce_ms 50
  @spec_refresh_events ~w(best_advanced sampling_advanced spec_revision_advanced)

  @impl true
  def mount(params, session, socket) do
    if authenticated?(session) do
      params = if(is_map(params), do: params, else: %{})
      campaign_id = Pika.Persistence.current_campaign().id

      if connected?(socket) do
        Phoenix.PubSub.subscribe(Pika.PubSub, Pika.Persistence.topic(campaign_id))
      end

      snapshot = Pika.Dashboard.snapshot(campaign_id)
      tab = if(params["tab"] in @tabs, do: params["tab"], else: "attempts")
      selected = snapshot.attempts |> List.last() |> then(&(&1 && &1.id))
      {remote, branch} = sync_defaults()

      {:ok,
       socket
       |> assign(:campaign_id, campaign_id)
       |> assign(:tabs, @tabs)
       |> assign(:snapshot, snapshot)
       |> assign(:tab, tab)
       |> assign(:selected_attempt_id, selected)
       |> assign(:metric_filter, "all")
       |> assign(:case_filter, "all")
       |> assign(:spec_filter, "all")
       |> assign(:btw_open, false)
       |> assign(:btw_form, to_form(%{"body" => "", "mode" => "chat"}, as: :btw))
       |> assign(:sync_form, to_form(%{"remote" => remote, "branch" => branch}, as: :sync))
       |> assign(:sync_preview, nil)
       |> assign(:stop_armed, false)
       |> assign(:flash_message, nil)
       |> assign(:refresh_timer, nil)
       |> assign(:refresh_spec, false)
       |> assign(:action_keys, action_keys())}
    else
      {:ok, redirect(socket, to: "/")}
    end
  end

  @impl true
  def handle_info({:domain_event, event}, socket),
    do: {:noreply, schedule_refresh(socket, spec_refresh_event?(event))}

  def handle_info(:refresh_snapshot, socket) do
    refresh_spec = socket.assigns.refresh_spec

    {:noreply,
     socket
     |> assign(refresh_timer: nil, refresh_spec: false)
     |> refresh(refresh_spec)}
  end

  @impl true
  def handle_event("select_tab", %{"tab" => tab}, socket) when tab in @tabs,
    do: {:noreply, assign(socket, :tab, tab)}

  def handle_event("select_attempt", %{"id" => id}, socket),
    do: {:noreply, assign(socket, selected_attempt_id: id, btw_open: false)}

  def handle_event("filter_metrics", params, socket) do
    {:noreply,
     assign(socket,
       metric_filter: params["metric_filter"] || "all",
       case_filter: params["case_filter"] || "all",
       spec_filter: params["spec_filter"] || "all"
     )}
  end

  def handle_event("open_btw", _params, socket),
    do: {:noreply, assign(socket, :btw_open, true)}

  def handle_event("close_btw", _params, socket),
    do: {:noreply, assign(socket, :btw_open, false)}

  def handle_event("send_btw", %{"btw" => params}, socket) do
    key = socket.assigns.action_keys.btw

    result =
      Pika.AttemptCoordinator.create_btw(
        socket.assigns.selected_attempt_id,
        params["body"],
        params["mode"],
        key
      )

    case result do
      {:ok, _guidance} ->
        {:noreply,
         socket
         |> refresh()
         |> assign(:btw_form, to_form(%{"body" => "", "mode" => params["mode"]}, as: :btw))
         |> rotate_key(:btw)
         |> assign(:flash_message, "BTW 已记录并按所选作用域处理。")}

      {:error, reason} ->
        {:noreply,
         socket
         |> assign(:btw_form, to_form(params, as: :btw))
         |> assign(:flash_message, inspect(reason))}
    end
  end

  def handle_event("pause", _params, socket), do: control(socket, :pause, &Pika.Control.pause/1)

  def handle_event("resume", _params, socket),
    do: control(socket, :resume, &Pika.Control.resume/1)

  def handle_event("arm_stop", _params, socket),
    do: {:noreply, assign(socket, :stop_armed, true)}

  def handle_event("cancel_stop", _params, socket),
    do: {:noreply, assign(socket, :stop_armed, false)}

  def handle_event("stop_now", _params, socket) do
    case control_result(socket, :stop, &Pika.Control.stop_now/1) do
      {:noreply, socket} -> {:noreply, assign(socket, :stop_armed, false)}
    end
  end

  def handle_event("preview_sync", %{"sync" => params}, socket) do
    form = to_form(params, as: :sync)

    case Pika.SyncCoordinator.preview(params["remote"], params["branch"]) do
      {:ok, preview} ->
        {:noreply,
         socket
         |> assign(:sync_form, form)
         |> assign(:sync_preview, preview)
         |> assign(:flash_message, nil)}

      {:error, reason} ->
        {:noreply,
         socket
         |> assign(:sync_form, form)
         |> assign(:sync_preview, nil)
         |> assign(:flash_message, inspect(reason))}
    end
  end

  def handle_event("confirm_sync", _params, socket) do
    preview = socket.assigns.sync_preview
    key = socket.assigns.action_keys.sync

    result =
      if preview,
        do: Pika.SyncCoordinator.request(preview.remote, preview.branch, key),
        else: {:error, :sync_preview_required}

    case result do
      {:ok, _run} ->
        {:noreply,
         socket
         |> refresh()
         |> assign(:sync_preview, nil)
         |> rotate_key(:sync)
         |> assign(:flash_message, "Sync Run 已创建；新 Attempt dispatch 已关闭。")}

      {:error, reason} ->
        {:noreply, assign(socket, :flash_message, inspect(reason))}
    end
  end

  def handle_event("confirm_sync_spec", %{"decision" => decision}, socket) do
    run = socket.assigns.snapshot.control.sync
    approved = decision == "approve"
    key = socket.assigns.action_keys.sync_spec

    result =
      if run,
        do: Pika.SyncCoordinator.confirm_spec(run.id, approved, key),
        else: {:error, :sync_run_missing}

    case result do
      {:ok, _} ->
        {:noreply,
         socket
         |> refresh()
         |> rotate_key(:sync_spec)
         |> assign(
           :flash_message,
           if(approved, do: "已确认新 Spec Revision，将重建 Baseline。", else: "已拒绝保护输入变化；Best 保持不变。")
         )}

      {:error, reason} ->
        {:noreply, assign(socket, :flash_message, inspect(reason))}
    end
  end

  @impl true
  def render(assigns) do
    assigns =
      assigns
      |> assign(:selected_attempt, selected_attempt(assigns))
      |> assign(:filtered_metrics, filtered_metrics(assigns))
      |> then(&assign(&1, :metric_json, Jason.encode!(&1.filtered_metrics)))

    ~H"""
    <main class="control-shell">
      <header class="control-header">
        <a class="brand control-brand" href="/">
          <span class="brand-mark">P</span>
          <span><strong>Pika</strong><small>Kernel Optimization Agent</small></span>
        </a>
        <nav aria-label="主导航">
          <a href="/alignment">目标对齐</a>
          <button :for={tab <- @tabs} class={if @tab == tab, do: "active"} phx-click="select_tab" phx-value-tab={tab}>
            {tab_label(tab)}
          </button>
        </nav>
        <div class="campaign-controls">
          <span class={"state-pill state-#{@snapshot.control.campaign.status}"}>
            {@snapshot.control.campaign.status}
          </span>
          <button :if={@snapshot.control.campaign.status not in ~w(paused stopped blocked completed)} class="secondary compact" phx-click="pause">Pause</button>
          <button :if={@snapshot.control.campaign.status in ~w(paused stopped blocked)} class="primary compact" phx-click="resume">Resume</button>
          <button :if={!@stop_armed && @snapshot.control.campaign.status != "completed"} class="danger compact" phx-click="arm_stop">Stop Now</button>
          <span :if={@stop_armed} class="stop-confirm">
            确认终止所有活跃 Turn？
            <button class="danger compact" phx-click="stop_now">确认</button>
            <button class="secondary compact" phx-click="cancel_stop">取消</button>
          </span>
        </div>
      </header>

      <p :if={@flash_message} class="control-flash">{@flash_message}</p>

      <section :if={@tab == "attempts"} class="attempt-workspace">
        <aside class="attempt-sidebar panel">
          <div class="panel-heading">
            <div><p class="eyebrow">Attempts</p><h2>{@snapshot.control.campaign.attempts_created} 个尝试</h2></div>
            <span>{@snapshot.control.active_sessions} 活跃</span>
          </div>
          <div class="attempt-list">
            <button :for={attempt <- @snapshot.attempts} class={"attempt-list-item #{if @selected_attempt_id == attempt.id, do: "selected"}"} phx-click="select_attempt" phx-value-id={attempt.id}>
              <span class="attempt-ordinal">#{attempt.ordinal}</span>
              <span><strong>{attempt.summary || attempt.description || "等待摘要"}</strong><small>Slot {attempt.slot_index + 1} · {attempt.status}</small></span>
              <b>{best_delta(attempt.metrics)}</b>
            </button>
            <p :if={@snapshot.attempts == []} class="empty-copy">尚未创建 Attempt。</p>
          </div>
        </aside>

        <section class="attempt-main panel">
          <div :if={@selected_attempt} class="attempt-detail">
            <div class="attempt-title">
              <div><p class="eyebrow">Attempt #{@selected_attempt.ordinal}</p><h1>{@selected_attempt.summary || @selected_attempt.description || "运行中"}</h1></div>
              <button :if={btw_allowed?(@selected_attempt)} class="primary" phx-click="open_btw">By the way</button>
            </div>
            <div class="context-strip">
              <div><span>Status</span><strong>{@selected_attempt.status}</strong></div>
              <div><span>Base</span><code>{short_sha(@selected_attempt.base_sha)}</code></div>
              <div><span>Candidate</span><code>{short_sha(@selected_attempt.candidate_sha)}</code></div>
              <div><span>Worktree</span><strong>{@selected_attempt.worktree_relative_path}</strong></div>
            </div>
            <div class="attempt-stream">
              <article :for={event <- @selected_attempt.sessions} class="backend-card">
                <span>{event.role}</span><strong>{event.backend} · {event.model || "default"}</strong><small>{event.backend_protocol} · {event.reasoning_effort || "default"} · {event.status}</small>
              </article>
              <section class="agent-event-stream">
                <div class="stream-heading"><p class="eyebrow">Agent events</p><span>{length(@selected_attempt.agent_events)} events</span></div>
                <article :for={event <- @selected_attempt.agent_events} class={"agent-event event-#{event["type"] || "unknown"}"}>
                  <div><strong>{event_type_label(event["type"])}</strong><time>{event["at"] || "—"}</time></div>
                  <pre>{event_summary(event)}</pre>
                </article>
                <p :if={@selected_attempt.agent_events == []} class="empty-copy">Agent JSONL 到达后，文本、Plan、Tool、Diff 与 Terminal 事件会按顺序显示。</p>
              </section>
              <article class="summary-card">
                <p class="eyebrow">Summary</p>
                <p>{@selected_attempt.summary || "Agent 尚未提交结构化摘要。"}</p>
                <dl><div><dt>Profiler</dt><dd>{@selected_attempt.profiler_summary || "—"}</dd></div><div><dt>Outcome</dt><dd>{@selected_attempt.outcome_reason || @selected_attempt.recommended_outcome || "—"}</dd></div></dl>
              </article>
              <div class="metric-table-wrap">
                <table class="metric-table">
                  <thead><tr><th>Case</th><th>Metric</th><th>Value</th><th>改善</th><th>Source</th></tr></thead>
                  <tbody><tr :for={metric <- @selected_attempt.metrics}><td>{metric.case_id}</td><td>{metric.metric_id}</td><td>{format_value(metric.value, metric.unit)}</td><td>{format_ratio(metric.improvement_ratio)}</td><td>{metric.source}</td></tr></tbody>
                </table>
              </div>
              <section class="artifact-list">
                <div class="stream-heading"><p class="eyebrow">Artifacts</p><span>{length(@selected_attempt.artifacts)} items</span></div>
                <article :for={artifact <- @selected_attempt.artifacts} class="artifact-row">
                  <div><strong>{artifact.kind}</strong><code>{artifact.relative_path}</code></div>
                  <small>{format_bytes(artifact.byte_size)} · sha256 {short_sha(artifact.sha256)}</small>
                </article>
                <p :if={@selected_attempt.artifacts == []} class="empty-copy">尚无持久化 Artifact。</p>
              </section>
            </div>
          </div>
          <p :if={!@selected_attempt} class="empty-state">选择一个 Attempt 查看 Agent、Metrics、Artifacts 与 Outcome。</p>
        </section>

        <aside :if={@btw_open && @selected_attempt} class="btw-drawer panel" role="dialog" aria-modal="true">
          <div class="panel-heading"><div><p class="eyebrow">Forked context</p><h2>BTW · Attempt #{@selected_attempt.ordinal}</h2></div><button class="icon-button" phx-click="close_btw">×</button></div>
          <p>只允许从当前运行中的 Attempt 建立 BTW。选择作用域后才会注入。</p>
          <.form for={@btw_form} phx-submit="send_btw" class="btw-form">
            <.input field={@btw_form[:body]} type="textarea" placeholder="补充信息或询问当前 Agent…" />
            <select id={@btw_form[:mode].id} name={@btw_form[:mode].name}>
              <option value="chat" selected={@btw_form[:mode].value == "chat"}>仅对话</option>
              <option value="current" selected={@btw_form[:mode].value == "current"}>注入当前 Attempt</option>
              <option value="future" selected={@btw_form[:mode].value == "future"}>注入后续 Attempts</option>
            </select>
            <button class="primary">发送</button>
          </.form>
        </aside>
      </section>

      <section :if={@tab == "metrics"} class="metrics-workspace">
        <div class="metrics-heading"><div><p class="eyebrow">Metrics Timeline</p><h1>每次测量都保留真实时间与 Summary</h1></div><span>Spec v{@snapshot.spec && @snapshot.spec.revision}</span></div>
        <form id="metric-filters" class="metric-filters panel" phx-change="filter_metrics">
          <label>Metric
            <select name="metric_filter">
              <option value="all" selected={@metric_filter == "all"}>全部</option>
              <option :for={metric_id <- metric_ids(@snapshot.metrics)} value={metric_id} selected={@metric_filter == metric_id}>{metric_id}</option>
            </select>
          </label>
          <label>Case
            <select name="case_filter">
              <option value="all" selected={@case_filter == "all"}>全部</option>
              <option :for={case_id <- case_ids(@snapshot.metrics)} value={case_id} selected={@case_filter == case_id}>{case_id}</option>
            </select>
          </label>
          <label>Spec Revision
            <select name="spec_filter">
              <option value="all" selected={@spec_filter == "all"}>全部</option>
              <option :for={revision <- spec_revisions(@snapshot.metrics)} value={to_string(revision)} selected={@spec_filter == to_string(revision)}>v{revision}</option>
            </select>
          </label>
          <span>{length(@filtered_metrics)} points</span>
        </form>
        <div id="metrics-chart" class="metrics-chart panel" phx-hook="MetricsChart" phx-update="ignore" data-points={@metric_json}></div>
        <div class="metric-points panel">
          <button :for={point <- @filtered_metrics} class={"metric-point-card #{if @selected_attempt_id == point.attempt_id, do: "selected"}"} phx-click="select_attempt" phx-value-id={point.attempt_id}>
            <div><strong>Attempt #{point.ordinal} · {point.case_id}</strong><span>{point.source}</span></div>
            <p>{point.summary}</p>
            <small>{format_time(point.measured_at)} · {format_value(point.value, point.unit)} · {format_ratio(point.improvement_ratio)}</small>
          </button>
          <p :if={@filtered_metrics == []} class="empty-copy">当前筛选没有 Metrics。</p>
        </div>
        <section :if={@selected_attempt} class="metric-selection panel">
          <div class="panel-heading"><div><p class="eyebrow">Selected point</p><h2>Attempt #{@selected_attempt.ordinal}</h2></div><span>{@selected_attempt.status}</span></div>
          <dl>
            <div><dt>Patch</dt><dd>{artifact_path(@selected_attempt, ~w(patch diff))}</dd></div>
            <div><dt>Profiler</dt><dd>{@selected_attempt.profiler_summary || artifact_path(@selected_attempt, ~w(profiler profile))}</dd></div>
            <div><dt>Agent JSONL</dt><dd>{artifact_path(@selected_attempt, ["agent_jsonl"])}</dd></div>
            <div><dt>Outcome</dt><dd>{@selected_attempt.outcome_reason || @selected_attempt.recommended_outcome || "—"}</dd></div>
          </dl>
        </section>
      </section>

      <section :if={@tab == "sync"} class="sync-workspace">
        <div class="sync-grid">
          <section class="panel sync-panel">
            <div class="panel-heading"><div><p class="eyebrow">Manual Sync</p><h1>明确预览，明确确认</h1></div></div>
            <.form for={@sync_form} phx-submit="preview_sync" class="sync-form">
              <label>Remote<.input field={@sync_form[:remote]} placeholder="origin 或远端 URL" /></label>
              <label>Branch<.input field={@sync_form[:branch]} placeholder="main" /></label>
              <button class="secondary">读取远端状态</button>
            </.form>
            <div :if={@sync_preview} class="sync-preview">
              <dl>
                <div><dt>Remote SHA</dt><dd><code>{short_sha(@sync_preview.remote_sha)}</code></dd></div>
                <div><dt>Best SHA</dt><dd><code>{short_sha(@sync_preview.best_sha)}</code></dd></div>
                <div><dt>关系</dt><dd>{@sync_preview.relationship}</dd></div>
                <div><dt>待 Push commits</dt><dd>{@sync_preview.pending_commits}</dd></div>
              </dl>
              <p>确认后才会创建 Sync Run，并关闭新 Attempt dispatch；在途工作继续排空。</p>
              <button class="primary" phx-click="confirm_sync">确认并创建 Sync Run</button>
            </div>
          </section>

          <aside class="panel sync-status">
            <div class="panel-heading"><div><p class="eyebrow">Latest Run</p><h2>外部状态</h2></div></div>
            <%= if run = @snapshot.control.sync do %>
              <dl>
                <div><dt>Status</dt><dd>{run.status}</dd></div><div><dt>Remote</dt><dd>{run.remote}/{run.branch}</dd></div>
                <div><dt>Before</dt><dd><code>{short_sha(run.remote_before_sha)}</code></dd></div><div><dt>Candidate</dt><dd><code>{short_sha(run.candidate_sha)}</code></dd></div>
              </dl>
              <div :if={run.status == "awaiting_spec_confirmation"} class="spec-confirmation">
                <strong>保护输入发生变化</strong>
                <p>{Enum.join(run.protected_paths, "、")}</p>
                <button class="primary" phx-click="confirm_sync_spec" phx-value-decision="approve">确认新 Revision 并重建 Baseline</button>
                <button class="danger" phx-click="confirm_sync_spec" phx-value-decision="reject">拒绝 Sync</button>
              </div>
              <p :if={run.failure_reason} class="error-copy">{run.failure_reason}</p>
            <% else %>
              <p class="empty-copy">还没有 Sync Run。Pika 不会自动 Push 或自动 Sync。</p>
            <% end %>
          </aside>
        </div>
      </section>

      <section :if={@tab == "audit"} class="audit-workspace panel">
        <div class="panel-heading"><div><p class="eyebrow">Domain Events</p><h1>用户动作与状态变更审计</h1></div><span>{length(@snapshot.events)} events</span></div>
        <table class="audit-table"><thead><tr><th>Seq</th><th>Time</th><th>Aggregate</th><th>Event</th><th>Payload</th></tr></thead><tbody><tr :for={event <- @snapshot.events}><td>{event.sequence}</td><td>{format_time(event.created_at)}</td><td>{event.aggregate_type}</td><td>{event.event_type}</td><td><code>{compact_payload(event.payload)}</code></td></tr></tbody></table>
      </section>
    </main>
    """
  end

  defp refresh(socket, refresh_spec \\ false) do
    refresh_spec = refresh_spec or socket.assigns.refresh_spec
    if socket.assigns.refresh_timer, do: Process.cancel_timer(socket.assigns.refresh_timer)

    snapshot_opts = if refresh_spec, do: [], else: [spec: socket.assigns.snapshot.spec]
    snapshot = Pika.Dashboard.snapshot(socket.assigns.campaign_id, snapshot_opts)
    selected = selected_attempt_id(snapshot, socket.assigns.selected_attempt_id)

    assign(socket,
      snapshot: snapshot,
      selected_attempt_id: selected,
      refresh_timer: nil,
      refresh_spec: false
    )
  end

  defp schedule_refresh(socket, refresh_spec) do
    socket = assign(socket, :refresh_spec, socket.assigns.refresh_spec or refresh_spec)

    if socket.assigns.refresh_timer do
      socket
    else
      assign(
        socket,
        :refresh_timer,
        Process.send_after(self(), :refresh_snapshot, @refresh_debounce_ms)
      )
    end
  end

  defp spec_refresh_event?(event) when is_map(event) do
    (Map.get(event, :event_type) || Map.get(event, "event_type")) in @spec_refresh_events
  end

  defp spec_refresh_event?(_event), do: false

  defp control(socket, action, fun), do: control_result(socket, action, fun)

  defp control_result(socket, action, fun) do
    key = Map.fetch!(socket.assigns.action_keys, action)

    case fun.(key) do
      {:ok, _campaign} ->
        {:noreply,
         socket
         |> refresh()
         |> rotate_key(action)
         |> assign(:flash_message, "Campaign #{action} 已持久化。")}

      {:error, reason} ->
        {:noreply, assign(socket, :flash_message, inspect(reason))}
    end
  end

  defp selected_attempt(assigns),
    do: Enum.find(assigns.snapshot.attempts, &(&1.id == assigns.selected_attempt_id))

  defp selected_attempt_id(snapshot, current) do
    if Enum.any?(snapshot.attempts, &(&1.id == current)),
      do: current,
      else: snapshot.attempts |> List.last() |> then(&(&1 && &1.id))
  end

  defp filtered_metrics(assigns) do
    Enum.filter(assigns.snapshot.metrics, fn point ->
      (assigns.metric_filter == "all" or point.metric_id == assigns.metric_filter) and
        (assigns.case_filter == "all" or point.case_id == assigns.case_filter) and
        (assigns.spec_filter == "all" or to_string(point.spec_revision) == assigns.spec_filter)
    end)
  end

  defp metric_ids(points), do: points |> Enum.map(& &1.metric_id) |> Enum.uniq() |> Enum.sort()
  defp case_ids(points), do: points |> Enum.map(& &1.case_id) |> Enum.uniq() |> Enum.sort()

  defp spec_revisions(points),
    do:
      points
      |> Enum.map(& &1.spec_revision)
      |> Enum.reject(&is_nil/1)
      |> Enum.uniq()
      |> Enum.sort()

  defp artifact_path(attempt, kinds) do
    attempt.artifacts
    |> Enum.find(fn artifact ->
      kind = String.downcase(artifact.kind)
      Enum.any?(kinds, &String.contains?(kind, &1))
    end)
    |> case do
      nil -> "—"
      artifact -> artifact.relative_path
    end
  end

  defp event_type_label(nil), do: "Event"
  defp event_type_label("message_delta"), do: "Text"
  defp event_type_label("plan_updated"), do: "Plan"

  defp event_type_label(type) when type in ~w(tool_started tool_updated tool_completed),
    do: "Tool Call"

  defp event_type_label("file_changed"), do: "Diff"
  defp event_type_label("command_output"), do: "Terminal"
  defp event_type_label("best_advanced"), do: "BestAdvanced"
  defp event_type_label("sampling_advanced"), do: "SamplingAdvanced"
  defp event_type_label(type), do: type |> to_string() |> String.replace("_", " ")

  defp event_summary(event) do
    data = event["data"] || %{}

    value =
      first_present(
        data,
        ~w(delta text content output title command path diff message error summary)
      ) ||
        if(data == %{}, do: Map.drop(event, ~w(data at)), else: data)

    value
    |> case do
      value when is_binary(value) -> value
      value -> value |> Pika.JSONSafe.json_safe() |> Jason.encode!(pretty: true)
    end
    |> String.slice(0, 4_000)
  end

  defp first_present(map, keys) when is_map(map) do
    Enum.find_value(keys, fn key ->
      value = map[key]
      if value in [nil, "", []], do: nil, else: value
    end)
  end

  defp format_bytes(nil), do: "—"
  defp format_bytes(bytes) when bytes < 1_024, do: "#{bytes} B"
  defp format_bytes(bytes) when bytes < 1_048_576, do: "#{Float.round(bytes / 1_024, 1)} KiB"
  defp format_bytes(bytes), do: "#{Float.round(bytes / 1_048_576, 1)} MiB"

  defp sync_defaults do
    case Process.whereis(Pika.WorkspaceLock) do
      nil ->
        {"", ""}

      _ ->
        sync = get_in(Pika.WorkspaceLock.workspace().snapshot, ["mutable", "sync"]) || %{}
        {sync["remote"] || "", sync["branch"] || ""}
    end
  end

  defp action_keys do
    Map.new([:pause, :stop, :resume, :sync, :sync_spec, :btw], &{&1, Ecto.UUID.generate()})
  end

  defp rotate_key(socket, key),
    do: update(socket, :action_keys, &Map.put(&1, key, Ecto.UUID.generate()))

  defp authenticated?(session) do
    if Application.get_env(:pika, :runtime_mode, :preview) == :serve,
      do: Pika.Auth.authenticated_marker?(session["pika_auth"]),
      else: Pika.PreviewAuth.authenticated_marker?(session["preview_auth"])
  end

  defp btw_allowed?(attempt),
    do: attempt.status in ~w(running awaiting_report refreshing integrating interrupted)

  defp tab_label("attempts"), do: "Attempts"
  defp tab_label("metrics"), do: "Metrics"
  defp tab_label("sync"), do: "Sync"
  defp tab_label("audit"), do: "Audit"
  defp short_sha(nil), do: "—"
  defp short_sha(value), do: String.slice(value, 0, 10)
  defp format_ratio(nil), do: "—"
  defp format_ratio(value), do: :erlang.float_to_binary(value * 100, decimals: 2) <> "%"
  defp format_value(nil, _unit), do: "—"

  defp format_value(value, unit),
    do: :erlang.float_to_binary(value * 1.0, decimals: 3) <> " " <> unit

  defp best_delta([]), do: "—"

  defp best_delta(metrics),
    do:
      metrics
      |> Enum.max_by(&(&1.improvement_ratio || -1.0))
      |> Map.get(:improvement_ratio)
      |> format_ratio()

  defp format_time(value) when is_integer(value) do
    value |> DateTime.from_unix!(:microsecond) |> Calendar.strftime("%m-%d %H:%M:%S")
  end

  defp compact_payload(payload), do: payload |> Jason.encode!() |> String.slice(0, 180)
end
