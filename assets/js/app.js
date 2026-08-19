import "../css/app.css"
import {Socket} from "phoenix"
import {LiveSocket} from "phoenix_live_view"
import * as echarts from "echarts/core"
import {LineChart} from "echarts/charts"
import {GridComponent, LegendComponent, TooltipComponent} from "echarts/components"
import {CanvasRenderer} from "echarts/renderers"

echarts.use([LineChart, GridComponent, LegendComponent, TooltipComponent, CanvasRenderer])

const csrfToken = document.querySelector("meta[name='csrf-token']")?.getAttribute("content")

const Hooks = {}

Hooks.ConversationScroll = {
  mounted() {
    this.shouldStick = true
    this.scrollToLatest()
    this.el.addEventListener("scroll", () => {
      this.shouldStick = this.distanceFromBottom() < 80
    })
  },
  beforeUpdate() {
    this.shouldStick = this.distanceFromBottom() < 80
  },
  updated() {
    if (this.shouldStick) this.scrollToLatest()
  },
  distanceFromBottom() {
    return this.el.scrollHeight - this.el.scrollTop - this.el.clientHeight
  },
  scrollToLatest() {
    this.el.scrollTop = this.el.scrollHeight
    window.requestAnimationFrame(() => {
      this.el.scrollTop = this.el.scrollHeight
    })
  }
}

Hooks.Composer = {
  mounted() {
    this.textarea = this.el.querySelector("textarea")
    this.handleEvent("composer:clear", () => {
      this.el.reset()
      if (this.textarea) {
        this.textarea.value = ""
        this.textarea.dispatchEvent(new Event("input", {bubbles: true}))
        this.textarea.focus()
      }
    })
    this.handleEvent("composer:focus", () => {
      window.requestAnimationFrame(() => {
        this.textarea?.focus()
        const end = this.textarea?.value.length || 0
        this.textarea?.setSelectionRange(end, end)
      })
    })
    this.onKeydown = event => {
      const composing = event.isComposing || event.keyCode === 229
      if (event.key === "Enter" && !event.shiftKey && !composing) {
        event.preventDefault()
        const submit = this.el.querySelector("button[type='submit']")
        if (!submit?.disabled) this.el.requestSubmit(submit)
      }
    }
    this.textarea?.addEventListener("keydown", this.onKeydown)
  },
  destroyed() {
    this.textarea?.removeEventListener("keydown", this.onKeydown)
  }
}

Hooks.MetricsChart = {
  mounted() {
    this.chart = echarts.init(this.el, undefined, {renderer: "canvas"})
    this.renderChart()
    this.resize = () => this.chart?.resize()
    window.addEventListener("resize", this.resize)
    this.chart.on("click", params => {
      const attemptId = params?.data?.attemptId
      if (attemptId) this.pushEvent("select_attempt", {id: attemptId})
    })
  },
  updated() {
    this.renderChart()
  },
  destroyed() {
    window.removeEventListener("resize", this.resize)
    this.chart?.dispose()
  },
  renderChart() {
    const points = JSON.parse(this.el.dataset.points || "[]")
    const grouped = new Map()

    points.forEach(point => {
      const key = `v${point.spec_revision} · ${point.case_id} · ${point.metric_id} · ${point.source}`
      if (!grouped.has(key)) grouped.set(key, [])
      grouped.get(key).push({
        value: [Math.floor(point.measured_at / 1000), point.improvement_ratio * 100],
        attemptId: point.attempt_id,
        ordinal: point.ordinal,
        status: point.status,
        summary: point.summary,
        rawValue: point.value,
        unit: point.unit,
        source: point.source,
        specRevision: point.spec_revision,
        caseId: point.case_id,
        metricId: point.metric_id,
        noise: point.noise_tolerance * 100
      })
    })

    const series = Array.from(grouped, ([name, data]) => ({
      name,
      type: "line",
      showSymbol: true,
      symbolSize: 8,
      connectNulls: false,
      data: data.sort((left, right) => left.value[0] - right.value[0]),
      emphasis: {focus: "series"},
      lineStyle: {width: 2}
    }))

    this.chart.setOption({
      animationDuration: 350,
      backgroundColor: "transparent",
      color: ["#b8f35a", "#65d9ff", "#ffbd66", "#ce8cff", "#69d5bd", "#f18888"],
      grid: {top: 58, right: 28, bottom: 54, left: 66},
      legend: {
        type: "scroll",
        top: 16,
        textStyle: {color: "#82959d", fontSize: 10}
      },
      tooltip: {
        trigger: "item",
        backgroundColor: "#111a24",
        borderColor: "#30434d",
        textStyle: {color: "#dce7eb"},
        extraCssText: "max-width:360px;border-radius:10px;box-shadow:0 18px 48px rgba(0,0,0,.48)",
        formatter: params => {
          const point = params.data
          return `<div style="padding:5px 7px;white-space:normal">
            <strong>Attempt #${point.ordinal} · ${escapeHTML(point.status)}</strong>
            <div style="margin-top:7px;color:#8fa1a9">Spec v${escapeHTML(point.specRevision)} · ${escapeHTML(point.caseId)} / ${escapeHTML(point.metricId)}</div>
            <div style="margin-top:5px;font-family:monospace">${Number(point.rawValue).toFixed(3)} ${escapeHTML(point.unit)} · ${Number(point.value[1]).toFixed(2)}%</div>
            <div style="margin-top:8px;padding-top:8px;border-top:1px solid #2a3940;color:#aab8be;line-height:1.5"><span style="display:block;color:#647780;font-size:9px;text-transform:uppercase">Summary</span>${escapeHTML(point.summary)}</div>
            <div style="margin-top:6px;color:#647780;font-size:9px">${escapeHTML(point.source)} · noise ±${Number(point.noise).toFixed(2)}%</div>
          </div>`
        }
      },
      xAxis: {
        type: "time",
        axisLine: {lineStyle: {color: "#30414a"}},
        axisLabel: {color: "#70818a", fontSize: 10}
      },
      yAxis: {
        type: "value",
        name: "相对 Baseline 改善 (%)",
        nameTextStyle: {color: "#70818a"},
        axisLabel: {color: "#70818a", formatter: "{value}%"},
        splitLine: {lineStyle: {color: "rgba(111,133,147,.13)"}}
      },
      series
    }, {notMerge: true})
  }
}

function escapeHTML(value) {
  return String(value ?? "")
    .replaceAll("&", "&amp;")
    .replaceAll("<", "&lt;")
    .replaceAll(">", "&gt;")
    .replaceAll('"', "&quot;")
    .replaceAll("'", "&#039;")
}

const liveSocket = new LiveSocket("/live", Socket, {
  hooks: Hooks,
  params: {_csrf_token: csrfToken}
})

liveSocket.connect()
window.liveSocket = liveSocket
