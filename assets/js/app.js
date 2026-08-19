import "../css/app.css"
import {Socket} from "phoenix"
import {LiveSocket} from "phoenix_live_view"

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

const liveSocket = new LiveSocket("/live", Socket, {
  hooks: Hooks,
  params: {_csrf_token: csrfToken}
})

liveSocket.connect()
window.liveSocket = liveSocket
