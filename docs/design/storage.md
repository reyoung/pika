# Storage

## 1. Authority and transaction model

Each Optimization owns one SQLite database at:

```text
$HERDR_PLUGIN_STATE_DIR/instances/<instance>/pika.db
```

SQLite runs with WAL, foreign keys, a busy timeout, explicit migrations, and one logical writer. Domain state, operation receipts, domain events, and runtime outbox effects that arise from one command are committed in one transaction.

Herdr persists terminal layout; Pika persists desired workflow and correlations. Neither database is copied into the other.

## 2. Conceptual schema

### Domain

| Table | Purpose |
| --- | --- |
| `optimizations` | singleton identity, lifecycle status, domain revision, repository |
| `baseline_revisions` | immutable definitions and accepted/rejected verification result |
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

### Reliability

| Table | Purpose |
| --- | --- |
| `operation_receipts` | idempotency key, canonical request digest, committed response |
| `domain_events` | ordered audit of committed domain transitions |
| `runtime_outbox` | Herdr/provider effects that must be dispatched or reconciled |
| `migrations` | applied schema version and checksum |

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

SQLite `TEXT` is used for valid UTF-8 JSON/text and `BLOB` for arbitrary bytes. The database preserves the full payload; previews, token budgets, and relevance filtering are concerns of Context Builder queries, not retention.

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

Context generation is bounded and deterministic:

1. Select current domain facts and required terminal operation.
2. Select recent and relevant Turns across all Sessions for the Work.
3. Include concise tool-output excerpts only when relevant.
4. Materialize a complete `messages.jsonl` or evidence view under the instance context directory when the Role needs a file.
5. Record the query inputs and generated digest on the new Agent Session.

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

- Database and state directories are mode `0700`; the database and socket are user-only.
- Raw bearer grants and provider credentials are never persisted; store hashes or references.
- Tool output may contain secrets. It is never printed by default in `status` and is supplied to later Agents only through explicit Context Builder selection.
- Journal and tool data are retained for the lifetime of the Optimization. Deleting an Optimization is a separate explicit destructive command and is not implied by shutdown.
- Backups copy the SQLite database using a SQLite-safe online backup/checkpoint procedure, not a blind copy of a live WAL set.

## 8. Recovery invariants

- Every Work generation has at most one current Agent Session.
- A provider event is accepted only when its pane/session binding matches the current or a known historical Session.
- Old or out-of-order events remain auditable but cannot mutate current Work.
- A terminal operation transaction includes its receipt, result, domain event, and successor outbox effect.
- Runtime effect uncertainty never causes a second domain transition.
- Schema migration failure prevents scheduling and leaves the prior database recoverable.
