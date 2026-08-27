# Incremental Development Plan

## 1. Delivery strategy

Pika-Go should be built as a sequence of executable vertical slices. Each phase ends with an observable workflow that crosses the interfaces introduced in that phase. A phase is not complete merely because packages compile or isolated mocks pass.

The ordering follows four rules:

1. Make domain transitions deterministic before involving a coding agent.
2. Prove the Herdr runtime with a scripted fake Agent before involving Codex.
3. Complete the Baseline Draft/Verification loop before implementing concurrent Attempts.
4. Add automatic Follow-up only after conversation and turn telemetry is trustworthy.

The first release formally supports Codex telemetry. OpenCode and Cursor may run through Herdr for direct user interaction, but their complete journal and automatic Follow-up remain disabled until their provider adapters pass conformance.

### Implementation checkpoint — 2026-08-27

| Phase | Implementation state | Current evidence / remaining release gate |
| --- | --- | --- |
| 0–4 | Functional slices implemented; local crash gates pass | Domain, Unix control plane, metadata-based instance discovery, real Herdr runtime, Baseline, idempotent receipts, and fresh-Session recovery pass. Baseline submission separately freezes Definition identity and a clean Repository Snapshot SHA; acceptance rejects committed or uncommitted repository drift. Domain `before_commit`/`after_commit` injection and real daemon crashes at running Session, dispatching start outbox, and committed terminal receipt all recover without duplicate Work or Agent. |
| 5 | Implemented; local workflow and crash gates pass | Concurrent Attempt/FIFO Integration, Git intent, stale/back-off, cancellation, and scheduler-config propagation tests pass. Existing user configuration is preserved and its scheduler values are frozen into the Optimization at init. A failed Git pre-commit retries the same staged intent, a response-loss retry returns the existing Best commit, and `finish_integration` before/after database-commit failures produce exactly one Best transition. A real daemon crash after the Best Git commit but before Domain finish recovers through a fresh Integration Session, reuses the pending intent, and records the same SHA exactly once. |
| 6 | Implemented; real-model core path passes | Immutable embedded Role System Prompts, runtime-rendered dynamic System Context, empty user overlays, per-Session prompt freezing, Codex managed profile/hooks, and full observable journal payload storage pass local tests. A disposable real-Codex Optimization completed Draft, Verification, Iteration, Integration, scoped Git commits and Best application, and graceful drain. |
| 7 | Implemented; real-model smoke passes | Fake-clock budget/race/exhaustion tests pass. Real Herdr with deterministic Agents passes Follow-up generation and delivery. A production-equivalent real-Codex short-timeout smoke generated and delivered a target-specific Follow-up to a still-running Verification Work, then completed the Optimization. |
| 8 | Hardening and full local release matrix pass; remote CI open | Online validated backup, storage growth view, migration/integrity rejection, permissions, structured daemon logs, protocol compatibility, cross-build archives/checksums, isolated macOS archive-link smoke, real-Herdr graceful drain, and process crash recovery pass locally. Running Session, dispatching outbox, committed terminal receipt, and post-Git/pre-Domain Integration crash scenarios pass two consecutive real-process rounds. The real-Codex matrix now also passes Follow-up, fresh-Session daemon recovery, parallel sibling-isolated cancellation, Integration Back-off, Best advancement, and graceful drain. CI contains macOS/Linux clean-link jobs; confirmed remote CI success remains the release blocker. |

The checkpoint distinguishes code present in the tree from release acceptance. It must be updated with command output or a concrete blocker; starting a test or compiling one package is not completion evidence.

Local verification recorded on 2026-08-27:

- `make verify` passed formatting, `go vet ./...`, and `go test -race ./...` for every package.
- `make herdr-integration` passed one real-Herdr runtime conformance test and ten real-Herdr workflow tests, including graceful drain, FIFO Integration, and the four daemon process-crash boundaries listed above.
- `make crash-integration` passed two consecutive rounds of all four real daemon process-crash scenarios (eight crash/recovery executions).
- `make real-codex-integration` passed the complete disposable real-Codex release matrix. A dedicated Follow-up Session delivered guidance to a still-running Verification Work; a daemon process crash replaced that target with a fresh Pika Session and a distinct provider session; Baseline Revision 1 was accepted; two Iteration Sessions ran concurrently; cancelling one Attempt left its sibling pending; the first Integration backed off to a new Round with its message preserved; and a later Candidate advanced `pika/best` from sequence 0 to 1. The run drained normally with 12 distinct provider sessions and 106 observable tool events retained. Baseline Draft and Iteration committed through grant-scoped `commit_changes` while coding Agents remained in `workspace-write`.
- `make dist VERSION=0.1.0-dev` built Darwin/Linux archives for `amd64` and `arm64`; `shasum -a 256 -c SHA256SUMS` verified all four archives.
- The extracted native Darwin/arm64 archive reported version `0.1.0-dev` and passed isolated `herdr plugin link`/`plugin list` with disposable XDG directories.
- These local results do not satisfy the explicitly open remote Linux/macOS CI gate in the table.

```mermaid
flowchart LR
    P0[0 Skeleton] --> P1[1 Baseline domain]
    P1 --> P2[2 Control plane]
    P2 --> P3[3 Herdr runtime]
    P3 --> P4[4 Baseline E2E]
    P4 --> P5[5 Attempt and Integration]
    P5 --> P6[6 Codex and Journal]
    P6 --> P7[7 Follow-up]
    P7 --> P8[8 Release hardening]
    P8 -. post-release .-> P9[9 Cursor and OpenCode]
```

## 2. Module seams

The implementation should keep the external interfaces from [Architecture](architecture.md) small and put complexity behind them.

| Module | Interface | Hidden implementation | First exercised |
| --- | --- | --- | --- |
| Domain Engine | `Apply(Command) -> Receipt`, `Inspect(Query) -> View` | validation, SQLite transaction, idempotency, events, outbox, lifecycle invariants | Phase 1 |
| Work Projector | internal `Project(State) -> DesiredWork` | static Role scheduling, concurrency, FIFO Integration, draining rules | Phase 1, extended Phase 5 |
| Work Runtime | `Start`, `Prompt`, `Close`, `Snapshot`, `Events` | Herdr NDJSON, pane identity, subscriptions, snapshot/reconnect, launch semantics | Phase 3 |
| Tool Application | `Catalog(Grant)`, `Invoke(Grant, Call)` | MCP authorization, schema validation, file contracts, terminal transactions | Phase 4 |
| Context Builder | `Build(RoleActivation) -> ContextBundle` | domain projection, dynamic System Context, user-instruction freezing, journal selection, evidence paths | Phase 4, extended Phase 6 |
| Git Workspace | `CommitChanges`, `CreateAttempt`, `RefreshFromBest`, `PrepareBestUpdate`, `ApplyAuthorizedBestUpdate`, `VerifyBestUpdate` | scoped commits, worktrees, merge policy, idempotent Git intent/postconditions, digests | Phase 5 |
| Provider Telemetry | `Normalize(Binding, HookEvent) -> JournalEvents` | Codex wire fields, deduplication, session/Turn correlation, tool payloads | Phase 6 |
| Follow-up Coordinator | internal state transition interface | clocks, eligibility, budget, supersession, generator sessions, delivery uncertainty | Phase 7 |

Do not introduce a generic repository interface over SQLite in the first release. SQLite is the only domain-state adapter. The useful seam is the Domain Engine interface, not one CRUD method per table.

`WorkRuntime` and `ProviderTelemetry` are real seams because they have multiple adapters: scripted/in-memory versus Herdr, and Codex versus future Cursor/OpenCode. Time should be injected into Follow-up logic because a fake clock materially changes testability.

## 3. Test fixtures built first

Two small test programs prevent model and terminal behavior from contaminating workflow tests:

- `fake-agent`: a deterministic interactive process registered through a test-only Herdr Agent manifest so Herdr can detect/start it. A script tells it when to become idle, emit provider-like events, call MCP, hang, exit, or accept a prompt.
- `pika-go test-driver`: waits for daemon health or kills a separate daemon process when SQLite and Git observe a declared point. The current points are a running Agent Session, a dispatching outbox effect, a committed terminal-operation receipt, and an applied Best Git intent that has not reached Domain finish. It emits a machine-readable record of the point it reached; `fake-agent` remains responsible for deterministic MCP calls.

They are test-only adapters, not alternate production runtimes. System tests use real Unix sockets and SQLite files. Herdr conformance tests use a real local Herdr process; most domain tests do not.

## 4. Phase 0 — distributable skeleton

### Outcome

A platform-specific release directory can be linked as a Herdr plugin, open a visible Symphony pane, and run a health-checkable daemon.

### Build

- Go module, command root, version/protocol constants, structured errors, and signal handling.
- `pika-go daemon`, `pika-go status`, and internal test-driver entrypoints.
- Unix-socket HTTP server with `/v1/health`; no domain mutations yet.
- `herdr-plugin.toml`, embedded System Prompt and empty instruction-overlay packaging layout, release layout, and macOS/Linux cross-build jobs. Final System Prompt content remains a Phase 6 review gate.
- Test fixtures and a process harness that never depends on a real coding model.

### Exit gate

- `gofmt`, `go vet ./...`, `go test -race ./...`.
- Build the supported OS/architecture matrix.
- `herdr plugin link` followed by opening the `symphony` pane.
- A separate pane reaches `/v1/health` through the Unix socket.
- Restart detects a stale socket and removes it only after lock/liveness checks prove that no daemon owns it.

### Deliberately absent

SQLite domain state, Agent launch, MCP, Codex hooks, and workflow scheduling.

## 5. Phase 1 — Baseline domain kernel

### Outcome

The Domain Engine can initialize an Optimization and deterministically execute the Baseline Revision lifecycle without Herdr or MCP.

### Build

- SQLite migrations, WAL/foreign-key configuration, online-open validation, and schema versioning.
- Optimization, Baseline Revision, frozen Repository Snapshot SHA, Baseline Verification, Work, operation receipt, domain event, and runtime outbox records.
- `Init`, `SubmitBaselineDefinition`, `FinishBaselineVerification`, `BackOff`, `CancelWork`, and `RequestShutdown` commands.
- Baseline-only Work Projector and immutable transition views.
- Atomic receipt replay: the transaction includes result, receipt, domain events, and outbox effects.

### Exit gate

- Table-driven tests for every allowed and rejected Baseline transition.
- Same idempotency key/same body returns the original receipt; same key/different body fails.
- Reopen the database after every declared transaction crash point and obtain the same projected Work.
- Rejected verification creates a successor Baseline Revision without rewriting history.
- The historical revision and successor Draft projection expose the prior `failure_kind`, reason, requested changes, and verification evidence; execution Work/Session IDs are never treated as durable Definition identity.
- Verification never materializes an inline Definition into the frozen repository or conflates the daemon's stored-JSON digest with an independently formatted file digest.
- `idle`, `done`, or arbitrary text cannot be expressed as a domain-completion command.

### Deliberately absent

Herdr IDs, provider IDs, Git worktrees, Attempt/Integration tables, and network transport inside domain types.

## 6. Phase 2 — instance and control plane

### Outcome

The real CLI controls one durable Optimization through the daemon socket, including initialization failure and graceful draining semantics, without launching Agents.

### Build

- Plugin config/state path resolution and per-instance database layout.
- Workspace instance discovery contract, socket ownership/permissions, and single-daemon lock.
- HTTP/JSON endpoints for init, status, Baseline recovery, Back-off, cancel Work, and shutdown.
- CLI request IDs, revision conflicts, JSON status output, stable exit codes, and human-readable errors.
- Outbox dispatcher abstraction, initially backed by a recording adapter.

### Exit gate

- CLI-to-daemon-to-SQLite system tests over a real Unix socket.
- A second daemon cannot own the same instance.
- Replayed CLI mutations return stable receipts.
- Init failure returns non-zero, records diagnostics, exits the daemon, and leaves no active Work.
- Shutdown enters `draining`, rejects new primary Work, and waits rather than cancelling recorded children.
- The daemon remains reachable while any Work, runtime effect, or Agent Session is non-terminal, then removes its socket and exits automatically.

### Deliberately absent

Real Herdr pane effects and any provider process.

## 7. Phase 3 — Herdr runtime adapter

### Outcome

Pika can reconcile durable desired sessions with real Herdr panes using `fake-agent`, while Herdr observations remain non-authoritative for Work completion.

### Build

- Raw NDJSON client, capability/version check, `events.subscribe` plus `session.snapshot` bootstrap, reconnect, and snapshot reconciliation.
- Workspace metadata publication with `pika_instance`.
- Pane creation/split, `agent.start`, `agent.prompt`, process inspection, pane close, and public-ID move tracking.
- Agent Session and Pane Binding lifecycle records.
- Runtime outbox claim/dispatch/acknowledgement with uncertain-effect reconciliation.
- Fresh-session replacement after a lost pane or process.

### Exit gate

- A real Herdr session starts and prompts `fake-agent` in a managed pane.
- Killing and restarting only the daemon leaves the child untouched while the daemon is absent, then retires/closes the old binding and starts a fresh Agent Session on recovery.
- Moving a pane updates the binding without changing Work identity.
- Herdr `idle`, `done`, `unknown`, pane title, and Agent exit never complete Work.
- Graceful daemon shutdown does not send interrupt keys or close a non-terminal child.
- A real Herdr Agent can receive shutdown, finish through its terminal MCP, close normally, and only then permit daemon exit.

### Deliberately absent

Role MCP and real Codex. Runtime tests complete Work only through the test driver.

## 8. Phase 4 — MCP and Baseline vertical slice

### Outcome

`pika-go init` turns its caller pane into Baseline Draft; the terminal MCP call closes that Agent and automatically starts an independent Baseline Verification session. Acceptance or rejection follows the designed state machine.

### Build

- Stdio `mcp-proxy`, daemon-side MCP endpoint, grant minting/hash/revocation, and Role-scoped catalogs.
- `get_context`, grant-scoped `commit_changes`, `submit_baseline_definition`, and `finish_baseline_verification`.
- Submission-time clean-worktree validation and a daemon-observed Repository Snapshot SHA stored separately from Definition identity; acceptance rechecks both HEAD and cleanliness before creating Best revision 0.
- File-path normalization, symlink/path-escape rejection, size/digest receipts, and frozen instructions.
- Baseline Context Builder and activation prompt.
- Normal terminal-operation pane lifecycle and successor scheduling.
- Scripted fake-Agent scenarios for accepted, rejected, duplicate, malformed, expired, and stale-generation calls.

### Exit gate

- Real Herdr end-to-end: daemon pane plus init pane, Baseline Draft, terminal MCP, fresh Verification session.
- Rejected verification automatically creates a new revision and fresh draft session.
- A replayed terminal call cannot create two successor Works.
- An old session grant cannot mutate the successor revision.
- A tracked Definition never has to contain its own commit SHA; committed or uncommitted repository drift after submission prevents acceptance.
- The terminal MCP response is flushed back to the Agent before normal pane closure begins.
- Init failure closes the instance; successful init reuses the caller pane as specified.

This is the first usable product slice. Do not begin Iteration concurrency until it is reliable.

## 9. Phase 5 — Attempt, Integration, and Git loop

### Outcome

A deterministic fake-Agent Optimization can progress from an accepted Baseline through concurrent Iteration, serialized Integration, and multiple Best revisions.

### Build

- Attempt, Iteration Round, Integration, and Best persistence plus their Work projection.
- Configurable Iteration concurrency, one pane per active Work, FIFO single-concurrency Integration.
- Iteration and Integration MCP catalogs, including scoped Candidate `commit_changes` and the three-step `prepare_best_update`/`apply_best_update`/`finish_integration` protocol.
- Git Workspace module: isolated worktrees, exact Git intent, postcondition verification, and `pika/best` ownership.
- Stale Integration and user Back-off to a new Iteration Round using `git merge`, never rebase/reset.
- Work cancellation and sibling isolation.

### Exit gate

- At least three concurrent fake Attempts finish out of order while Integration consumes FIFO order.
- Only a verified accepted Integration advances Best.
- A stale candidate returns to Iteration, receives the new Best context, merges it, and remeasures.
- Back-off preserves prior rounds and its message appears in the successor Context Bundle.
- Cancelling one Attempt does not change sibling Work or Best.
- Crash injection before/after Git intent and database commit never applies two Best transitions.
- Best application runs in the grant-scoped Pika MCP control plane, not the coding Agent's ordinary shell sandbox; the recovery CLI remains an operational path for the same bounded intent.

### Deliberately absent

Automatic Follow-up and Cursor telemetry.

## 10. Phase 6 — System Prompts, user instructions, Codex, and Conversation Journal

### Outcome

The complete workflow can run with real Codex sessions while Pika records session, Turn, message, and observable tool input/output data in SQLite.

### Build

- Migrate and review the legacy Role System Prompts as immutable binary resources, including their runtime-rendered Work/Attempt/Best/Integration/Follow-up fields.
- Install separate per-Role `instructions.md` user overlays that are empty by default and never overwrite user edits.
- Complete Context Builder projections for Baseline, Verification, Iteration, and Integration.
- Pika-owned Codex profile overlay with inline hooks and MCP configuration; inject each frozen complete prompt through Codex `developer_instructions` while preserving base config and Herdr hooks.
- Explicit MCP environment forwarding and automatic approval for only the Role grant's catalog; launch with non-interactive shell approval policy while retaining the configured Codex sandbox.
- Git metadata writes required by Baseline Draft and Iteration run through `commit_changes` in Pika's control plane; the coding Agent remains in `workspace-write` rather than using `danger-full-access`.
- Codex event normalization for session start/end, user prompt, tool completion, and stop.
- Provider-event deduplication, Turn correlation, full observable tool payload storage, and bounded journal selection for later sessions.
- Static per-Role Agent selection and explicit one-shot override when a fresh session is created.

### Exit gate

- Human review of each shipped immutable Role System Prompt, its dynamic fields, and terminal-operation wording; verify default user instructions are empty.
- Profile validation proves base hooks, Herdr hook, and Pika hooks coexist.
- A real Codex Baseline Draft and Verification reach their terminal MCP operations.
- At least one real Codex Iteration reaches Integration on a disposable tuning fixture.
- Daemon restart replaces the Codex session rather than resuming it, and the replacement receives the expected Context Bundle.
- Tool payload coverage is reported factually; unsupported provider internals are not described as captured.

Recorded real-model evidence for this phase uses an opt-in disposable fixture. It is excluded from ordinary `make verify` because it consumes model quota; `make real-codex-integration` is the explicit gate.

### Deliberately absent

Automatic Follow-up. A stopped incomplete Codex turn remains visible and can be steered manually.

## 11. Phase 7 — automatic Follow-up

### Outcome

Eligible incomplete Codex Work receives bounded, target-specific Follow-up messages after the configured best-effort pane-inactivity interval.

### Build

- Follow-up Request persistence, injected clock, eligibility, separate target/generator budgets, and generator retry.
- One Follow-up Agent Configuration with target-specific immutable System Prompts and empty-by-default instruction overlays for Baseline Verification, Iteration, and Integration.
- Default `5m` deadline and reset from `pane.updated`, stored as `observed_pane_activity` rather than human input.
- Fresh generator sessions, `submit_followup_message`, target revalidation, and `agent.prompt` delivery.
- Supersession without generator interruption, terminal races, `delivery_unknown`, and no blind duplicate prompt.
- Draining behavior that permits Follow-up for already active children but starts no new primary Work.

### Exit gate

- Fake-clock tests cover every Follow-up transition without waiting in wall-clock time.
- User prompt or observed pane activity resets/supersedes as designed.
- Activity during generation discards the eventual generated message without killing the generator.
- Target terminal completion wins every race with generation and delivery.
- A real Codex smoke uses a short test-only timeout; production default remains five minutes.
- Status output explicitly states that `pane.updated` is not strict human-input detection.

## 12. Phase 8 — recovery, security, and release

### Outcome

The macOS/Linux artifact survives realistic crashes, installs cleanly, and completes a small real Optimization without hidden manual repair.

### Build

- Exhaustive startup reconciliation and provider-session replacement.
- SQLite-safe backup/checkpoint procedure, migration failure behavior, corruption diagnostics, and disk-growth visibility.
- Socket/grant permissions, secret redaction, path traversal/symlink tests, and safe subprocess argv handling.
- Structured daemon logs, status/debug views, and protocol/version compatibility checks.
- Release archives, checksums, upgrade instructions, and Herdr plugin install/link documentation.

### Exit gate

- `gofmt`, `go vet ./...`, `go test -race ./...`, and supported cross-builds.
- Clean-machine plugin install smoke on macOS and Linux.
- Real disposable Optimization: init, Baseline Draft/Verification, parallel Iteration, Integration, Best update, Back-off, cancellation, Follow-up, and graceful shutdown.
- Repeat the smoke with daemon crashes at outbox and terminal-operation boundaries.
- Verify shutdown never kills an active Agent and may remain draining indefinitely.
- Verify final database invariants and no orphan Pika-owned processes or sockets.

Release only after this gate. Earlier phases are implementation milestones, not a production-ready product.

## 13. Phase 9 — optional provider expansion

Cursor and OpenCode adapters are post-first-release work:

1. Pin a CLI version and write a provider conformance fixture.
2. Prove session start/end, user prompt, assistant stop/message, tool input/output, and prompt delivery.
3. Implement the Provider Telemetry adapter without changing domain interfaces.
4. Enable complete journaling and Follow-up only for capabilities that actually pass.

A provider that fails conformance may still run as a directly steerable Herdr Agent. Pika must show the missing capabilities in status rather than silently degrading automation.

## 14. Commit and review cadence

Each phase should be reviewable and revertible on its own:

- Commit schema and code that consume it together; never leave a migration with no reader or writer.
- Include the phase's tests and smoke driver in the same change as the behavior.
- Record design changes in the existing ADRs or add a new ADR before changing an accepted invariant.
- Do not mix refactoring for the next phase into the current phase's acceptance diff.
- At every phase end, update the design status with delivered behavior, remaining gaps, and exact verification evidence.

The preferred first implementation target is Phase 0 followed by Phase 1. Phase 0 proves packaging and process mechanics cheaply; Phase 1 then creates the authoritative domain seam on which every later adapter depends.
