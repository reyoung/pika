import { useEffect, useMemo, useRef, useState } from 'react'
import * as echarts from 'echarts/core'
import { LineChart, ScatterChart } from 'echarts/charts'
import { GridComponent, LegendComponent, TooltipComponent } from 'echarts/components'
import { CanvasRenderer } from 'echarts/renderers'
import type { EChartsCoreOption } from 'echarts/core'
import type { Metrics } from './types'

echarts.use([LineChart, ScatterChart, GridComponent, LegendComponent, TooltipComponent, CanvasRenderer])

function Chart({ option, label }: { option: EChartsCoreOption; label: string }) {
  const ref = useRef<HTMLDivElement>(null)
  useEffect(() => {
    if (!ref.current) return
    const chart = echarts.init(ref.current, undefined, { renderer: 'canvas' })
    chart.setOption(option)
    const resize = () => chart.resize()
    window.addEventListener('resize', resize)
    return () => { window.removeEventListener('resize', resize); chart.dispose() }
  }, [option])
  return <div ref={ref} className="metric-chart" role="img" aria-label={label} />
}

export function MetricPanel({ metrics, legacy }: { metrics: Metrics; legacy: boolean }) {
  const [tab, setTab] = useState<'trend' | 'cases' | 'regressions'>('trend')
  const definitions = metrics.metrics || []
  const primary = definitions.find((metric) => metric.role === 'primary')
  const comparisons = useMemo(() => metrics.comparisons || [], [metrics.comparisons])
  const aggregate = comparisons.filter((item) => !item.case_id && item.metric_id === primary?.metric_id && item.aggregate_speedup != null)
  const latestIntegration = aggregate.at(-1)?.integration_id
  const cases = comparisons.filter((item) => item.integration_id === latestIntegration && item.case_id && item.metric_id === primary?.metric_id)
  const regressions = useMemo(() => [...comparisons].filter((item) => item.case_id && item.regression).sort((a, b) => b.regression_fraction - a.regression_fraction), [comparisons])
  const trendOption = useMemo<EChartsCoreOption>(() => ({
    backgroundColor: 'transparent',
    tooltip: { trigger: 'axis' },
    grid: { left: 42, top: 22, right: 18, bottom: 28 },
    xAxis: { type: 'category', data: aggregate.map((_, index) => `Best ${index + 1}`), axisLabel: { color: '#79879b' } },
    yAxis: { type: 'value', name: 'speedup ×', min: 0.8, nameTextStyle: { color: '#79879b' }, axisLabel: { color: '#79879b' }, splitLine: { lineStyle: { color: '#202b3b' } } },
    series: [{ type: 'line', smooth: 0.24, data: aggregate.map((item) => item.aggregate_speedup), symbolSize: 7, lineStyle: { color: '#5dd6c0', width: 2 }, itemStyle: { color: '#5dd6c0' }, areaStyle: { color: 'rgba(93,214,192,.08)' } }],
  }), [aggregate])
  const caseOption = useMemo<EChartsCoreOption>(() => ({
    backgroundColor: 'transparent',
    tooltip: { trigger: 'item' },
    grid: { left: 90, top: 18, right: 24, bottom: 30 },
    xAxis: { type: 'value', name: primary?.unit || '', axisLabel: { color: '#79879b' }, splitLine: { lineStyle: { color: '#202b3b' } } },
    yAxis: { type: 'category', data: cases.map((item) => item.case_id), axisLabel: { color: '#a8b3c3', width: 76, overflow: 'truncate' } },
    series: cases.flatMap((item, index) => [
      { name: item.case_id, type: 'line' as const, data: [[item.reference_value, index], [item.candidate_value, index]], symbolSize: 8, lineStyle: { color: item.regression ? '#ff7b72' : '#5dd6c0', width: 3 }, itemStyle: { color: item.regression ? '#ff7b72' : '#5dd6c0' } },
      { name: `reference-${item.case_id}`, type: 'scatter' as const, data: [[item.reference_value, index]], symbolSize: 8, itemStyle: { color: '#77849a' } },
    ]),
  }), [cases, primary])

  if (legacy) return <section className="metrics-panel legacy"><div className="panel-heading"><span>Measurements</span><span className="pill warning">legacy evidence</span></div><div className="empty-state">此 Baseline Revision 创建于 measurement contract 迁移前。原始 evidence 可查看，但不会被启发式解析成结构化指标。</div></section>
  if (!primary) return <section className="metrics-panel"><div className="panel-heading">Measurements</div><div className="empty-state">等待 Development Baseline 指标。</div></section>
  return (
    <section className="metrics-panel">
      <div className="panel-heading">
        <div><span>Measurements</span><small>{primary.label} · {primary.unit} · {primary.direction.replaceAll('_', ' ')}</small></div>
        <div className="metric-tabs" role="tablist">
          {(['trend', 'cases', 'regressions'] as const).map((name) => <button key={name} className={tab === name ? 'active' : ''} onClick={() => setTab(name)}>{name}</button>)}
        </div>
      </div>
      {tab === 'trend' && <Chart option={trendOption} label="Best primary metric trend" />}
      {tab === 'cases' && (cases.length ? <Chart option={caseOption} label="Integration per-case dumbbell comparison" /> : <div className="empty-state">尚无 Integration 配对测量。</div>)}
      {tab === 'regressions' && <div className="regression-table"><table><thead><tr><th>Case</th><th>Metric</th><th>Ref</th><th>Candidate</th><th>Δ</th></tr></thead><tbody>{regressions.map((item) => <tr key={`${item.integration_id}-${item.case_id}-${item.metric_id}`}><td>{item.case_id}</td><td>{item.metric_id}</td><td>{item.reference_value}</td><td>{item.candidate_value}</td><td className="negative">-{(item.regression_fraction * 100).toFixed(1)}%</td></tr>)}</tbody></table>{!regressions.length && <div className="empty-state">当前记录中没有 per-Case regression。</div>}</div>}
      <footer className="formula">weight: frozen per Case · statistic: {primary.sample_statistic} · aggregate: {primary.aggregation.replaceAll('_', ' ')}</footer>
    </section>
  )
}
