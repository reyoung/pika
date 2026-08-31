import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { Background, Controls, MiniMap, ReactFlow, type NodeMouseHandler } from '@xyflow/react'
import '@xyflow/react/dist/style.css'
import { downloadArtifact, importFragmentToken, loadArtifactPreview, loadNode, loadSnapshot, tokenStorageKey } from './api'
import { layoutGraph } from './graph'
import { MetricPanel } from './MetricPanel'
import type { ArtifactMetadata, ArtifactPreview, NodeDetail, Snapshot } from './types'
import { WorkbenchNode } from './WorkbenchNode'
import './styles.css'

const nodeTypes = { workbench: WorkbenchNode }

function Login({ onLogin }: { onLogin: (token: string) => void }) {
  const [value, setValue] = useState('')
  return <main className="login-shell"><div className="login-card"><div className="brand-mark">P</div><p className="eyebrow">PIKA-GO / READ ONLY</p><h1>Lineage Workbench</h1><p>输入 Workspace 的 <span className="mono">runtime/webui/token</span>。Token 只保存在本标签页的 sessionStorage。</p><form onSubmit={(event) => { event.preventDefault(); if (value.trim()) onLogin(value.trim()) }}><input autoFocus type="password" value={value} onChange={(event) => setValue(event.target.value)} placeholder="Bearer token" /><button>Open workbench</button></form></div></main>
}

function DetailDrawer({ token, detail, loading, onClose }: { token: string; detail: NodeDetail | null; loading: boolean; onClose: () => void }) {
  const [preview, setPreview] = useState<ArtifactPreview | null>(null)
  const openArtifact = async (artifact: ArtifactMetadata) => {
    const result = await loadArtifactPreview(token, artifact.id)
    if (result instanceof Blob) await downloadArtifact(token, artifact.id, artifact.relative_path.split('/').at(-1) || 'artifact')
    else setPreview(result)
  }
  return <aside className={`detail-drawer ${detail || loading ? 'is-open' : ''}`} aria-hidden={!detail && !loading}>
    <div className="drawer-header"><div><p className="eyebrow">DEEP LINK</p><h2>{detail?.node.label || 'Loading…'}</h2></div><button className="icon-button" onClick={onClose} aria-label="Close details">×</button></div>
    {detail && <div className="drawer-scroll">
      <div className="detail-status"><span className={`status-dot status-${detail.node.domain_status}`} /> <b>domain</b> {detail.node.domain_status}{detail.node.runtime && <><i /> <b>runtime</b> {detail.node.runtime.status}</>}</div>
      {detail.node.primary && <section className="detail-card metric-summary"><span>primary aggregate</span><strong>{detail.node.primary.aggregate_speedup.toFixed(3)}×</strong><small>max Case {detail.node.primary.max_case_speedup.toFixed(3)}×</small></section>}
      <section className="detail-card"><h3>Domain record</h3><pre>{JSON.stringify(detail.domain, null, 2)}</pre></section>
      <section className="detail-card"><h3>Runtime observation</h3><pre>{JSON.stringify(detail.node.runtime || { status: 'not observed' }, null, 2)}</pre></section>
      <section className="detail-card"><h3>Registered artifacts</h3>{detail.artifacts?.length ? detail.artifacts.map((artifact) => <div className="artifact-row" key={artifact.id}><button onClick={() => void openArtifact(artifact)}>{artifact.relative_path}</button><span>{formatBytes(artifact.byte_size)}</span><button className="download" onClick={() => void downloadArtifact(token, artifact.id, artifact.relative_path.split('/').at(-1) || 'artifact')}>↓</button></div>) : <p className="muted">No registered artifacts.</p>}</section>
      {preview && <section className="detail-card preview"><div className="preview-head"><h3>{preview.metadata.relative_path}</h3><button onClick={() => setPreview(null)}>close</button></div><pre>{preview.content}</pre></section>}
    </div>}
  </aside>
}

function formatBytes(bytes: number) { return bytes < 1024 ? `${bytes} B` : bytes < 1048576 ? `${(bytes / 1024).toFixed(1)} KiB` : `${(bytes / 1048576).toFixed(1)} MiB` }

export default function App() {
  const [token, setToken] = useState(() => importFragmentToken())
  const [snapshot, setSnapshot] = useState<Snapshot | null>(null)
  const [stale, setStale] = useState<string>('')
  const [loading, setLoading] = useState(false)
  const [terminalLimit, setTerminalLimit] = useState(() => Number(localStorage.getItem('pika-terminal-limit')) || 25)
  const [refreshSeconds, setRefreshSeconds] = useState(() => Math.max(5, Number(localStorage.getItem('pika-refresh-seconds')) || 60))
  const [selected, setSelected] = useState<{ kind: string; id: string } | null>(null)
  const [detail, setDetail] = useState<NodeDetail | null>(null)
  const refreshing = useRef(false)
  const login = (next: string) => { sessionStorage.setItem(tokenStorageKey, next); setToken(next) }
  const refresh = useCallback(async () => {
    if (!token || refreshing.current) return
    refreshing.current = true
    setLoading(true)
    try { setSnapshot(await loadSnapshot(token, terminalLimit)); setStale('') }
    catch (error) { setStale(error instanceof Error ? error.message : 'Snapshot unavailable') }
    finally { refreshing.current = false; setLoading(false) }
  }, [token, terminalLimit])
  useEffect(() => { void refresh(); const timer = window.setInterval(() => void refresh(), refreshSeconds * 1000); return () => clearInterval(timer) }, [refresh, refreshSeconds])
  useEffect(() => { localStorage.setItem('pika-terminal-limit', String(terminalLimit)); localStorage.setItem('pika-refresh-seconds', String(refreshSeconds)) }, [terminalLimit, refreshSeconds])
  useEffect(() => {
    if (!selected || !token) { setDetail(null); return }
    let active = true
    void loadNode(token, selected.kind, selected.id).then((value) => { if (active) setDetail(value) }).catch((error) => { if (active) setStale(error instanceof Error ? error.message : 'Node unavailable') })
    return () => { active = false }
  }, [selected, token, snapshot?.snapshot_version.domain_revision])
  const flow = useMemo(() => snapshot ? layoutGraph(snapshot.nodes, snapshot.edges) : { nodes: [], edges: [] }, [snapshot])
  const onNodeClick: NodeMouseHandler = (_, node) => setSelected({ kind: String(node.data.kind), id: node.id })
  if (!token) return <Login onLogin={login} />
  return <div className="app-shell">
    <header className="topbar">
      <div className="brand"><div className="brand-mark">P</div><div><strong>PIKA-GO</strong><span>Lineage Workbench · read only</span></div></div>
      <div className="health-line"><span className={`health-beacon ${stale ? 'is-stale' : ''}`} /><div><b>{stale ? 'STALE SNAPSHOT' : 'DAEMON ATTACHED'}</b><small>{snapshot ? `rev ${snapshot.snapshot_version.domain_revision} · runtime g${snapshot.snapshot_version.runtime_generation}` : 'connecting…'}</small></div></div>
      <div className="toolbar"><label>History<select value={terminalLimit} onChange={(event) => setTerminalLimit(Number(event.target.value))}>{[5, 10, 25, 50, 100].map((value) => <option key={value}>{value}</option>)}</select></label><label>Refresh<select value={refreshSeconds} onChange={(event) => setRefreshSeconds(Math.max(5, Number(event.target.value)))}>{[5, 15, 30, 60, 120].map((value) => <option key={value} value={value}>{value}s</option>)}</select></label><button className="refresh-button" onClick={() => void refresh()} disabled={loading}>{loading ? '···' : '↻'} Refresh</button></div>
    </header>
    {stale && <div className="stale-banner"><b>Connection degraded.</b> Showing the last coherent snapshot. {stale}</div>}
    <div className="context-strip"><div><span>OPTIMIZATION</span><b>{snapshot?.optimization.id || '—'}</b></div><div><span>DOMAIN</span><b>{snapshot?.optimization.status || '—'}</b></div><div><span>SCHEDULER</span><b>{snapshot?.scheduler.status || '—'}</b></div><div><span>LINEAGE</span><b>{snapshot ? `${snapshot.nodes.filter((node) => node.kind === 'attempt').length} attempts · ${snapshot.nodes.filter((node) => node.kind === 'best').length} bests` : '—'}</b></div></div>
    <main className="workspace-grid">
      <section className="lineage-panel"><div className="panel-label"><span>BASELINE → ATTEMPT / ROUND → INTEGRATION → BEST</span><span>domain solid · runtime striped</span></div>{snapshot ? <ReactFlow nodes={flow.nodes} edges={flow.edges} nodeTypes={nodeTypes} onNodeClick={onNodeClick} fitView fitViewOptions={{ padding: 0.16 }} minZoom={0.18} maxZoom={1.8} colorMode="dark"><Background gap={22} size={1} color="#1c2735" /><MiniMap pannable zoomable nodeStrokeWidth={3} /><Controls showInteractive={false} /></ReactFlow> : <div className="loading-grid">Loading lineage…</div>}</section>
      <MetricPanel metrics={snapshot?.metrics || { measurement_sequence: 0 }} legacy={snapshot?.legacy_measurement || false} />
    </main>
    <DetailDrawer token={token} detail={detail} loading={Boolean(selected) && !detail} onClose={() => { setSelected(null); setDetail(null) }} />
  </div>
}
