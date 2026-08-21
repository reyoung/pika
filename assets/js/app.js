import "../css/app.css"
import {Socket} from "phoenix"
import {LiveSocket} from "phoenix_live_view"
import * as echarts from "echarts/core"
import {LineChart} from "echarts/charts"
import {GridComponent, LegendComponent, TooltipComponent} from "echarts/components"
import {CanvasRenderer} from "echarts/renderers"
import hljs from "highlight.js/lib/core"
import bash from "highlight.js/lib/languages/bash"
import c from "highlight.js/lib/languages/c"
import cmake from "highlight.js/lib/languages/cmake"
import cpp from "highlight.js/lib/languages/cpp"
import elixir from "highlight.js/lib/languages/elixir"
import go from "highlight.js/lib/languages/go"
import ini from "highlight.js/lib/languages/ini"
import java from "highlight.js/lib/languages/java"
import javascript from "highlight.js/lib/languages/javascript"
import json from "highlight.js/lib/languages/json"
import makefile from "highlight.js/lib/languages/makefile"
import plaintext from "highlight.js/lib/languages/plaintext"
import python from "highlight.js/lib/languages/python"
import rust from "highlight.js/lib/languages/rust"
import toml from "highlight.js/lib/languages/ini"
import typescript from "highlight.js/lib/languages/typescript"
import yaml from "highlight.js/lib/languages/yaml"

echarts.use([LineChart, GridComponent, LegendComponent, TooltipComponent, CanvasRenderer])

Object.entries({
  bash,
  c,
  cmake,
  cpp,
  elixir,
  go,
  ini,
  java,
  javascript,
  json,
  makefile,
  plaintext,
  python,
  rust,
  toml,
  typescript,
  yaml
}).forEach(([name, language]) => hljs.registerLanguage(name, language))

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

Hooks.CommandConsole = {
  mounted() {
    this.output = this.el.querySelector("[data-console-output]")
    this.resizeHandle = this.el.querySelector("[data-console-resize]")
    this.collapseButton = this.el.querySelector("[data-console-collapse]")
    this.latestButton = this.el.querySelector("[data-console-latest]")
    this.shouldStick = true
    this.scrollToLatest()

    this.onScroll = () => {
      this.shouldStick = this.distanceFromBottom() < 80
      this.el.classList.toggle("console-paused", !this.shouldStick)
    }
    this.onLatest = () => {
      this.shouldStick = true
      this.el.classList.remove("console-paused")
      this.scrollToLatest()
    }
    this.onCollapse = () => this.el.classList.toggle("collapsed")
    this.onKeydown = event => {
      if (event.key === "Escape") this.pushEvent("close_command_console", {})
      if (event.target === this.resizeHandle && ["ArrowUp", "ArrowDown"].includes(event.key)) {
        event.preventDefault()
        this.setHeight(this.el.getBoundingClientRect().height + (event.key === "ArrowUp" ? 24 : -24))
      }
    }
    this.onPointerDown = event => {
      event.preventDefault()
      const startY = event.clientY
      const startHeight = this.el.getBoundingClientRect().height
      const move = moveEvent => this.setHeight(startHeight + startY - moveEvent.clientY)
      const up = () => {
        window.removeEventListener("pointermove", move)
        window.removeEventListener("pointerup", up)
      }
      window.addEventListener("pointermove", move)
      window.addEventListener("pointerup", up)
    }

    this.output?.addEventListener("scroll", this.onScroll)
    this.latestButton?.addEventListener("click", this.onLatest)
    this.collapseButton?.addEventListener("click", this.onCollapse)
    this.resizeHandle?.addEventListener("pointerdown", this.onPointerDown)
    window.addEventListener("keydown", this.onKeydown)
  },
  beforeUpdate() {
    this.shouldStick = this.distanceFromBottom() < 80
    this.wasCollapsed = this.el.classList.contains("collapsed")
  },
  updated() {
    this.output = this.el.querySelector("[data-console-output]")
    this.el.classList.toggle("collapsed", this.wasCollapsed)
    if (this.shouldStick) this.scrollToLatest()
  },
  destroyed() {
    this.output?.removeEventListener("scroll", this.onScroll)
    this.latestButton?.removeEventListener("click", this.onLatest)
    this.collapseButton?.removeEventListener("click", this.onCollapse)
    this.resizeHandle?.removeEventListener("pointerdown", this.onPointerDown)
    window.removeEventListener("keydown", this.onKeydown)
  },
  distanceFromBottom() {
    if (!this.output) return 0
    return this.output.scrollHeight - this.output.scrollTop - this.output.clientHeight
  },
  scrollToLatest() {
    if (!this.output) return
    this.output.scrollTop = this.output.scrollHeight
    window.requestAnimationFrame(() => { this.output.scrollTop = this.output.scrollHeight })
  },
  setHeight(height) {
    const bounded = Math.max(160, Math.min(window.innerHeight * 0.85, height))
    this.el.style.height = `${bounded}px`
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

Hooks.CopyMarkdown = {
  mounted() {
    this.defaultLabel = this.el.textContent
    this.onClick = async () => {
      const markdown = this.el.dataset.markdown || ""

      try {
        await copyText(markdown)
        this.showResult("已复制", true)
      } catch (_error) {
        this.showResult("复制失败", false)
      }
    }
    this.el.addEventListener("click", this.onClick)
  },
  destroyed() {
    this.el.removeEventListener("click", this.onClick)
    window.clearTimeout(this.resetTimer)
  },
  showResult(label, copied) {
    this.el.textContent = label
    this.el.classList.toggle("copied", copied)
    window.clearTimeout(this.resetTimer)
    this.resetTimer = window.setTimeout(() => {
      this.el.textContent = this.defaultLabel
      this.el.classList.remove("copied")
    }, 1400)
  }
}

Hooks.ReferenceSyntaxHighlight = {
  mounted() {
    this.highlightSource()
  },
  updated() {
    this.highlightSource()
  },
  highlightSource() {
    const code = this.el.querySelector("code")
    if (!code) return

    const source = code.textContent || ""
    const language = this.el.dataset.language || "plaintext"
    code.textContent = source
    code.className = "hljs"

    if (!hljs.getLanguage(language)) return

    try {
      const result = hljs.highlight(source, {language, ignoreIllegals: true})
      code.innerHTML = result.value
      code.classList.add(`language-${language}`)
    } catch (_error) {
      code.textContent = source
    }
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
      const targetImprovement = point.target_relative_improvement ?? point.improvement_ratio
      const bestImprovement = point.best_relative_improvement ?? point.improvement_ratio
      grouped.get(key).push({
        value: [Math.floor(point.measured_at / 1000), targetImprovement == null ? null : targetImprovement * 100],
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
        targetValue: point.target_value,
        targetImprovement: targetImprovement == null ? null : targetImprovement * 100,
        bestImprovement: bestImprovement == null ? null : bestImprovement * 100,
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
            <div style="margin-top:5px;font-family:monospace">Development ${formatMetricNumber(point.rawValue)} ${escapeHTML(point.unit)} · Target ${formatMetricNumber(point.targetValue)} ${escapeHTML(point.unit)}</div>
            <div style="margin-top:4px;font-family:monospace">vs Target ${formatPercent(point.targetImprovement)} · vs Best ${formatPercent(point.bestImprovement)}</div>
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
        name: "相对固定 Target 改善 (%)",
        nameTextStyle: {color: "#70818a"},
        axisLabel: {color: "#70818a", formatter: "{value}%"},
        splitLine: {lineStyle: {color: "rgba(111,133,147,.13)"}}
      },
      series
    }, {notMerge: true})
  }
}

function formatMetricNumber(value) {
  return value == null ? "—" : Number(value).toFixed(3)
}

function formatPercent(value) {
  return value == null ? "—" : `${Number(value).toFixed(2)}%`
}

function escapeHTML(value) {
  return String(value ?? "")
    .replaceAll("&", "&amp;")
    .replaceAll("<", "&lt;")
    .replaceAll(">", "&gt;")
    .replaceAll('"', "&quot;")
    .replaceAll("'", "&#039;")
}

async function copyText(text) {
  if (navigator.clipboard?.writeText) {
    await navigator.clipboard.writeText(text)
    return
  }

  const textarea = document.createElement("textarea")
  textarea.value = text
  textarea.setAttribute("readonly", "")
  textarea.style.position = "fixed"
  textarea.style.opacity = "0"
  document.body.appendChild(textarea)
  textarea.select()

  const copied = document.execCommand("copy")
  textarea.remove()
  if (!copied) throw new Error("clipboard unavailable")
}

const liveSocket = new LiveSocket("/live", Socket, {
  hooks: Hooks,
  params: {_csrf_token: csrfToken}
})

liveSocket.connect()
window.liveSocket = liveSocket
