# Capture provider events but always recover with a fresh session

Pika installs provider-specific hooks to correlate sessions, turns, messages, and observable tool activity with Work, while retaining a provider-neutral Conversation Journal in SQLite. Recovery and Back-off always create a fresh Agent Session and rebuild context from Pika state; provider session IDs are used for correlation and audit, not resume. A static Role selects one provider, and recovery reuses that configuration without fallback. Back-off may cross providers only when it moves to a differently configured Role.

Codex and Cursor implement the same deep Provider Adapter contract: validate configuration, probe exact runtime capabilities, prepare Session-owned launch resources, and normalize raw Hook events. Each Agent Session records its provider kind, version, and capabilities. Native session, turn, and tool identities are scoped by provider and Pika Agent Session.

## Consequences

- Provider transcript files are debugging inputs, not stable protocols or domain authority.
- A Codex-owned profile layer injects Pika hooks without copying the user's complete `CODEX_HOME`.
- Cursor init additively installs one environment-parameterized global MCP entry and event-specific hooks while preserving existing configuration. Each Session keeps only its frozen prompt/argv privately, injects the prompt through `sessionStart.additional_context`, and loads the `pika_go` dynamic namespace before Role MCP calls.
- `pika-go kick-off` checks Herdr before creating a workspace and, with explicit user consent, atomically sets `session.resume_agents_on_restore = false` and requires a successful live config reload. A reload failure restores the original file. Direct `pika-go init` remains read-only and rejects invalid configuration. Both provider wrappers reject resume arguments as defense in depth.
- Cursor Shell and MCP supplements, including large output, are retained in SQLite alongside the normalized logical Tool event.
- Providers lacking reliable turn and conversation events may still run under Herdr, but automatic Follow-up and complete journaling are disabled.
