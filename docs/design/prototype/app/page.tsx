"use client";

import { useCallback, useEffect, useRef, useState } from "react";
import { LineChart } from "echarts/charts";
import { GridComponent, TooltipComponent } from "echarts/components";
import * as echarts from "echarts/core";
import type { EChartsType } from "echarts/core";
import { CanvasRenderer } from "echarts/renderers";

echarts.use([LineChart, GridComponent, TooltipComponent, CanvasRenderer]);

type View = "alignment" | "attempts" | "metrics";
type BtwMode = "chat" | "current" | "future";
type MetricKey = "latency" | "tflops" | "memory";

const attempts = [
  { id: 12, title: "TMA 双缓冲 + warp specialization", agent: "Codex · gpt-5.6-sol", status: "running", time: "11:46", delta: "测量中" },
  { id: 11, title: "缩短 epilogue 写回路径", agent: "Cursor · sonnet", status: "accepted", time: "11:18", delta: "+1.6%" },
  { id: 10, title: "增大 BLOCK_M 到 128", agent: "Codex · gpt-5.6-sol", status: "rejected", time: "10:57", delta: "−0.4%" },
  { id: 9, title: "合并 mask 与 softmax scale", agent: "Cursor · sonnet", status: "accepted", time: "10:31", delta: "+4.4%" },
  { id: 8, title: "减少 KV 重复加载", agent: "Codex · gpt-5.6-sol", status: "accepted", time: "10:06", delta: "+3.8%" },
];

const metricMeta: Record<MetricKey, { label: string; color: string; current: string; change: string }> = {
  latency: { label: "Latency", color: "#b8f35a", current: "37.9 μs", change: "↓ 11.4%" },
  tflops: { label: "TFLOPS", color: "#65d9ff", current: "139.2", change: "↑ 14.8%" },
  memory: { label: "Peak memory", color: "#ffbd66", current: "766 MB", change: "↓ 5.7%" },
};

function StatusPill({ status }: { status: string }) {
  const labels: Record<string, string> = { running: "运行中", accepted: "已接受", rejected: "已拒绝" };
  return <span className={`status-pill ${status}`}><span className="status-dot" />{labels[status] ?? status}</span>;
}

function AppIcon() {
  return <div className="app-icon" aria-hidden="true"><span>P</span><i /></div>;
}

function AlignmentView() {
  const [artifactOpen, setArtifactOpen] = useState(true);
  return (
    <div className="screen-grid alignment-grid">
      <section className="conversation-panel panel">
        <div className="panel-heading">
          <div><p className="eyebrow">目标对齐</p><h1>定义 Flash Attention Decode 边界</h1></div>
          <span className="phase-badge">Drafting Spec · v3</span>
        </div>
        <div className="conversation-scroll">
          <div className="message user-message">
            <div className="message-label">你 <span>09:21</span></div>
            <p>我希望融合 QK、softmax、PV 和 mask。重点优化线上 Decode 的小 batch shape，目标机器是 H20。</p>
          </div>
          <div className="message agent-message">
            <div className="message-label"><span className="agent-glyph">P</span> Boundary Agent <span>09:22</span></div>
            <p>计算边界清楚了。现在最容易让结果失真的，是只测一个“好看”的 shape。我建议从线上请求采样中提取频率，而不是手填权重。</p>
            <p>我可以先生成一个只记录 shape、dtype、layout 与出现次数的脚本，不保存 Tensor 数据。你希望直接运行脚本，还是已经有 pickle / JSONL dump？</p>
            <div className="inline-plan">
              <span>接下来</span>
              <ol><li className="done">冻结计算语义与 Fusion 边界</li><li className="active">收集线上 Benchmark Cases</li><li>确认 Metrics 与停止条件</li></ol>
            </div>
          </div>
          <div className="message user-message compact">
            <div className="message-label">你 <span>09:24</span></div><p>先生成脚本。我可以在线上容器里跑，然后把 jsonl 传回来。</p>
          </div>
          <div className="message agent-message">
            <div className="message-label"><span className="agent-glyph">P</span> Boundary Agent <span>09:25</span></div>
            <p>脚本已生成。它会对相同签名聚合计数，并对动态维度保留原值；不会序列化 Tensor。</p>
            <button className="artifact-card" onClick={() => setArtifactOpen(!artifactOpen)} aria-expanded={artifactOpen}>
              <span className="file-mark">PY</span><span><strong>dump_decode_shapes.py</strong><small>3.8 KB · 可在生产容器独立运行</small></span><b>{artifactOpen ? "收起" : "查看"}</b>
            </button>
            {artifactOpen && <pre className="code-preview"><code>{`with ShapeCollector("decode_shapes.jsonl") as c:\n    c.observe(q=q, k=k, v=v, kv_len=kv_len)\n# 仅写入 shape / dtype / layout / count`}</code></pre>}
          </div>
        </div>
        <div className="composer">
          <textarea aria-label="发送给 Boundary Agent" placeholder="继续描述边界，或拖入 pickle / JSONL 文件…" />
          <div className="composer-row"><span>⌘ ↵ 发送</span><button className="primary-button">发送</button></div>
        </div>
      </section>

      <aside className="spec-panel panel">
        <div className="spec-title"><div><p className="eyebrow">Campaign Spec</p><h2>待你确认的边界</h2></div><span>v3</span></div>
        <div className="spec-section">
          <h3>计算语义</h3>
          <div className="spec-row"><span>Reference</span><strong>reference.py</strong></div><div className="spec-row"><span>Fusion</span><strong>QK → mask → softmax → PV</strong></div><div className="spec-row"><span>DType</span><strong>BF16 input · FP32 accumulator</strong></div><div className="spec-row"><span>Tolerance</span><strong>atol 2e−2 · rtol 2e−2</strong></div>
        </div>
        <div className="spec-section">
          <div className="section-line"><h3>Benchmark Cases</h3><button>查看全部 7 个</button></div>
          <div className="case-card"><span className="case-type target">目标</span><div><strong>B=1 · Hq=24 · S=2,048</strong><small>线上频率 41.2% · contiguous</small></div></div>
          <div className="case-card"><span className="case-type guard">保护</span><div><strong>B=4 · Hq=24 · S=8,192</strong><small>线上频率 8.7% · paged KV</small></div></div>
          <div className="case-card muted"><span className="case-type info">观察</span><div><strong>B=16 · Hq=24 · S=512</strong><small>离线回归 · 不参与门禁</small></div></div>
        </div>
        <div className="spec-section">
          <h3>优化门禁</h3>
          <div className="metric-rule"><i className="lime" /><span>Latency</span><strong>至少改善 1%</strong></div><div className="metric-rule"><i className="cyan" /><span>TFLOPS</span><strong>不低于噪声带</strong></div><div className="metric-rule"><i className="amber" /><span>Peak memory</span><strong>不高于噪声带</strong></div>
        </div>
        <div className="spec-actions"><button className="ghost-button">查看 Spec diff</button><button className="confirm-button">确认并建立 Baseline</button></div>
      </aside>
    </div>
  );
}

function AttemptsView() {
  const [selected, setSelected] = useState(12);
  const [btwOpen, setBtwOpen] = useState(false);
  const [btwMode, setBtwMode] = useState<BtwMode>("chat");
  const current = attempts.find((item) => item.id === selected) ?? attempts[0];
  return (
    <div className="attempt-layout">
      <aside className="attempt-list panel">
        <div className="list-heading"><div><p className="eyebrow">Iterations</p><h2>12 / 30 Attempts</h2></div><span>3 并行</span></div>
        <div className="attempt-items">
          {attempts.map((attempt) => (
            <button key={attempt.id} className={`attempt-item ${selected === attempt.id ? "selected" : ""}`} onClick={() => { setSelected(attempt.id); setBtwOpen(false); }}>
              <div className="attempt-number">#{attempt.id}</div><div className="attempt-copy"><strong>{attempt.title}</strong><small>{attempt.agent}</small><div><StatusPill status={attempt.status} /><span>{attempt.time}</span></div></div><b>{attempt.delta}</b>
            </button>
          ))}
        </div>
        <div className="slot-summary"><span>Agent slots</span><div><i className="slot active" /><i className="slot active" /><i className="slot active" /></div><strong>3 / 3</strong></div>
      </aside>

      <section className="attempt-conversation panel">
        <div className="attempt-topbar"><div><div className="attempt-kicker">Attempt #{current.id} <StatusPill status={current.status} /></div><h1>{current.title}</h1></div><button className="btw-button" onClick={() => setBtwOpen(true)}>↗ By the way</button></div>
        <div className="context-strip"><div><span>Base</span><strong>9f31c2a</strong></div><div><span>Worktree</span><strong>attempts/0012</strong></div><div><span>Best latency</span><strong>38.1 μs</strong></div><div><span>Plan</span><strong>关闭</strong></div></div>
        <div className="conversation-scroll attempt-chat">
          <div className="message system-message"><span>11:46</span> 已从 Best <code>9f31c2a</code> 创建独立 worktree，并注入最近 10 次 Attempt 历史。</div>
          <div className="message agent-message">
            <div className="message-label"><span className="agent-glyph codex">C</span> Codex · high <span>11:47</span></div>
            <p>我会先确认当前瓶颈，再只尝试一个方向：把 K/V load 改为 TMA 双缓冲，同时保持现有 epilogue 不动。先读取 profiler 和最近失败记录。</p>
            <div className="tool-call"><div><span className="tool-icon">›_</span><strong>读取 Profiler 与历史</strong><StatusPill status="accepted" /></div><code>query_attempt_history(limit=10, tags=["tma", "pipeline"])</code><small>返回 4 条相关尝试 · 其中 2 条因寄存器压力被拒绝</small></div>
          </div>
          <div className="message agent-message">
            <div className="message-label"><span className="agent-glyph codex">C</span> Codex · high <span>11:53</span></div>
            <p>第一次实现通过正确性，但 BLOCK_N=128 时 occupancy 从 50% 降到 37.5%。我保留 TMA，缩小 stage buffer 后重新测量。</p>
            <div className="terminal-block"><div><span>benchmark_pair.py</span><b>运行中</b></div><pre>{`case decode_b1_s2048   baseline 38.12 μs\n                          candidate 37.64 μs\npair 18 / 30             median +1.23%`}</pre><div className="progress"><i style={{ width: "60%" }} /></div></div>
          </div>
          <div className="message event-message"><span className="pulse" />BestAdvanced 收到：Attempt #11 已合入 <code>2ae80df</code>。正式提交前必须刷新基础版本。</div>
          <div className="message agent-message"><div className="message-label"><span className="agent-glyph codex">C</span> Codex · high <span>11:58</span></div><p>收到。我会先完成当前测量，然后 rebase 到 <code>2ae80df</code>，重跑正确性和正式配对测量。</p></div>
        </div>
        <div className="read-only-composer"><span>Attempt 由 Agent 自主运行。需要纠偏时，从这里开启 BTW fork。</span><button onClick={() => setBtwOpen(true)}>开启 BTW</button></div>

        {btwOpen && <div className="btw-overlay" role="dialog" aria-modal="true" aria-label={`Attempt ${selected} By the way`}>
          <div className="btw-drawer">
            <div className="btw-heading"><div><p className="eyebrow">Forked context</p><h2>By the way · Attempt #{selected}</h2><small>已携带系统状态和该 Attempt 当前摘要</small></div><button onClick={() => setBtwOpen(false)} aria-label="关闭">×</button></div>
            <div className="btw-summary"><span>当前状态</span><p>Agent 正在完成配对测量，随后将 rebase 到 Best <code>2ae80df</code>。当前候选相对旧 Base 改善约 1.23%。</p></div>
            <div className="message user-message compact"><div className="message-label">你 <span>刚刚</span></div><p>我记得这个 shape 在线上经常是非 contiguous 的。现在覆盖了吗？</p></div>
            <div className="message agent-message compact"><div className="message-label"><span className="agent-glyph">P</span> BTW Agent</div><p>当前 Campaign Spec 的目标 Case 是 contiguous，另有一个 paged KV Guard Case，但没有非 contiguous Q。这个信息会改变测试边界。</p></div>
            <div className="mode-label">这条消息要去哪里？</div>
            <div className="mode-chips">{(["chat", "current", "future"] as BtwMode[]).map((mode) => { const label = { chat: "仅在 BTW 对话", current: `注入 Attempt #${selected}`, future: "注入后续 Attempts" }[mode]; return <button key={mode} className={btwMode === mode ? "active" : ""} onClick={() => setBtwMode(mode)}>{label}</button>; })}</div>
            <div className="btw-composer"><textarea aria-label="By the way 消息" placeholder="继续追问，或明确注入一条指导…" /><button className="primary-button">发送</button></div>
            {btwMode !== "chat" && <p className="injection-warning">发送前会再次确认；Pika 不会根据自然语言静默注入。</p>}
          </div>
        </div>}
      </section>

      <aside className="attempt-inspector panel">
        <p className="eyebrow">Attempt state</p><h2>实时摘要</h2>
        <div className="inspector-metric"><span>Latency</span><strong>37.64 μs</strong><b>+1.23%</b></div><div className="inspector-metric"><span>Correctness</span><strong>通过</strong><b className="neutral">7 / 7</b></div><div className="inspector-metric"><span>Pair samples</span><strong>18 / 30</strong><b className="neutral">MAD 0.31%</b></div>
        <div className="divider" />
        <div className="inspector-section"><span>当前假设</span><p>TMA 双缓冲能隐藏 KV load latency；缩小 stage buffer 避免 occupancy 退化。</p></div>
        <div className="inspector-section"><span>未读事件</span><div className="event-card"><i />BestAdvanced<small>新 Best 2ae80df · 待刷新</small></div></div>
        <div className="inspector-section"><span>Artifacts</span><button className="artifact-link"><b>NCU</b><span>profiles/0012/tma-stage2</span></button><button className="artifact-link"><b>LOG</b><span>agents/session-a91.jsonl</span></button></div>
      </aside>
    </div>
  );
}

function MetricChart({ enabled, onSelect }: { enabled: MetricKey[]; onSelect: (index: number) => void }) {
  const chartRef = useRef<HTMLDivElement>(null);
  const enabledKey = enabled.join(",");
  useEffect(() => {
    if (!chartRef.current) return;
    const chart: EChartsType = echarts.init(chartRef.current, undefined, { renderer: "canvas" });
      const labels = ["09:41", "10:06", "10:31", "10:57", "11:18", "11:46", "12:07"];
      const values: Record<MetricKey, number[]> = { latency: [0, 3.8, 8.2, 7.7, 11.0, 12.1, 11.4], tflops: [0, 3.1, 8.9, 8.2, 13.2, 15.4, 14.8], memory: [0, 1.5, 2.8, 2.8, 4.9, 4.9, 5.7] };
      const rawValues: Record<MetricKey, string[]> = { latency: ["42.8 μs", "41.2 μs", "39.3 μs", "39.5 μs", "38.1 μs", "37.6 μs", "37.9 μs"], tflops: ["121.3", "125.1", "132.1", "131.2", "137.3", "140.0", "139.2"], memory: ["812 MB", "800 MB", "789 MB", "789 MB", "772 MB", "772 MB", "766 MB"] };
      const points = [
        { title: "Baseline", status: "基线", summary: "Campaign Spec v3 的首次正式测量，建立各 Benchmark Case 的基准值与噪声带。" },
        { title: "Attempt #8 · 已接受", status: "+3.8%", summary: "减少 KV 重复加载，并复用相邻 tile 的地址计算；所有 Guard Cases 均位于噪声带内。" },
        { title: "Attempt #9 · 已接受", status: "+4.4%", summary: "将 mask 与 softmax scale 合并进同一 consumer 阶段，减少一次中间结果往返。" },
        { title: "Attempt #10 · 已拒绝", status: "−0.4%", summary: "BLOCK_M 增大到 128 后 occupancy 下降；正确性通过，但 Latency 未达到接受门槛。" },
        { title: "Attempt #11 · 已接受", status: "+1.6%", summary: "将向量化写回合并到 consumer warp，缩短 epilogue 路径且未增加 Peak memory。" },
        { title: "Attempt #12 · 测量中", status: "+1.2%", summary: "尝试 TMA 双缓冲与 warp specialization；当前结果仍基于旧 Best，刷新后需要重新测量。" },
        { title: "主线复验 · 已校正", status: "−0.7%", summary: "主线复验发现 Iteration 报告存在超过 1% 的偏差，已用最新测量覆盖该时间点。" },
      ];
      const names: Record<MetricKey, string> = { latency: "Latency 改善", tflops: "TFLOPS 改善", memory: "显存改善" };
      const colors: Record<MetricKey, string> = { latency: "#b8f35a", tflops: "#65d9ff", memory: "#ffbd66" };
      chart.setOption({
        animationDuration: 500,
        grid: { top: 28, right: 22, bottom: 44, left: 52 },
        tooltip: {
          trigger: "axis",
          axisPointer: { type: "line", lineStyle: { color: "rgba(184,243,90,.32)", width: 1 } },
          backgroundColor: "#111a24",
          borderColor: "#2a3846",
          padding: 0,
          textStyle: { color: "#eef4f7" },
          extraCssText: "border-radius:12px;box-shadow:0 18px 52px rgba(0,0,0,.45);overflow:hidden",
          formatter: (params: unknown) => {
            const items = (Array.isArray(params) ? params : [params]) as Array<{ dataIndex?: number; seriesIndex?: number; seriesName?: string; value?: number }>;
            const pointIndex = items[0]?.dataIndex ?? 0;
            const point = points[pointIndex];
            const rows = items.map((item) => {
              const metric = enabled[item.seriesIndex ?? 0];
              return `<div style="display:grid;grid-template-columns:8px 1fr auto auto;gap:8px;align-items:center;margin-top:7px"><i style="width:7px;height:7px;border-radius:50%;background:${colors[metric]}"></i><span style="color:#9aabb6">${metricMeta[metric].label}</span><strong style="font-family:monospace;color:#eef4f7">${rawValues[metric][pointIndex]}</strong><span style="font-family:monospace;color:${colors[metric]}">${Number(item.value ?? 0).toFixed(1)}%</span></div>`;
            }).join("");
            return `<div style="width:310px;padding:14px 15px"><div style="display:flex;align-items:center;justify-content:space-between;gap:12px"><strong style="font-size:12px;color:#f1f6f8">${point.title}</strong><b style="font:10px monospace;color:#b8f35a">${point.status}</b></div><div style="margin-top:5px;color:#627481;font:9px monospace">${labels[pointIndex]}</div><div style="margin-top:10px;padding-top:9px;border-top:1px solid rgba(119,143,160,.18)">${rows}</div><p style="margin:11px 0 0;padding-top:10px;border-top:1px solid rgba(119,143,160,.18);color:#b9c5cb;font-size:10px;line-height:1.55;white-space:normal"><span style="display:block;margin-bottom:4px;color:#627481;font:8px monospace;text-transform:uppercase;letter-spacing:.08em">Summary</span>${point.summary}</p></div>`;
          },
        },
        xAxis: { type: "category", data: labels, boundaryGap: false, axisLine: { lineStyle: { color: "#314151" } }, axisLabel: { color: "#738493", fontSize: 11 }, axisTick: { show: false } },
        yAxis: { type: "value", name: "相对 Baseline 改善 (%)", nameTextStyle: { color: "#738493", padding: [0, 0, 8, 20] }, splitLine: { lineStyle: { color: "rgba(107,129,146,.13)" } }, axisLabel: { color: "#738493", formatter: "{value}%" } },
        series: enabled.map((key) => ({ name: names[key], type: "line", smooth: 0.22, data: values[key], symbol: "circle", symbolSize: 8, lineStyle: { width: 2.5, color: colors[key] }, itemStyle: { color: colors[key], borderColor: "#0b1118", borderWidth: 2 }, areaStyle: key === "latency" ? { color: "rgba(184,243,90,.06)" } : undefined, emphasis: { focus: "series" } })),
      });
      chart.on("click", (params) => { if (typeof params.dataIndex === "number") onSelect(params.dataIndex); });
    const resize = () => chart.resize();
    window.addEventListener("resize", resize);
    return () => { window.removeEventListener("resize", resize); chart.dispose(); };
  }, [enabledKey, onSelect]);
  return <div ref={chartRef} className="metric-chart" role="img" aria-label="各次 Attempt 的 Metrics 时间折线图" />;
}

function MetricsView() {
  const [enabled, setEnabled] = useState<MetricKey[]>(["latency", "tflops", "memory"]);
  const [selectedPoint, setSelectedPoint] = useState(4);
  const selectPoint = useCallback((index: number) => setSelectedPoint(index), []);
  const pointData = [["Baseline", "42.8 μs", "121.3", "812 MB", "基线"], ["Attempt #8", "41.2 μs", "125.1", "800 MB", "已接受"], ["Attempt #9", "39.3 μs", "132.1", "789 MB", "已接受"], ["Attempt #10", "39.5 μs", "131.2", "789 MB", "已拒绝"], ["Attempt #11", "38.1 μs", "137.3", "772 MB", "已接受"], ["Attempt #12", "37.6 μs", "140.0", "772 MB", "测量中"], ["主线复验", "37.9 μs", "139.2", "766 MB", "已校正"]][selectedPoint];
  const toggle = (key: MetricKey) => setEnabled((current) => current.includes(key) ? (current.length === 1 ? current : current.filter((item) => item !== key)) : [...current, key]);
  return (
    <div className="metrics-page">
      <div className="metrics-header"><div><p className="eyebrow">Metrics timeline</p><h1>每次尝试，都留在曲线上</h1><p>时间轴展示全部 Attempt 的最新测量；不同单位统一为相对 Baseline 的改善百分比。</p></div><div className="revision-select"><span>Spec Revision</span><button>v3 · Decode production cases⌄</button></div></div>
      <div className="metric-cards">{(Object.keys(metricMeta) as MetricKey[]).map((key) => <button key={key} className={`metric-card ${enabled.includes(key) ? "active" : ""}`} onClick={() => toggle(key)} style={{ "--metric-color": metricMeta[key].color } as React.CSSProperties}><span><i />{metricMeta[key].label}</span><strong>{metricMeta[key].current}</strong><b>{metricMeta[key].change}</b><small>相对 Baseline</small></button>)}</div>
      <div className="chart-panel panel">
        <div className="chart-heading"><div><h2>优化时间线</h2><span>09:41 — 12:07 · 7 个测量节点</span></div><div className="chart-legend">{enabled.map((key) => <span key={key}><i style={{ background: metricMeta[key].color }} />{metricMeta[key].label}</span>)}</div></div>
        <MetricChart enabled={enabled} onSelect={selectPoint} />
        <div className="timeline-statuses"><span className="accepted">● #8 接受</span><span className="accepted">● #9 接受</span><span className="rejected">× #10 拒绝</span><span className="accepted">● #11 接受</span><span className="running">◌ #12 运行中</span><span className="corrected">◆ 主线校正</span></div>
      </div>
      <div className="metric-detail-grid">
        <section className="point-detail panel"><div><p className="eyebrow">Selected point</p><h2>{pointData[0]}</h2></div><StatusPill status={pointData[4] === "已拒绝" ? "rejected" : pointData[4] === "测量中" ? "running" : "accepted"} /><div className="point-values"><div><span>Latency</span><strong>{pointData[1]}</strong></div><div><span>TFLOPS</span><strong>{pointData[2]}</strong></div><div><span>Peak memory</span><strong>{pointData[3]}</strong></div></div><div className="noise-band"><span>Noise band</span><strong>±0.43%</strong><div><i style={{ width: "43%" }} /></div></div></section>
        <section className="attempt-note panel"><div><p className="eyebrow">Why it moved</p><h2>缩短 epilogue 写回路径</h2></div><p>将 partial output 的向量化写回从独立 epilogue 合并到 consumer warp，Latency 中位数改善 1.6%，所有 Guard Cases 位于噪声带内。</p><div className="detail-links"><button>查看 Summary ↗</button><button>Patch · 4.2 KB ↗</button><button>Profiler ↗</button></div></section>
      </div>
    </div>
  );
}

export default function Home() {
  const [view, setView] = useState<View>("alignment");
  const labels: Record<View, string> = { alignment: "目标对齐", attempts: "Attempts", metrics: "Metrics" };
  return (
    <main className="app-shell">
      <header className="app-header">
        <div className="brand"><AppIcon /><div><strong>PIKA</strong><span>Kernel Optimization Agent</span></div></div>
        <nav aria-label="主导航">{(Object.keys(labels) as View[]).map((item) => <button key={item} className={view === item ? "active" : ""} onClick={() => setView(item)}>{labels[item]}{item === "attempts" && <i>3</i>}</button>)}</nav>
        <div className="header-status"><div><span>flash_attn_decode</span><strong>pika/best · 2ae80df</strong></div><span className="live-pill"><i />OPTIMIZING</span><button className="more-button" aria-label="更多操作">•••</button></div>
      </header>
      <section className="workspace">{view === "alignment" && <AlignmentView />}{view === "attempts" && <AttemptsView />}{view === "metrics" && <MetricsView />}</section>
      <footer className="prototype-footer"><span>交互原型 · 数据仅用于设计评审</span><span>Spec v3 · H20 · BF16</span></footer>
    </main>
  );
}
