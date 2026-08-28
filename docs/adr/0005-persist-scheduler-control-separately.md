# Persist Scheduler control separately from Optimization lifecycle

Scheduler Pause is an orthogonal, durable control state whose transition and per-Session deliveries are serialized with runtime dispatch and audited independently from Optimization Pause, because reusing the workflow lifecycle would conflate intervention, cancellation, and process control while a best-effort interrupt alone could race with newly scheduled Sessions or be duplicated after a crash.
