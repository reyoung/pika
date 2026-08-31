import { expect, test } from '@playwright/test'

const snapshot = {
  snapshot_version: { api_version: 1, domain_revision: 19, event_sequence: 42, measurement_sequence: 18, runtime_generation: 3 },
  optimization: { id: 'optimization-fixture', status: 'optimizing', revision: 19 },
  scheduler: { status: 'running', epoch: 2 },
  terminal_attempt_limit: 25,
  legacy_measurement: false,
  nodes: [
    { kind: 'baseline', id: 'baseline-1', label: 'Baseline r1', domain_status: 'accepted', subtitle: 'abc123' },
    { kind: 'best', id: 'best-0', label: 'Best 0', domain_status: 'accepted', subtitle: 'abc123' },
    { kind: 'attempt', id: 'attempt-a', label: 'Attempt attempt-a', domain_status: 'accepted', subtitle: 'def456' },
    { kind: 'round', id: 'attempt-a:round:1', label: 'Round 1', domain_status: 'candidate', subtitle: 'explore' },
    { kind: 'integration', id: 'integration-a', label: 'Integration integra', domain_status: 'accepted', subtitle: 'def456', aggregates: [
      { metric_id: 'latency', label: 'Latency', role: 'primary', direction: 'lower_is_better', aggregate_speedup: 1.25, max_case_speedup: 1.4, unit: 'us', aggregation: 'weighted_geomean_of_ratios', source: 'structured' },
      { metric_id: 'accuracy', label: 'Accuracy', role: 'guard', direction: 'higher_is_better', aggregate_speedup: 1.02, max_case_speedup: 1.03, unit: 'score', aggregation: 'weighted_geomean_of_ratios', source: 'structured' },
    ] },
    { kind: 'best', id: 'best-1', label: 'Best 1', domain_status: 'accepted', subtitle: 'def456' },
    { kind: 'attempt', id: 'attempt-b', label: 'Attempt attempt-b', domain_status: 'rejected', subtitle: 'bad999' },
    { kind: 'round', id: 'attempt-b:round:1', label: 'Round 1', domain_status: 'rejected', subtitle: 'explore' },
    { kind: 'attempt', id: 'attempt-c', label: 'Attempt attempt-c', domain_status: 'iterating', runtime: { agent_name: 'pika-c', status: 'working', pane_id: 'pane-c' } },
    { kind: 'round', id: 'attempt-c:round:1', label: 'Round 1', domain_status: 'running', runtime: { agent_name: 'pika-c', status: 'working' } },
  ],
  edges: [
    { id: 'seed', source: 'baseline-1', target: 'best-0', kind: 'seed' },
    { id: 'a', source: 'best-0', target: 'attempt-a', kind: 'branch' },
    { id: 'ar', source: 'attempt-a', target: 'attempt-a:round:1', kind: 'round' },
    { id: 'ai', source: 'attempt-a:round:1', target: 'integration-a', kind: 'integration' },
    { id: 'ib', source: 'integration-a', target: 'best-1', kind: 'accepted' },
    { id: 'b', source: 'best-0', target: 'attempt-b', kind: 'branch' },
    { id: 'br', source: 'attempt-b', target: 'attempt-b:round:1', kind: 'round' },
    { id: 'c', source: 'best-1', target: 'attempt-c', kind: 'branch' },
    { id: 'cr', source: 'attempt-c', target: 'attempt-c:round:1', kind: 'round' },
  ],
  metrics: {
    measurement_sequence: 18,
    metrics: [{ baseline_revision_id: 'baseline-1', metric_id: 'latency', label: 'Latency', unit: 'us', role: 'primary', direction: 'lower_is_better', sample_statistic: 'median', aggregation: 'weighted_geomean_of_ratios' }],
    case_weights: [{ baseline_revision_id: 'baseline-1', case_id: 'case-a', weight: 1, ordinal: 0 }, { baseline_revision_id: 'baseline-1', case_id: 'case-b', weight: 2, ordinal: 1 }],
    measurement_sets: [], case_values: [], artifacts: [],
    comparisons: [
      { integration_id: 'integration-a', metric_id: 'latency', speedup: 1.25, regression: false, regression_fraction: 0, aggregate_speedup: 1.25, max_case_speedup: 1.4, max_case_id: 'case-a' },
      { integration_id: 'integration-a', case_id: 'case-a', metric_id: 'latency', reference_value: 100, candidate_value: 80, speedup: 1.25, regression: false, regression_fraction: 0 },
      { integration_id: 'integration-a', case_id: 'case-b', metric_id: 'latency', reference_value: 90, candidate_value: 95, speedup: .947, regression: true, regression_fraction: .053 },
    ],
  },
}

for (const viewport of [{ width: 1440, height: 900 }, { width: 1024, height: 768 }]) {
  test(`renders deterministic lineage at ${viewport.width}x${viewport.height}`, async ({ page }) => {
    await page.setViewportSize(viewport)
    let requests = 0
    await page.route('**/api/v1/workbench/snapshot**', async (route) => {
      requests += 1
      expect(route.request().headers().authorization).toBe('Bearer fixture-token')
      await route.fulfill({ json: snapshot })
    })
    await page.route('**/api/v1/workbench/nodes/**', async (route) => route.fulfill({ json: { node: snapshot.nodes[2], domain: { id: 'attempt-a', status: 'accepted' }, metrics: snapshot.metrics, artifacts: [] } }))
    await page.goto('/#token=fixture-token')
    await expect(page).toHaveURL('http://127.0.0.1:4173/')
    await expect(page.locator('.workbench-node')).toHaveCount(snapshot.nodes.length)
    await expect(page.locator('.node-state').filter({ hasText: 'rejected' }).first()).toBeVisible()
    await expect(page.getByText('runtime · working', { exact: true }).first()).toBeVisible()
    await expect(page.getByText('GM +25.0%', { exact: true })).toBeVisible()
    await expect(page.getByText('GM +2.0%', { exact: true })).toBeVisible()
    const flowBox = await page.locator('.react-flow').boundingBox()
    expect(flowBox, 'lineage canvas must have a usable viewport').not.toBeNull()
    expect(flowBox!.height, 'lineage canvas must not collapse into a strip').toBeGreaterThan(viewport.height * 0.4)
    const boxes = await page.locator('.workbench-node').evaluateAll((elements) => elements.map((element) => { const rect = element.getBoundingClientRect(); return { x: rect.x, y: rect.y, width: rect.width, height: rect.height } }))
    for (let left = 0; left < boxes.length; left += 1) for (let right = left + 1; right < boxes.length; right += 1) {
      const a = boxes[left], b = boxes[right]
      expect(a.x < b.x + b.width && a.x + a.width > b.x && a.y < b.y + b.height && a.y + a.height > b.y).toBe(false)
    }
    await page.getByText('Attempt attempt-a').click()
    await expect(page.locator('.detail-drawer')).toHaveClass(/is-open/)
    await expect(page.getByText('Domain record')).toBeVisible()
    await expect(page.locator('label').filter({ hasText: 'Refresh' }).locator('select')).toHaveValue('60')
    expect(requests).toBeGreaterThan(0)
  })
}

test('keeps the last snapshot and shows a stale banner after disconnect', async ({ page }) => {
  let available = true
  await page.route('**/api/v1/workbench/snapshot**', async (route) => available ? route.fulfill({ json: snapshot }) : route.fulfill({ status: 502, json: { error: { code: 'daemon_unavailable' } } }))
  await page.goto('/#token=fixture-token')
  await expect(page.locator('.workbench-node')).toHaveCount(snapshot.nodes.length)
  available = false
  await page.getByRole('button', { name: /Refresh/ }).click()
  await expect(page.getByText('STALE SNAPSHOT')).toBeVisible()
  await expect(page.locator('.workbench-node')).toHaveCount(snapshot.nodes.length)
})
