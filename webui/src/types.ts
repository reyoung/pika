export type SnapshotVersion = {
  api_version: number
  domain_revision: number
  event_sequence: number
  measurement_sequence: number
  runtime_generation: number
}

export type RuntimeObservation = {
  agent_name: string
  agent_kind?: string
  status: string
  workspace_id?: string
  tab_id?: string
  pane_id?: string
  terminal_id?: string
}

export type PrimarySummary = {
  metric_id: string
  label?: string
  role?: 'primary' | 'guard' | 'informational'
  direction?: 'lower_is_better' | 'higher_is_better'
  aggregate_speedup: number
  max_case_speedup: number
  unit?: string
  aggregation?: 'weighted_geomean_of_ratios' | 'ratio_of_weighted_arithmetic_means'
  source?: 'structured' | 'legacy_evidence'
}

export type WorkbenchNode = {
  kind: 'baseline' | 'best' | 'attempt' | 'round' | 'integration'
  id: string
  label: string
  domain_status: string
  subtitle?: string
  runtime?: RuntimeObservation
  primary?: PrimarySummary
  aggregates?: PrimarySummary[]
}

export type WorkbenchEdge = {
  id: string
  source: string
  target: string
  kind: string
  dashed?: boolean
}

export type MetricDefinition = {
  baseline_revision_id: string
  metric_id: string
  label: string
  unit: string
  role: 'primary' | 'guard' | 'informational'
  direction: 'lower_is_better' | 'higher_is_better'
  sample_statistic: string
  aggregation: 'weighted_geomean_of_ratios' | 'ratio_of_weighted_arithmetic_means'
}

export type CaseWeight = { baseline_revision_id: string; case_id: string; weight: number; ordinal: number }
export type MeasurementSet = {
  id: string
  baseline_revision_id: string
  work_id?: string
  integration_id?: string
  best_sequence?: number
  kind: 'development_baseline' | 'reference' | 'candidate'
  created_at: string
}
export type CaseValue = { measurement_set_id: string; case_id: string; metric_id: string; value: number }
export type Comparison = {
  integration_id: string
  case_id?: string
  metric_id: string
  reference_value?: number
  candidate_value?: number
  speedup: number
  regression: boolean
  regression_fraction: number
  aggregate_speedup?: number
  max_case_speedup?: number
  max_case_id?: string
}
export type ArtifactMetadata = {
  id: string
  work_id: string
  receipt_id?: string
  relative_path: string
  byte_size: number
  content_sha256: string
  contract_version: number
}
export type Metrics = {
  metrics?: MetricDefinition[]
  case_weights?: CaseWeight[]
  measurement_sets?: MeasurementSet[]
  case_values?: CaseValue[]
  comparisons?: Comparison[]
  artifacts?: ArtifactMetadata[]
  measurement_sequence: number
}

export type Snapshot = {
  snapshot_version: SnapshotVersion
  optimization: { id: string; status: string; revision: number }
  scheduler: { status: string; epoch: number }
  nodes: WorkbenchNode[]
  edges: WorkbenchEdge[]
  metrics: Metrics
  legacy_measurement: boolean
  terminal_attempt_limit: number
}

export type NodeDetail = {
  node: WorkbenchNode
  domain: unknown
  metrics: Metrics
  artifacts?: ArtifactMetadata[]
}

export type ArtifactPreview = {
  metadata: ArtifactMetadata
  content_type: string
  previewable: true
  content: string
}
