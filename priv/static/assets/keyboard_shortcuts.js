document.addEventListener("keydown", event => {
  if (
    event.defaultPrevented ||
    event.key !== "Enter" ||
    event.isComposing ||
    event.keyCode === 229 ||
    event.target.tagName !== "TEXTAREA"
  ) {
    return
  }

  const form = event.target.closest("form[data-submit-on-enter]")
  if (!form) return

  if (event.shiftKey) return

  if (event.metaKey || event.ctrlKey) {
    event.preventDefault()
    event.target.setRangeText(
      "\n",
      event.target.selectionStart,
      event.target.selectionEnd,
      "end"
    )
    event.target.dispatchEvent(new Event("input", {bubbles: true}))
    return
  }

  event.preventDefault()
  form.requestSubmit()
})

window.addEventListener("phx:clear-form", event => {
  const form = document.getElementById(event.detail.id)
  if (!form) return

  form.reset()
  form.querySelectorAll("input, textarea, select").forEach(field => {
    field.dispatchEvent(new Event("input", {bubbles: true}))
    field.dispatchEvent(new Event("change", {bubbles: true}))
  })
})
