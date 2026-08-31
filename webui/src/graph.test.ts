import { describe, expect, it } from 'vitest'
import { layoutGraph, nodeHeight, nodeWidth } from './graph'
import type { WorkbenchEdge, WorkbenchNode } from './types'

describe('layoutGraph', () => {
  it('lays out the deterministic lineage fixture without node overlap', () => {
    const nodes: WorkbenchNode[] = [
      { kind: 'baseline', id: 'baseline', label: 'Baseline r1', domain_status: 'accepted' },
      { kind: 'best', id: 'best-0', label: 'Best 0', domain_status: 'accepted' },
      { kind: 'attempt', id: 'attempt-a', label: 'Attempt a', domain_status: 'accepted' },
      { kind: 'attempt', id: 'attempt-b', label: 'Attempt b', domain_status: 'rejected' },
      { kind: 'round', id: 'round-a', label: 'Round 1', domain_status: 'candidate' },
      { kind: 'round', id: 'round-b', label: 'Round 1', domain_status: 'rejected' },
      { kind: 'integration', id: 'integration-a', label: 'Integration a', domain_status: 'accepted' },
      { kind: 'best', id: 'best-1', label: 'Best 1', domain_status: 'accepted' },
    ]
    const edges: WorkbenchEdge[] = [
      { id: 'seed', source: 'baseline', target: 'best-0', kind: 'seed' },
      { id: 'a', source: 'best-0', target: 'attempt-a', kind: 'branch' },
      { id: 'b', source: 'best-0', target: 'attempt-b', kind: 'branch' },
      { id: 'ar', source: 'attempt-a', target: 'round-a', kind: 'round' },
      { id: 'br', source: 'attempt-b', target: 'round-b', kind: 'round' },
      { id: 'ai', source: 'round-a', target: 'integration-a', kind: 'integration' },
      { id: 'ib', source: 'integration-a', target: 'best-1', kind: 'accepted' },
    ]
    const result = layoutGraph(nodes, edges)
    for (let left = 0; left < result.nodes.length; left += 1) {
      for (let right = left + 1; right < result.nodes.length; right += 1) {
        const a = result.nodes[left].position
        const b = result.nodes[right].position
        const overlaps = a.x < b.x + nodeWidth && a.x + nodeWidth > b.x && a.y < b.y + nodeHeight && a.y + nodeHeight > b.y
        expect(overlaps, `${result.nodes[left].id} overlaps ${result.nodes[right].id}`).toBe(false)
      }
    }
  })
})
