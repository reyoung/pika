# Capture provider events but always recover with a fresh session

Pika installs provider-specific hooks to correlate sessions, turns, messages, and observable tool activity with Work, while retaining a provider-neutral Conversation Journal in SQLite. Recovery, Back-off, and provider changes always create a fresh Agent Session and rebuild context from Pika state; provider session IDs are used for correlation and audit, not resume. Codex is the first supported telemetry adapter, and Cursor remains conditional on a real CLI conformance test.

## Consequences

- Provider transcript files are debugging inputs, not stable protocols or domain authority.
- A Codex-owned profile layer injects Pika hooks without copying the user's complete `CODEX_HOME`.
- Providers lacking reliable turn and conversation events may still run under Herdr, but automatic Follow-up and complete journaling are disabled.
