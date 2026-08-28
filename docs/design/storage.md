# Storage

## 1. Authority and transaction model

Each Optimization owns one SQLite database at:

```text
<optimization-workspace>/pika.db
```

SQLite runs with WAL, `synchronous=FULL`, foreign keys, a busy timeout, explicit migrations, and one logical writer. Domain state, operation receipts, domain events, and runtime outbox effects that arise from one command are committed in one transaction. `workspace_identity` mirrors the immutable JSON identity and rejects a database opened under a different Workspace; `git_worktrees` records the base, Best, and Attempt branch/path/HEAD registry.

Herdr persists terminal layout; Pika persists desired workflow and correlations. Neither database is copied into the other.

## 2. Conceptual schema

### Domain

| Table | Purpose |
| --- | --- |
| `optimizations` | singleton identity, lifecycle status, domain revision, repository |
| `baseline_revisions` | immutable Definition bytes/digest, frozen Repository Snapshot SHA, predecessor identity, and accepted/rejected verification projection |
| `best_revisions` | accepted Best history and Git/evidence identity |
| `attempts` | candidate identity, hypothesis, terminal status |
| `iteration_rounds` | immutable per-Attempt execution rounds and stale/back-off ancestry |
| `integrations` | FIFO position, intent, evidence, Git postcondition, decision |
| `works` | Role-assigned durable execution state and generation |
| `back_offs` | source phase, successor identity, message, receipt |

### Agent runtime and conversation

| Table | Purpose |
| --- | --- |
| `agent_sessions` | Pika Session, provider session ID, Work/generation, Role, Agent config, pane binding, lifecycle |
| `pane_bindings` | Herdr workspace/tab/pane/terminal correlation and last runtime observation |
| `conversation_turns` | provider Turn ID, sequence, user message, assistant message, start/stop/end observations |
| `tool_events` | complete observable tool input and output, identifiers, ordering, status, timing |
| `provider_events` | deduplicated raw hook envelope for audit and forward-compatible reprocessing |
| `followup_requests` | target, inactivity deadline, generation attempts, message, delivery, supersession |
| `instruction_snapshots` | user overlay plus the byte-exact frozen rendered System Prompt and digest for one Agent Session |
| `context_snapshots` | frozen Context Bundle paths, schema version, SHA-256 digests, byte sizes, and JSONL record count |

### Reliability

| Table | Purpose |
| --- | --- |
| `operation_receipts` | idempotency key, canonical request digest, committed response |
| `domain_events` | ordered audit of committed domain transitions |
| `runtime_outbox` | Herdr/provider effects that must be dispatched or reconciled |
| `migrations` | applied schema version and checksum |
| `workspace_identity` | immutable Workspace/source Git identity mirrored from `workspace.json` |
| `git_worktrees` | durable role/Attempt to branch, repository path, HEAD, and lifecycle mapping |

## 3. Full shell and tool output in SQLite

All tool input and all output visible to a supported provider hook are stored directly in `tool_events`. There is no output-size spill to external artifact files.

Suggested fields:

```text
id
agent_session_id
conversation_turn_id
provider_tool_use_id
sequence
tool_name
tool_input_json_or_blob
tool_output_json_or_blob
output_encoding
exit_status
started_at
finished_at
byte_size
content_sha256
```

SQLite `TEXT` is used for valid UTF-8 JSON/text and `BLOB` for arbitrary bytes. The database preserves the full payload; status previews and token budgets do not affect retention or Context Bundle completeness.

This choice intentionally favors one-file state and simple backup over database size. Implementations must not impose a hidden truncation limit. If disk growth later becomes a real problem, retention or compression requires a new explicit design decision and migration.

Provider coverage remains factual: Pika stores every event the adapter can observe, not invisible hosted-provider internals.

## 4. Domain evidence files

“All tool output in SQLite” does not eliminate Work-owned evidence files. Agents may still produce benchmark logs, definitions, reports, patches, and other domain artifacts in their assigned workspace or Pika evidence directory.

When an MCP result references a file, Pika records:

- normalized relative path and owning Work;
- size and SHA-256;
- file contract/schema version;
- submission and receipt identity.

Context Bundles may reference these files rather than copying large domain documents into prompt text. Tool output itself remains in SQLite even if it mentions or produced the file.

## 5. Conversation Journal

The normalized journal saves:

- every observed user prompt;
- every observed final assistant message;
- full observable tool input/output;
- Role MCP calls and receipts;
- provider and Herdr lifecycle observations;
- session and Turn ordering across fresh recovery Sessions.

Provider transcript paths are metadata. Pika does not depend on an undocumented transcript schema to rebuild history.

Context generation is complete and deterministic:

1. Select current domain facts and required terminal operation.
2. Select every normalized Turn across all Sessions for the relevant Work.
3. Include complete observable tool and shell inputs and outputs, MCP receipts, and supplements; do not duplicate raw provider-hook events.
4. Atomically materialize `contexts/<session-id>/context.json`, current-Work `messages.jsonl`, the immediately preceding Round's complete `previous-round/round-N/messages.jsonl` when present, and selected on-demand `attempt-history/<attempt-id>/{messages,summary}.jsonl` before provider launch.
5. Record both relative paths, SHA-256 digests, byte sizes, record count, and schema version in `context_snapshots`.

The full JSON Schemas for the context document, normalized message records, and terminal Attempt summary records are versioned with the producer and embedded verbatim in every consuming System Prompt. The locator section is rendered from an embedded Go template with missing-key failures. A dispatch retry verifies every referenced digest and reuses the frozen files; a new or recovery Agent Session creates a new reviewable snapshot.

When Baseline Verification rejects or supersedes a revision, the historical view retains its failure kind, reason, requested changes, and evidence. The successor Baseline Draft projection copies none of that into a mutable Definition automatically; it exposes the predecessor facts to the new Session so the Agent can make an explicit revision. Baseline Revision ID is durable identity, while Work and Agent Session IDs remain execution-only.

Definition identity and repository identity are not conflated. Submission stores the exact Definition bytes and digest plus the clean HEAD observed as `repository_sha`. The tracked Definition never needs to embed the commit that contains itself. Verification receives both identities and acceptance rechecks that repository HEAD and worktree cleanliness still match before seeding Best revision 0.

## 6. Follow-up activity data

For each eligible stopped Turn, persist:

```text
target_session_id
target_turn_id
last_observed_pane_activity_at
inactivity_timeout_ms
followup_due_at
activity_source = pane.updated
```

Because `pane.updated` is approximate, the database stores the observed source rather than asserting that a human acted. A status/debug view can therefore explain why a deadline moved.

## 7. Security and retention

- Workspace state directories are mode `0700`; the manifest, configuration, database, and lock are user-only.
- Raw bearer grants and provider credentials are never persisted; store hashes or references.
- Tool output may contain secrets. It is never printed by default in `status`; Context Bundle directories and files are user-only and intentionally include the complete normalized history needed by the relevant Work.
- Journal and tool data are retained for the lifetime of the Optimization. Deleting an Optimization is a separate explicit destructive command and is not implied by shutdown.
- Backups copy the SQLite database using a SQLite-safe online backup/checkpoint procedure, not a blind copy of a live WAL set.
- `pika-go backup --output <absolute-path>` refuses overwrite and live SQLite paths, performs a passive WAL checkpoint plus `VACUUM INTO`, applies mode `0600`, and validates `quick_check` and the exact supported schema before success.
- `status --json` reports database/WAL bytes, SQLite page count/size, raw provider-event bytes, and Tool payload bytes. It reports sizes only and never emits retained payloads or grant tokens.

## 8. Recovery invariants

- Every Work generation has at most one current Agent Session.
- A provider event is accepted only when its pane/session binding matches the current or a known historical Session.
- Old or out-of-order events remain auditable but cannot mutate current Work.
- A terminal operation transaction includes its receipt, result, domain event, and successor outbox effect.
- Runtime effect uncertainty never causes a second domain transition.
- Schema migration failure prevents scheduling and leaves the prior database recoverable.
- Workspace relocation or source Git common-directory identity drift prevents daemon startup; recovery never resets, cleans, rebases, or checks out an active worktree.
