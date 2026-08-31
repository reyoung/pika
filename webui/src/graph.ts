import dagre from 'dagre'
import type { Edge, Node } from '@xyflow/react'
import type { WorkbenchEdge, WorkbenchNode } from './types'

export const nodeWidth = 188
export const nodeHeight = 82

export type FlowNodeData = WorkbenchNode & Record<string, unknown>

export function layoutGraph(nodes: WorkbenchNode[], edges: WorkbenchEdge[]): { nodes: Node<FlowNodeData>[]; edges: Edge[] } {
  const graph = new dagre.graphlib.Graph()
  graph.setGraph({ rankdir: 'LR', ranksep: 92, nodesep: 34, marginx: 32, marginy: 32, acyclicer: 'greedy' })
  graph.setDefaultEdgeLabel(() => ({}))
  nodes.forEach((node) => graph.setNode(node.id, { width: nodeWidth, height: nodeHeight }))
  edges.forEach((edge) => graph.setEdge(edge.source, edge.target))
  dagre.layout(graph)
  return {
    nodes: nodes.map((node) => {
      const point = graph.node(node.id)
      return { id: node.id, type: 'workbench', data: node, position: { x: point.x - nodeWidth / 2, y: point.y - nodeHeight / 2 } }
    }),
    edges: edges.map((edge) => ({
      id: edge.id,
      source: edge.source,
      target: edge.target,
      type: edge.kind === 'best_spine' ? 'smoothstep' : 'bezier',
      animated: edge.kind === 'accepted',
      className: `lineage-edge lineage-edge--${edge.kind}${edge.dashed ? ' is-dashed' : ''}`,
    })),
  }
}
