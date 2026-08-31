# Pika-Go Design

Status: accepted design baseline, 2026-08-27.

Pika-Go is a Herdr plugin for long-running automatic optimization. A visible `pika-go` daemon acts as Symphony: it projects durable Work from SQLite, starts fresh coding-agent sessions in Herdr panes, exposes Role-scoped MCP operations, and lets users intervene directly in the Agent pane.

## Goals

- Preserve the useful Optimization, Baseline, Attempt, Integration, Follow-up, prompt, and MCP semantics from the Elixir Pika v2 design.
- Make direct human interaction a terminal concern: users steer Codex, OpenCode, or Cursor in the Herdr pane itself.
- Keep workflow completion deterministic and transactional even when terminal detection or provider hooks are incomplete.
- Distribute one Go binary plus a small Herdr plugin manifest on macOS and Linux.
- Make daemon restart and provider switching safe by rebuilding context into a fresh Agent Session.

## Non-goals

- A Web application, Web authentication, or LiveView progress UI.
- Dynamic Role registration or arbitrary Work/DAG injection.
- A Pika API for steering or cancelling one provider turn.
- Provider session resume.
- Inferring completion from terminal text, `idle`, or `done`.
- Modifying Herdr for the first release.

## Documents

- [Architecture](architecture.md): ownership, process topology, interfaces, lifecycle, and recovery.
- [Incremental development plan](development-plan.md): executable vertical slices, module seams, and phase exit gates.
- [State machines](state-machine.md): Baseline, Attempt, Integration, Follow-up, cancellation, and shutdown transitions.
- [Protocols](protocols.md): CLI/daemon, MCP, Herdr socket, hooks, identity, and idempotency contracts.
- [Role contracts](role-contracts.md): static immutable System Prompts, dynamic activation context, user instruction overlays, MCP catalogs, and completion rules.
- [Configuration](configuration.md): initialization, Agent settings, instruction editing, provider overlays, and paths.
- [Storage](storage.md): SQLite ownership, conceptual schema, journaling, and retention.
- [Release operations](../release.md): archive verification, Herdr linking, online backup, and upgrade procedure.
- [Runtime capability research](research/runtime-capabilities.md): versioned primary-source evidence.
- [Domain language](../../CONTEXT.md): canonical project terminology.

## Decision summary

1. One Optimization has one visible daemon and one SQLite database.
2. The daemon listens only on a Unix socket and publishes a short instance identifier in Herdr Workspace metadata.
3. Herdr owns runtime observation; Pika and terminal MCP own domain state.
4. Every recovery starts a fresh Agent Session, even if Herdr or a provider exposes a native session ID.
5. Roles, System Prompts, and scheduling are static. Users append optional Markdown instructions with `$EDITOR` and use declared `back-off` transitions when the workflow must move earlier.
6. Baseline, Baseline Verification, and Integration are single-concurrency; Iteration has one concurrent slot per independently configured Agent entry; Integration is FIFO.
7. Follow-up is a dedicated Role with one Agent Configuration, multiple target-specific System Prompts, and matching user instruction overlays.
8. Codex hooks capture session/turn/message/tool data. All observable shell and tool output is kept in SQLite.
9. Follow-up inactivity defaults to five minutes and is reset from best-effort Herdr pane activity. `pane.updated` is deliberately accepted despite not being a strict human-input signal.
10. Graceful daemon shutdown drains existing Agents and never kills them merely to make shutdown finish.
