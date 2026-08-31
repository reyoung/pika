import { Handle, Position, type NodeProps } from '@xyflow/react'
import type { FlowNodeData } from './graph'

const statusGlyph = (status: string) => {
  if (status === 'accepted' || status === 'completed') return '✓'
  if (status === 'rejected' || status === 'cancelled') return '×'
  if (status === 'stale' || status === 'backed_off' || status === 'refresh_pending') return '↻'
  if (status === 'running' || status === 'iterating' || status === 'integrating') return '●'
  return '◇'
}

const aggregatePerformance = (speedup: number, aggregation?: string) => {
  const prefix = aggregation === 'weighted_geomean_of_ratios' ? 'GM' : aggregation === 'ratio_of_weighted_arithmetic_means' ? 'AM' : 'AVG'
  const improvement = (speedup - 1) * 100
  return `${prefix} ${improvement >= 0 ? '+' : ''}${improvement.toFixed(1)}%`
}

export function WorkbenchNode({ data, selected }: NodeProps) {
  const node = data as FlowNodeData
  const aggregates = node.aggregates?.length ? node.aggregates : node.primary ? [node.primary] : []
  return (
    <div className={`workbench-node kind-${node.kind} status-${node.domain_status} ${selected ? 'is-selected' : ''}`}>
      <Handle type="target" position={Position.Left} />
      <div className="node-eyebrow"><span>{node.kind}</span><span className="node-state"><b aria-hidden>{statusGlyph(node.domain_status)}</b>{node.domain_status}</span></div>
      <div className="node-title">{node.label}</div>
      <div className="node-footer">
        <span className="mono">{node.subtitle || '—'}</span>
      </div>
      {aggregates.length > 0 && <div className="node-aggregates">{aggregates.slice(0, 2).map((aggregate) => <div className={`aggregate-badge ${aggregate.source === 'legacy_evidence' ? 'is-legacy' : ''}`} key={aggregate.metric_id} title={`${aggregate.aggregate_speedup.toFixed(3)}× aggregate speedup · ${aggregate.source || 'structured'}`}><span>{aggregate.label || aggregate.metric_id}</span><b>{aggregatePerformance(aggregate.aggregate_speedup, aggregate.aggregation)}</b></div>)}</div>}
      {node.runtime && <div className="runtime-strip"><span className="pulse" /> runtime · {node.runtime.status}</div>}
      <Handle type="source" position={Position.Right} />
    </div>
  )
}
