# Protocols

## 1. Instance discovery

One Herdr Workspace hosts one Pika Optimization and one daemon.

The daemon publishes this Workspace metadata token:

```text
pika_instance=<opaque-short-id>
```

The Unix socket is derived rather than stored as an arbitrary path:

```text
/tmp/pika-go-$UID/<instance>.sock
```

Inside Herdr, CLI, hook, and MCP proxy processes use inherited `HERDR_PANE_ID` and `HERDR_SOCKET_PATH` to resolve their current Workspace, read `pika_instance`, and derive the daemon socket. The underscore is required by Herdr's metadata-token key grammar. Outside Herdr, diagnostic commands require an explicit `--instance` or `--socket`.

The socket directory and socket are accessible only to the current user. The instance ID is routing metadata, not an authentication secret.

## 2. Daemon control API

The daemon serves HTTP/1.1 with JSON bodies over the Unix socket. Every mutating request accepts:

```json
{
  "request_id": "client-generated-idempotency-key",
  "expected_revision": 42
}
```

`request_id` gives safe replay. `expected_revision` is optional for commands that do not depend on a previously read view. Successful mutation responses include the committed domain revision and a stable receipt ID.

Initial endpoints:

| Method and path | Purpose |
| --- | --- |
| `GET /v1/health` | daemon liveness and protocol version |
| `GET /v1/status` | Optimization, active Work, queue, pane, session, and drain view |
| `GET /v1/init/options` | report whether configuration exists and return the selectable backend/model/effort catalog from successfully probed providers without writing state |
| `POST /v1/backups` | create and validate a new online SQLite snapshot at an absolute path |
| `POST /v1/update` | validate a staged local generation and begin zero-downtime process handoff |
| `POST /v1/init` | initialize and bind the caller pane |
| `POST /v1/baseline-drafts` | explicitly start a fresh Baseline Draft from an allowed paused state |
| `POST /v1/back-offs` | apply the allowed earlier-phase transition with a message |
| `POST /v1/works/{work_id}/cancel` | cancel the selected whole Work |
| `POST /v1/shutdown` | enter graceful draining |
| `POST /v1/scheduler/pause` | commit Scheduler Pause and synchronously interrupt active turns |
| `POST /v1/scheduler/resume` | continue surviving Sessions, then release held scheduling |
| `POST /v1/provider-events/{provider}` | ingest one provider hook event |
| `POST /mcp` | role-scoped MCP JSON-RPC endpoint used by `mcp-proxy` |

Every control response carries `X-Pika-Protocol-Version`. The CLI rejects a missing or different version before decoding or applying a response; `/v1/health` repeats the same version in its JSON body. Backup destinations must be absolute and absent. Backup is an operational snapshot and does not mutate the Optimization revision.

Scheduler control was added in protocol version 2. Both requests embed the standard `Mutation`. Their response is `{ "receipt": ..., "control": ... }`, where `control` contains the durable cycle and every per-Session result. Delivery is bounded to 15 seconds per Session, 32 concurrent Sessions, and 60 seconds overall. The HTTP response remains successful for `partial` delivery because the Scheduler state is already committed; the CLI prints the response and exits non-zero when an action is `failed` or `delivery_unknown`.

`/v1/health` also reports the process PID, binary digest, and handoff protocol. Hot update is process-level control state rather than an Optimization domain transition: durable progress is written to `runtime/daemon/update.json`, while the domain remains available through the inherited listener. The private parent/successor channel permits only `prepared`, `activate`, `ready`, and `commit`; it is never exposed on the public socket.

For a new instance, `POST /v1/init` requires the complete `configuration_toml`; an existing instance rejects a replacement candidate and keeps its file byte-for-byte. Initialization may take longer than ordinary control calls because it validates configuration, probes referenced provider executables and authentication, installs provider-owned integration resources, and prepares Git/SQLite state. The CLI therefore uses a 30-second HTTP ceiling for init while ordinary control calls keep the two-second ceiling. Provider-event ingestion has a separate ten-second ceiling because a single Hook may durably carry more than 16 MiB of complete Shell/MCP output.

`POST /v1/provider-events/{provider}` is the one intentionally unbounded JSON route on the permission-protected local socket so complete provider Shell and MCP results can reach SQLite. The route rejects a provider name that differs from the bound Agent Session. Other control and MCP routes retain their existing body limits.

Response errors have stable codes:

```json
{
  "error": {
    "code": "invalid_transition",
    "message": "integration may back off only to iteration",
    "details": {}
  }
}
```

Core codes include `daemon_not_initialized`, `revision_conflict`, `invalid_transition`, `work_not_found`, `work_terminal`, `work_conflict`, `forbidden`, `idempotency_conflict`, `runtime_unavailable`, `agent_lost`, `draining`, and `state_corrupt`.

`POST /v1/shutdown` acknowledges the durable transition; it does not mean the process has already exited. Clients may continue reading status while Pika waits for pending Work, runtime effects, and Agent Sessions. Socket disappearance is the observable successful completion of graceful shutdown.

## 3. CLI

The public CLI is a thin client of the control API except for commands that intentionally edit a local file.

```text
pika-go daemon [--instance ID]
pika-go init [configuration flags]
pika-go status [--json]
pika-go draft-baseline [--agent NAME]
pika-go back-off -m MESSAGE [--agent NAME]
pika-go cancel-work WORK_ID
pika-go pause
pika-go resume
pika-go shutdown
pika-go backup --output /absolute/path/to/snapshot.db
pika-go edit-instruction NAME
```

Internal entrypoints in the same binary:

```text
pika-go mcp-proxy
pika-go hook codex
pika-go hook cursor
```

`pika-go apply-best-update` remains an operational recovery/diagnostic entrypoint for an already-issued bounded Git Intent. Normal Integration Agents call the grant-scoped `apply_best_update` MCP instead, because their ordinary shell sandbox is not a control-plane transport.

`edit-instruction` discovers the Optimization Workspace from `--workspace`, `PIKA_GO_WORKSPACE`, or the current directory, validates `NAME` against the static catalog, then invokes `$EDITOR` directly on its `instructions/` overlay. The legacy Herdr instance lookup remains only for explicitly non-migrated instances.

`back-off` does not accept an arbitrary destination. The current phase selects the one declared transition; unsupported phases fail without mutation.

`init` automatically creates and starts the first Baseline Draft. `draft-baseline` is therefore a recovery/control command, not a normal extra stage: it is accepted only when Baseline Draft is the declared paused successor and always creates a fresh Agent Session. It returns `invalid_transition` if a draft is already active or the Optimization has advanced beyond the Baseline phase.

Herdr metadata tokens and workspace/tab/pane IDs are not durable Optimization identity. `pika-go open [WORKSPACE]` opens the immutable `workspace.json`, validates the source Git common-directory identity, creates a fresh Herdr layout, republishes metadata, and recovers SQLite/outbox state through fresh Agent Sessions. `kick-off` auto-discovers the same marker. Pika never guesses between multiple Workspaces, relocates one, or silently initializes over existing `pika.toml`/`pika.db`.

## 4. Herdr socket adapter

The daemon maintains one long-lived Herdr event subscription and fans observations out internally. Bootstrap order is:

1. Connect and send `events.subscribe`.
2. Receive subscription acknowledgement and buffer subsequent events.
3. Request `session.snapshot` on another connection.
4. Install the snapshot.
5. Apply buffered events in arrival order.

Required methods include:

- Workspace metadata read/report;
- pane create/split/get/list/close and process information;
- `agent.start`, `agent.prompt`, and Agent inspection;
- event subscription for Workspace, Pane, and Agent state;
- session snapshot.

Pika always stores IDs returned by Herdr. It does not predict Pane IDs, and it updates a Pane Binding when a pane move changes the public ID.

Herdr `agent.prompt --wait` is not used for Work completion because it observes settled status rather than a particular provider turn. Terminal MCP remains the completion protocol.

### Pane activity approximation

The accepted first release resets a Follow-up deadline on `pane.updated` for the target Pane Binding. It deliberately does not modify Herdr.

This is best effort:

- local key/text/paste forwarding does not directly publish `pane.updated`;
- `pane.send_text` and `pane.send_input` do not publish it either;
- pane metadata, Agent-name reconciliation, or stripped terminal-title changes can publish it; ordinary Agent status/end events use other event kinds.

Consequently `pane.updated` is named `observed_pane_activity` inside Pika, never `human_input`. Status output must state that strict unsubmitted-input detection is unavailable.

## 5. Agent launch and identity

Before `agent.start`, Pika prepares the target shell environment with a short-lived grant and provider overlay selection. Secrets are not placed in Agent argv.

Conceptually:

```text
PIKA_INSTANCE_ID
PIKA_SESSION_ID
PIKA_MCP_GRANT
provider-specific profile/plugin selector
```

The grant binds:

```text
Optimization ID
Work ID and generation
Role
Agent Session ID
Pane Binding
allowed MCP catalog
expiry/revocation state
```

Only a hash of the bearer grant is stored. A terminal Work, cancelled Work, replaced session, or closed pane revokes it.

Every recovery creates a new Pika Agent Session ID and new grant. Provider-native session ID is added later by hooks or Herdr integration for correlation only.

Each Agent Session also records the provider kind, probed executable version, and capability snapshot. Provider-native session, turn, and tool identities are scoped by both provider and Pika Agent Session; identical native IDs in another adapter or sibling Session cannot bind or deduplicate across that boundary.

## 6. MCP transport and application

The coding agent starts `pika-go mcp-proxy` as a stdio MCP server. The proxy:

1. Reads the inherited instance/session/grant binding.
2. Discovers the daemon socket.
3. Forwards MCP JSON-RPC requests to `POST /mcp`.
4. Returns the daemon's MCP result over stdio.

The proxy contains no domain logic. The daemon owns tool discovery, input validation, Role authorization, file contracts, idempotency, and domain transactions.

Domain-mutating tools require `idempotency_key`. Pika hashes the canonical request:

- same key and same request returns the original receipt;
- same key and different request returns `idempotency_conflict`;
- transport loss after commit is recovered by retrying the same key.

Integration uses three explicit MCP steps: `prepare_best_update` commits a bounded Intent, `apply_best_update` idempotently applies only that Intent inside the daemon control plane, and terminal `finish_integration` verifies the Git postcondition before advancing SQLite Best. `apply_best_update` uses the unique Intent ID as its idempotency identity and rejects an Intent not owned by the active Integration grant. This avoids depending on a coding Agent's ordinary shell sandbox for Unix-socket access.

Baseline Draft and Iteration use non-terminal `commit_changes` for the same sandbox reason. The tool is available only to those active Role grants, accepts a non-empty explicit list of repository-relative literal paths, rejects `.git`, path escapes, and any pre-existing staged change outside the authorized path set, and writes a Work-scoped idempotency/request digest into Git trailers. It never silently absorbs unrelated user index state. A lost response returns the same HEAD on retry; the same key with a different message/path set fails. The returned `commit_sha`, `clean`, and status are Git evidence for the terminal Role operation.

`submit_baseline_definition` accepts only a clean repository. The Tool Application observes its HEAD and includes that Repository Snapshot SHA in the same idempotent domain command that stores the Definition bytes and digest. The two identities are deliberately separate: a tracked Definition cannot contain the SHA of the commit that contains itself. Before `finish_baseline_verification(accepted)` creates Best revision 0, Pika re-reads the source snapshot and requires both the same HEAD and an empty worktree status. A committed or uncommitted change therefore forces rejection or a successor Baseline Revision rather than silently changing the accepted snapshot.

The domain transaction also enforces `benchmark_integrity` schema version 1. A Baseline Definition freezes the exact Case IDs, formal repeat and invocation counts, device-resident protected inputs and Oracle outputs, per-invocation restore/check protocol, compact deferred host transfer, and kernel/end-to-end timing boundaries. Accepted Baseline Verification evidence and Integration validation must cover exactly that Case set and prove `input_restores == checked_invocations == repeats * (warmups + measured)` with no mismatch, nonfinite value, or canonical-input mutation. Integration validation additionally declares primary and maximum per-Case speedup. Either value at or above 10x requires a passing independent retest with changed canonical inputs, output sentinel, cold-start/setup/steady-state/end-to-end reporting, and a fair timing boundary before Pika will create a Git Intent.

Large domain evidence may be submitted by relative file path. The daemon resolves it beneath the Work root, rejects symlink/path escapes, reads a stable snapshot, and records path, size, and digest with the result.

## 7. Codex hook adapter

`pika-go init` installs a Pika-owned Codex profile layer and launches Pika Codex Agents with that profile. The hook command is short, synchronous, fail-open, and writes no model-visible output:

```text
pika-go hook codex
```

It reads one JSON object from stdin and posts it to `/v1/provider-events/codex`. Routing uses the Herdr pane environment plus the Pika session grant.

Event normalization:

| Codex event | Pika journal effect |
| --- | --- |
| `SessionStart` | bind provider session ID and transcript path |
| `UserPromptSubmit` | start/update Turn and save user message |
| `PostToolUse` | save tool name, input, response, tool-use ID, and Turn correlation, including non-zero Bash results |
| `Stop` | save latest assistant message and mark Turn stopped |
| `SessionEnd` | save provider end observation and flush correlation |

Pika may store `transcript_path`, but it does not parse the transcript as a stable completion or recovery protocol.

The Herdr-owned Codex hook remains installed and unchanged. Matching hooks coexist; Pika does not claim Herdr lifecycle authority.

## 8. Codex profile overlay

Pika does not clone `~/.codex` or create a separate `CODEX_HOME`. `CODEX_HOME` includes authentication, sessions, logs, caches, skills, plugins, and other user state, so a copy would become stale and duplicate secrets.

Instead, initialization owns one namespaced layer:

```text
$CODEX_HOME/pika-go-managed.config.toml
```

Pika launches:

```text
codex --profile pika-go-managed ...
```

The base user configuration, authentication, Herdr hook, and other user hooks still load. The Pika profile supplies only Pika MCP integration, required feature flags, and inline `[hooks]` tables that invoke `pika-go hook codex`. Its MCP entry explicitly forwards `PIKA_GO_SOCKET` and `PIKA_MCP_GRANT`; without that allowlist the stdio child cannot reach or authenticate to the daemon. Pika auto-approves only the tools present in the Role-scoped grant catalog.

The instance wrapper adds the Session-frozen Pika System Prompt as a per-launch `developer_instructions` override and launches Codex with `--yolo` by default. This is the default unattended Agent sandbox policy and enables external network/H20 tooling; a future per-Role sandbox policy may replace it. Git metadata commits and Best application still use scoped MCP control-plane tools. Matching hooks from the base config and profile are additive. The profile name is reserved; Role Agent configuration cannot supply a second `--profile` argument.

Codex may require the user to trust the five Pika-managed hooks on first launch. Pika never bypasses hook trust automatically. Once Codex records its per-hook SHA-256 trust state, subsequent Pika profile refreshes preserve only valid entries keyed to those exact managed hooks and profile path. Other hook trust is not copied, and changed Pika hooks require review again.

## 9. Cursor hook adapter

When Cursor is referenced, init additively installs event-specific commands in `~/.cursor/hooks.json` while preserving existing Cursor and Herdr hooks. The command carries the event name explicitly because some observed CLI payloads omit it:

```text
pika-go hook cursor <event>
```

The hook forwards one complete JSON object plus the route event name to `/v1/provider-events/cursor`. Normalization accepts the pinned CLI's snake_case and camelCase payload variants, preserves the raw payload, and emits provider-neutral journal records:

| Cursor event | Pika journal effect |
| --- | --- |
| `sessionStart` | bind conversation/session identity and return the frozen prompt as `additional_context` |
| `beforeSubmitPrompt` | open/update the generation Turn and save the user message |
| `afterAgentResponse` | save the full assistant message |
| `postToolUse` | save successful tool input/output and duration |
| `postToolUseFailure` | save error/failure type, duration, and interruption state |
| `afterShellExecution` | supplement the logical tool with full shell output |
| `afterMCPExecution` | supplement the logical tool with the full MCP JSON result |
| `stop` | close the Turn as completed, aborted, or error and arm Follow-up eligibility |
| `sessionEnd` | record provider Session end independently of Turn completion |

Duplicate and reordered delivery is safe. A late prompt or response cannot regress a terminal Turn, and an unmatched Shell/MCP supplement is retained. The synchronous Cursor `followup_message` response is not used; Pika generates Follow-up in a separate configured Agent Session and delivers it through Herdr.

Init also installs one static `pika_go` stdio entry in `~/.cursor/mcp.json`. Cursor config interpolation resolves the launched Session's executable, socket, grant, and Session ID from its environment. Every tool input schema emits `required` as an array, including `[]` for zero-argument tools, because the pinned Cursor validator rejects JSON Schema `null` that more permissive clients tolerate. The frozen Cursor System Prompt identifies and defines both Context Bundle files, then calls `GetDynamicTools(namespace="pika_go")` and uses `CallDynamicTool`; direct Shell execution of `mcp-proxy` is not an accepted Role completion path.

The wrapper supplies the kickoff as Cursor's positional initial prompt, avoiding a startup `agent.prompt` race. Pika accepts Herdr's successful `agent.start` response for this provider without waiting for `interactive_ready`: Cursor may already be executing the positional prompt, and synchronously waiting for an idle prompt would block later outbox effects. The Session grant is active in both `starting` and `running`, and the binding promotes it to `running` from the launch observation. Later Follow-up delivery uses Herdr `agent.prompt`; the adapter retries the pinned TUI's dropped synthetic Enter without terminal scraping. The `pane.updated` and provider-reported user message caused by Pika's own prompt are ignored while the request is durably `dispatching`, so delivery cannot supersede itself; user/observed activity during generation still supersedes the request.

The pinned Cursor Adapter probes exact version `2026.08.25-3e8eec8` and authenticated `status`. Any mismatch is a startup/init failure. Runtime never falls back to Codex.

## 10. Follow-up delivery ordering

When `submit_followup_message` commits:

1. Lock the Follow-up Request and re-read target Work, current Agent Session, terminal receipt, and latest observed pane activity.
2. If terminal, replaced, cancelled, draining-ineligible, or superseded, store the generated message and mark it discarded.
3. Otherwise mark `dispatching` with a delivery ID.
4. Call Herdr `agent.prompt` without `--wait`.
5. On definite success mark `delivered`; provider prompt hooks later correlate the new Turn.
6. On definite rejection return to pending or fail per policy.
7. On lost response mark `delivery_unknown`; do not blindly duplicate the prompt.

User activity while generation is running supersedes the request but never interrupts the generator Agent.
