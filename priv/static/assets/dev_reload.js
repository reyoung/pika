(() => {
  let fingerprint = null
  let reloading = false

  const poll = async () => {
    try {
      const response = await fetch("/__pika_reload", {
        cache: "no-store",
        headers: {accept: "application/json"}
      })

      if (response.ok) {
        const next = (await response.json()).fingerprint

        if (fingerprint && next && next !== fingerprint) {
          reloading = true
          window.setTimeout(() => window.location.reload(), 250)
          return
        }

        fingerprint = next
      }
    } catch (_error) {
      // The server may be compiling or briefly unavailable; the next poll retries.
    }

    if (!reloading) window.setTimeout(poll, 750)
  }

  poll()
})()
