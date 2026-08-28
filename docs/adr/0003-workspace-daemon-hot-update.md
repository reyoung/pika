# ADR 0003: Workspace-owned daemon generations and FD handoff

Status: accepted

## Decision

Pika-Go stores executable generations under each Optimization Workspace. A local candidate is content-addressed by SHA-256 and must report the same handoff protocol, control protocol, Workspace format, SQLite schema, OS, and architecture as the running daemon.

Compatible updates use process replacement rather than in-process code loading. The old process retains ownership while the candidate performs provider preflight, then passes duplicate descriptors for the public Unix listener and Workspace flock plus a private socketpair. The candidate opens SQLite only after `activate`, reports `ready` after its control plane is serving, and does not reconcile or dispatch runtime effects until `commit`.

The old process quiesces HTTP, cancels and joins runtime loops, closes SQLite, activates the candidate, atomically records `current.json`, and requires matching health within 15 seconds. If any activation or stabilization checkpoint fails, it restores `current.json` and starts the prior content-addressed generation with the same retained listener and lock descriptors.

After a successor is prepared, the old HTTP generation gets a two-second graceful connection-drain window. Requests that remain in flight are canceled by closing only the old generation's connections; the inherited listener stays available to the successor. This bound is a handoff ownership policy, not extra startup tolerance: the deterministic regression holds an MCP request open, verifies cancellation, and requires the old process to return the handoff outcome.

## Consequences

- The Unix socket path and inode, Herdr workspace/tab/panes, Agent Session IDs, terminal IDs, and grants survive a compatible update.
- No domain `draining` state is introduced; update progress is operational state in `runtime/daemon/update.json`.
- A binary that changes a compatibility dimension requires backup, shutdown, and cold resume.
- A Workspace must be started once by an update-capable build before it can accept hot updates.
- Complexity is contained behind the daemon-generation and handoff boundary; the CLI, HTTP layer, Herdr adapter, and Symphony domain do not implement descriptor or rollback mechanics independently.
