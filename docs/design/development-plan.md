# Incremental Development Plan

## 1. Delivery strategy

Pika-Go should be built as a sequence of executable vertical slices. Each phase ends with an observable workflow that crosses the interfaces introduced in that phase. A phase is not complete merely because packages compile or isolated mocks pass.

The ordering follows four rules:

1. Make domain transitions deterministic before involving a coding agent.
2. Prove the Herdr runtime with a scripted fake Agent before involving Codex.
3. Complete the Baseline Draft/Verification loop before implementing concurrent Attempts.
4. Add automatic Follow-up only after conversation and turn telemetry is trustworthy.

The first release formally supports Codex. Later providers cross the same Provider Adapter seam one at a time and become supported only after their complete journal, automatic Follow-up, fresh-Session recovery, and real workflow matrix pass conformance. Phase 9 makes Cursor the next fully supported provider; OpenCode remains a separate later phase.

### Implementation checkpoint — 2026-08-28

| Phase | Implementation state | Current evidence / remaining release gate |
| --- | --- | --- |
| 0–4 | Functional slices implemented; local crash gates pass | Domain, Unix control plane, metadata-based instance discovery, real Herdr runtime, Baseline, idempotent receipts, and fresh-Session recovery pass. Baseline submission separately freezes Definition identity and a clean Repository Snapshot SHA; acceptance rejects committed or uncommitted repository drift. Domain `before_commit`/`after_commit` injection and real daemon crashes at running Session, dispatching start outbox, and committed terminal receipt all recover without duplicate Work or Agent. |
| 5 | Implemented; local workflow and crash gates pass | Concurrent Attempt/FIFO Integration, Git intent, stale/back-off, cancellation, and scheduler-config propagation tests pass. Existing user configuration is preserved and its scheduler values are frozen into the Optimization at init. A failed Git pre-commit retries the same staged intent, a response-loss retry returns the existing Best commit, and `finish_integration` before/after database-commit failures produce exactly one Best transition. A real daemon crash after the Best Git commit but before Domain finish recovers through a fresh Integration Session, reuses the pending intent, and records the same SHA exactly once. |
| 6 | Implemented; real-model core path passes | Immutable embedded Role System Prompts, frozen per-Session Context Bundle contracts, empty user overlays, per-Session prompt freezing, Codex managed profile/hooks, and full observable journal payload storage pass local tests. A disposable real-Codex Optimization completed Draft, Verification, Iteration, Integration, scoped Git commits and Best application, and graceful drain. |
| 7 | Implemented; real-model smoke passes | Fake-clock budget/race/exhaustion tests pass. Real Herdr with deterministic Agents passes Follow-up generation and delivery. A production-equivalent real-Codex short-timeout smoke generated and delivered a target-specific Follow-up to a still-running Verification Work, then completed the Optimization. |
| 8 | Release gate accepted | Online validated backup, storage growth view, migration/integrity rejection, permissions, structured daemon logs, protocol compatibility, cross-build archives/checksums, isolated archive-link smoke, real-Herdr graceful drain, and process crash recovery pass. Running Session, dispatching outbox, committed terminal receipt, and post-Git/pre-Domain Integration crash scenarios pass two consecutive real-process rounds. The real-Codex matrix passes Follow-up, fresh-Session daemon recovery, parallel sibling-isolated cancellation, Integration Back-off, Best advancement, and graceful drain. GitHub Actions run `33087182005` passed all five Linux/macOS verify, cross-build, and clean-install jobs, including compilation of pinned Herdr and native archive `plugin link`/`plugin list`. |
| 9 | Release gate accepted | Codex and Cursor share the Provider Adapter registry. Provider-specific init/preflight, additive Cursor MCP/Hook installation, per-Session frozen prompt bootstrap, Hook normalization, full raw and Shell/MCP payload retention, provider-scoped identities, fresh-Session recovery, cleanup, status metadata, and mirror-image mixed-role lifecycle matrices pass local tests. The pinned Cursor version passes the all-Cursor matrix, and both alternating Codex/Cursor assignments pass the credentialed mixed-provider matrix. |

The checkpoint distinguishes code present in the tree from release acceptance. It must be updated with command output or a concrete blocker; starting a test or compiling one package is not completion evidence.

Local verification recorded on 2026-08-27 and 2026-08-28:

- `make verify` passed formatting, `go vet ./...`, and `go test -race ./...` for every package.
- `make herdr-integration` passed one real-Herdr runtime conformance test and ten real-Herdr workflow tests, including graceful drain, FIFO Integration, and the four daemon process-crash boundaries listed above.
- `make crash-integration` passed two consecutive rounds of all four real daemon process-crash scenarios (eight crash/recovery executions).
- `make real-codex-integration` passed the complete disposable real-Codex release matrix. A dedicated Follow-up Session delivered guidance to a still-running Verification Work; a daemon process crash replaced that target with a fresh Pika Session and a distinct provider session; Baseline Revision 1 was accepted; two Iteration Sessions ran concurrently; cancelling one Attempt left its sibling pending; the first Integration backed off to a new Round with its message preserved; and a later Candidate advanced `pika/best` from sequence 0 to 1. The run drained normally with 12 distinct provider sessions and 106 observable tool events retained. Baseline Draft and Iteration committed through grant-scoped `commit_changes` while coding Agents remained in `workspace-write`.
- `make dist VERSION=0.1.0-dev` built Darwin/Linux archives for `amd64` and `arm64`; `shasum -a 256 -c SHA256SUMS` verified all four archives.
- The extracted native Darwin/arm64 archive reported version `0.1.0-dev` and passed isolated `herdr plugin link`/`plugin list` with disposable XDG directories.
- GitHub Actions run `33087182005` passed `make verify` on Ubuntu and macOS, supported cross-builds, and clean-machine install/link smoke on both platforms. The clean-install jobs provisioned Zig 0.15, compiled the pinned Herdr source, extracted the native Pika archive, and passed isolated `plugin link`/`plugin list`. Together with the real-Codex and crash matrices above, all first-release exit gates are satisfied.
- Phase 9 local verification passes `go test ./...`. Cursor fixtures cover exact candidate `2026.08.25-3e8eec8`, authentication/version failure, every normalized Hook category, duplicate/reordered terminal events, failed/interrupted Tools, full Shell/MCP supplements, a payload larger than 16 MiB, strict JSON Schema arrays, additive global integration with exact rollback, private prompt snapshots/cleanup, safe argv, init rollback, and fresh-session enforcement.
- Both deterministic static-role matrices pass through the production Provider Adapter and activation seams: Cursor/Codex/Cursor/Codex/Cursor and Codex/Cursor/Codex/Cursor/Codex. They cover both cross-provider Follow-up directions, simultaneous Iterations, cancellation isolation, Integration Back-off across providers, recovery without fallback, provider-route/grant/journal isolation, and graceful drain.
- `make real-cursor-integration` passed against Cursor `2026.08.25-3e8eec8` in 328.86 seconds. The all-Cursor Optimization delivered Follow-up to a pending Verification Work, replaced that Work with a fresh provider Session after daemon crash, isolated cancellation between parallel Iterations, backed off Integration, advanced Best to sequence 1, and drained normally with 11 provider Sessions and 79 observable Tool events.
- `make real-mixed-provider-integration` passed both alternating assignments in 753.70 seconds. Cursor/Codex/Cursor/Codex/Cursor completed in 323.36 seconds with 10 provider Sessions and 63 observable Tool events; Codex/Cursor/Codex/Cursor/Codex completed in 430.34 seconds with 11 provider Sessions and 67 observable Tool events. Together they prove both cross-provider Follow-up directions, configured-provider fresh recovery, cancellation isolation, Back-off across providers, Best advancement, and graceful drain without fallback.
- Herdr native Agent restore is enabled by default and can run `cursor-agent --resume <id>` or `codex resume <id>`. Phase 9 now rejects init unless `resume_agents_on_restore = false` is explicit and reloaded successfully; both provider wrappers also reject resume argv.

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
    P8 -. post-release .-> P9[9 Cursor parity]
    P9 -. later .-> P10[10 OpenCode]
```

## 2. Module seams

The implementation should keep the external interfaces from [Architecture](architecture.md) small and put complexity behind them.

| Module | Interface | Hidden implementation | First exercised |
| --- | --- | --- | --- |
| Domain Engine | `Apply(Command) -> Receipt`, `Inspect(Query) -> View` | validation, SQLite transaction, idempotency, events, outbox, lifecycle invariants | Phase 1 |
| Work Projector | internal `Project(State) -> DesiredWork` | static Role scheduling, concurrency, FIFO Integration, draining rules | Phase 1, extended Phase 5 |
| Work Runtime | `Start`, `Prompt`, `Close`, `Snapshot`, `Events` | Herdr NDJSON, pane identity, subscriptions, snapshot/reconnect, launch semantics | Phase 3 |
| Tool Application | `Catalog(Grant)`, `Invoke(Grant, Call)` | MCP authorization, schema validation, file contracts, terminal transactions | Phase 4 |
| Context Builder | `Materialize(AgentSession) -> ContextBundle` | complete domain projection and normalized Work journal, atomic read-only files, digests, schemas | Phase 4, extended Phase 6 |
| Git Workspace | `CommitChanges`, `CreateAttempt`, `RefreshFromBest`, `PrepareBestUpdate`, `ApplyAuthorizedBestUpdate`, `VerifyBestUpdate` | scoped commits, worktrees, merge policy, idempotent Git intent/postconditions, digests | Phase 5 |
| Provider Adapter | `Validate`, `Probe`, `PrepareSession`, `Normalize` | Provider configuration and capabilities, version checks, launch materialization, ephemeral resources, wire fields, deduplication, Session/Turn correlation, tool payloads | Phase 6, deepened Phase 9 |
| Follow-up Coordinator | internal state transition interface | clocks, eligibility, budget, supersession, generator sessions, delivery uncertainty | Phase 7 |

Do not introduce a generic repository interface over SQLite in the first release. SQLite is the only domain-state adapter. The useful seam is the Domain Engine interface, not one CRUD method per table.

`WorkRuntime` and the Provider Adapter are real seams because they have multiple adapters: scripted/in-memory versus Herdr, and Codex versus Cursor. The Provider Adapter must remain a deep Module: callers select a provider and receive a validated launch plus normalized journal events without learning its CLI, profile, Plugin, Hook, version, or cleanup rules. Time should be injected into Follow-up logic because a fake clock materially changes testability.

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
- Per-Session `context.json` and `messages.jsonl`, grant-scoped `commit_changes`, `submit_baseline_definition`, and `finish_baseline_verification`.
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

## 13. Phase 9 — Cursor parity

### Outcome

Any static Role may select Codex or Cursor while preserving the same Pika lifecycle: immutable frozen System Prompt, provider-scoped MCP, complete Conversation Journal, direct Herdr steering, automatic Follow-up, Work cancellation, Back-off, fresh recovery Sessions, and graceful drain. One Optimization may assign different providers to different Roles; every Session for one Role still uses that Role's single static Agent Configuration. Phase 9 does not introduce per-Session provider selection or mixed providers among sibling Iteration Sessions. Merely launching `cursor-agent` is not Cursor support.

Cursor support is pinned to the real-gate-tested CLI version `2026.08.25-3e8eec8`. A mismatch is a hard preflight failure, not a warning or silent capability downgrade.

### Provider Adapter seam

Replace the Codex-only telemetry path with a deep Provider Adapter Module. Its Interface has four operations:

- `Validate(AgentConfiguration)`: validate provider-specific model, reasoning effort, and launch arguments.
- `Probe(Context) -> ProviderCapabilities`: resolve the executable, authenticate, check the exact allowlisted version, and report journaling, turn-stop, Follow-up, full-output, and fresh-Session capabilities.
- `PrepareSession(SessionActivation) -> ProviderLaunch`: materialize provider-owned ephemeral resources and return executable, argv, environment, and cleanup ownership.
- `Normalize(SessionBinding, RawProviderEvent) -> JournalEvents`: preserve the raw event and produce provider-neutral Session, Turn, Message, Tool, and lifecycle events.

Move Codex behind this Interface without changing its behavior before adding the Cursor Adapter. Initialization, Work activation, Hook ingestion, status, and recovery select an Adapter from one registry; the Domain Engine never switches on Cursor event names or launch flags. Record the actual provider kind, version, and capabilities with each Agent Session. Codex and Cursor wrappers, Hooks, profiles/Plugins, provider session identities, and MCP grants must coexist within one instance without shared mutable launch state. Reject a raw provider event when its route provider does not match the bound Agent Session; provider session IDs are namespaced by provider and may not bind or deduplicate across adapters.

### Configuration and init

Cursor retains the existing `model + reasoning_effort` configuration shape for pinned models:

```toml
[agents.baseline]
kind = "cursor"
model = "gpt-5.6-sol"
reasoning_effort = "high"
args = ["--force", "--approve-mcps", "--trust"]
```

The Cursor Adapter passes Cursor's pinned-model ID shape by appending effort, for example `gpt-5.6-sol-high`. It also accepts the catalog's real `auto` ID with an empty effort, shown by init as `auto-routing (default)` and passed to Cursor as `--model auto`. Codex exposes a separate `default` entry represented by empty model and effort strings, so it does not override the Codex CLI configuration. Cursor accepts `low`, `medium`, `high`, `xhigh`, and `max`; `ultra` remains Codex-only. `args` is an optional Cursor-only argv array, not a shell string; configuration version remains `1` because the field is additive and an older binary already rejects the unknown provider. It may configure any non-reserved option supported by the pinned Cursor version, including `--force`, `--auto-review`, `--approve-mcps`, `--trust`, and `--sandbox enabled`. Pika parses option arity before launch and rejects `--`, a top-level command or initial prompt, `--resume`, `--continue`, `--print`, workspace/plugin/model/worktree overrides, and `--sandbox disabled`. Without an explicit sandbox policy Pika supplies `--yolo`; explicit `--sandbox enabled` retains the restricted sandbox. Interactive init never asks for raw argv: numbered command-approval, MCP-approval, and workspace-trust choices produce the supported flags, defaulting to `["--force", "--approve-mcps"]` without `--trust`. Advanced argv remains available through `--config`.

For a new instance, an interactive `pika-go init` asks separately for the backend, model, reasoning effort, and Cursor launch permissions of Baseline, Baseline Verification, Iteration, Integration, and Follow-up. Every input uses numbered lists; backend choices contain only successfully probed providers and effort choices are model-specific. Arbitrary provider or model IDs are not accepted. Every provider catalog starts with an explicit provider-default entry; changing backend selects that entry by default, while retaining the same backend preserves the Role's configured model when it remains available. Models such as Cursor auto-routing and Codex default that delegate effort selection to the provider skip the effort prompt. An existing `config.toml` remains byte-for-byte user-owned input and skips these questions.

Add `GET /v1/init/options`, returning `configuration_exists` and the available Provider kinds, executables, versions, compatibility, authentication state, capabilities, and selectable model/effort pairs without writing state. When configuration is absent, `POST /v1/init` requires `configuration_toml`, containing the complete candidate rendered by the interactive CLI, read from `--config PATH`, or generated by `--defaults`. When configuration exists, sending candidate TOML is an error rather than an overwrite. Non-TTY and `--json` init must use exactly one of `--config PATH` or `--defaults`; there is no implicit provider choice. `--config` must describe the same canonical repository supplied to init, while `--defaults` generates the current all-Codex configuration.

Init probes exactly the distinct providers referenced by the five static Agent Configurations. Codex-only configuration neither resolves nor authenticates Cursor, and Cursor-only configuration does not require Codex. A mixed configuration probes both adapters before creating durable Optimization state; if either adapter is absent, unauthenticated, incompatible, or missing a required capability, the entire init fails and rolls back. Runtime launch and recovery never fall back from the configured provider to the other adapter.

Before committing init, require the active Herdr configuration to contain:

```toml
[session]
resume_agents_on_restore = false
```

Herdr can otherwise restore official Cursor and Codex sessions natively, contradicting Pika's rule that recovery always creates a new Agent Session. `pika-go kick-off` resolves and validates the active Herdr config before creating a Workspace. If the setting is absent, enabled, or invalid, it asks permission to atomically set it to false and reload the running Herdr server; refusal is read-only, and reload failure restores the original file. Direct `pika-go init` remains read-only and rejects the configuration with exact instructions. Both paths require `server.reload_config` to return `applied` before durable init, and both provider wrappers reject native resume argv as defense in depth. See [Herdr integrations](https://herdr.dev/docs/integrations/) and [Herdr socket API](https://herdr.dev/docs/socket-api/).

### Cursor integration and per-Session prompt state

Cursor CLI does not reliably load every local Plugin component through `--plugin-dir`, so init installs two small additive entries in the authenticated user's existing Cursor configuration:

- `~/.cursor/mcp.json` gets one reserved `pika_go` stdio server. Its command and socket/grant/Session environment use Cursor `${env:NAME}` interpolation, so concurrent Sessions never rewrite shared configuration.
- `~/.cursor/hooks.json` gets event-specific commands of the form `"$PIKA_GO_EXECUTABLE" hook cursor <event>`. Existing Cursor and Herdr hooks are preserved in order; a conflicting reserved entry fails init; any later init failure restores the exact original bytes.

`PrepareSession` creates private Session state beneath instance runtime: a `0600` frozen System Prompt and validated argv snapshot in a `0700` directory. It also uses the user-only Context Bundle beneath `contexts/<session-id>/`. The `sessionStart` hook returns the prompt as `additional_context`; the frozen provider bootstrap tells Cursor to fully read both schema-described context files and load the `pika_go` namespace through `GetDynamicTools` before calling Role MCP tools. This bootstrap is binary-owned, not editable user Instructions. Cursor retains the real authenticated `HOME`; Pika never copies Cursor credentials or creates a competing config home.

The wrapper launches the interactive TUI with validated user args followed by `--workspace <repository> --model <parameterized-model> <initial-prompt>`. It defaults to `--yolo` unless the Role args explicitly select `--sandbox enabled`. Supplying the initial prompt positionally avoids a startup `agent.prompt` race. It never uses `--print`, `--resume`, or `--continue`. For Cursor, Pika accepts Herdr's successful `agent.start` response without synchronously waiting for `interactive_ready`; the positional prompt may already be executing, and blocking until an idle prompt would stall the outbox. Its grant is valid while the Session is `starting` or `running`, and launch binding promotes it to `running`. Later Follow-up and human steering still use Herdr `agent.prompt`. Cleanup removes only private Session state after the child exits, and startup removes stale Session directories not referenced by an active Session.

`beforeMCPExecution` is fail-closed and allows only Pika's MCP server. The existing Role grant remains the authority for individual tool names. Telemetry Hooks remain fail-open, short-running, and silent except for the valid JSON response required by Cursor.

### Cursor journal and Follow-up

`pika-go hook cursor` accepts the official event set documented in [Cursor Hooks](https://cursor.com/docs/hooks):

- `sessionStart`: bind `session_id`/`conversation_id` to the Pika Agent Session.
- `beforeSubmitPrompt`: open the Turn identified by `generation_id` and store the user message.
- `afterAgentResponse`: store the complete assistant message.
- `postToolUse` and `postToolUseFailure`: store tool input, output or failure, duration, and interrupt state.
- `afterShellExecution` and `afterMCPExecution`: retain full terminal output and full MCP JSON result as supplemental evidence for the logical Tool call, not a second Tool call.
- `stop`: close the current Turn with `completed`, `aborted`, or `error` and arm Pika Follow-up eligibility.
- `sessionEnd`: record provider-conversation termination independently of Turn completion.

Store every raw Cursor payload in SQLite before normalization. Deduplicate by provider, Pika Session, event identity/digest, conversation, generation, and tool-use identity where available. Specialized Shell/MCP events enrich or supplement the matching logical Tool event; an unmatched supplemental event is retained rather than discarded.

Remove the current mismatched one-MiB daemon and 16-MiB Hook limits for the provider-event route. The local permission-protected Unix-socket endpoint reads and persists the complete provider-emitted JSON payload; unrelated control-plane and MCP limits remain unchanged. Existing storage-growth status remains the warning surface for large SQLite journals.

Cursor's synchronous stop-Hook `followup_message` is never used. `stop` only begins Pika's configurable inactivity deadline; pane activity or a user prompt resets/supersedes it exactly as for Codex. A dedicated Follow-up Session generates guidance asynchronously, and Herdr `agent.prompt` delivers it to the still-active target Session. The request enters `dispatching` first, so Pika's own pane activity and provider-reported user-message echo cannot supersede the delivery. Pika continues to cancel Work rather than individual Turns.

### Build slices

1. Introduce the Provider Adapter registry and migrate Codex with no observable behavior change.
2. Add configuration/init contracts, reserved-argv validation, Herdr native-resume preflight, provider version recording, and Cursor event fixtures.
3. Add Cursor launch materialization, additive MCP/Hook installation, private frozen-prompt state, dynamic namespace bootstrap, MCP policy, wrapper defenses, cleanup, and normalized journaling.
4. Prove cross-Role Codex/Cursor workflows through the same Provider Adapter registry: atomic dual-provider preflight, wrapper/Hook/grant isolation, both Follow-up directions, Back-off across providers, configured-provider recovery, and no fallback.
5. Add the opt-in all-Cursor and mixed-provider real matrices; update Architecture, Protocols, Configuration, and the fresh-Session ADR before declaring the phase complete.

### Exit gate

Normal CI must pass:

- Existing Codex Adapter, real-Herdr, crash, packaging, and configuration tests without semantic regression.
- Cursor unit and fixture tests for every Hook event, duplicate and reordered delivery, failed/interrupted Tools, separate `stop`/`sessionEnd`, model/effort validation, reserved argv, version rejection, and authentication failure.
- A payload larger than 16 MiB reaches SQLite byte-for-byte, while other endpoint limits remain enforced.
- Global integration snapshots prove preservation/conflict detection/exact rollback; Session snapshots prove the complete frozen System Prompt, environment-scoped MCP grant, file permissions, shell-safe argv, provider-specific launch-readiness policy, and cleanup/reconciliation behavior.
- Two mirror-image fake-Herdr configurations alternate Codex and Cursor across Roles: Cursor/Codex/Cursor/Codex/Cursor and Codex/Cursor/Codex/Cursor/Codex for Baseline/Baseline Verification/Iteration/Integration/Follow-up respectively. They prove direct steering, both cross-provider Follow-up directions, wrapper/Hook/MCP-grant and Journal isolation, provider-route mismatch rejection, sibling-isolated cancellation, Back-off across providers, configured-provider fresh recovery without fallback, and graceful drain.
- Codex-only init succeeds without Cursor, Cursor-only init succeeds without Codex, and mixed init rolls back atomically when either referenced adapter fails preflight.
- Interactive init, existing-config preservation, `--config`, `--defaults`, non-TTY, `--json`, and Herdr native-resume failures are deterministic.

`make real-cursor-integration` is explicit and credentialed rather than part of ordinary CI. Against the exact candidate CLI it must prove:

- `sessionStart.additional_context` reaches the initial system context exactly once, including immutable Role Prompt, provider bootstrap, dynamic context, and frozen user Instructions.
- Session, Turn, user, assistant, successful/failed Tool, full Shell/MCP output, stop, and end Hooks are all observed and correlated.
- Pika MCP starts without a hidden approval, non-Pika MCP execution is denied, user steering remains interactive, and Pika asynchronous Follow-up reaches the intended stopped Turn.
- A disposable Draft -> Verification -> parallel Iteration -> Integration Back-off -> Best workflow passes with sibling-isolated cancellation and graceful drain.
- Daemon recovery replaces every running child with a fresh Pika Session and distinct Cursor conversation; native resume argv is rejected.
- Final SQLite, Git, process, pane, socket, global-config rollback, and ephemeral Session-state invariants match the real-Codex release matrix.

`make real-mixed-provider-integration` is a separate explicit, credentialed gate. It uses an independent disposable instance for each scenario:

1. A complete alternating Optimization configures Baseline=`cursor`, Baseline Verification=`codex`, Iteration=`cursor`, Integration=`codex`, and Follow-up=`cursor`. The first Codex Verification Turn stops incomplete and receives a Cursor-generated Follow-up before accepting the Baseline. Two Cursor Iteration Sessions then run concurrently; the test cancels one sibling, crashes the daemon while the other is running, and verifies a fresh Cursor recovery Session. The first Codex Integration backs off to a new Cursor Iteration Round, and a later Codex Integration advances Best before graceful drain.
2. A shorter reverse-direction scenario configures Baseline=`codex`, Baseline Verification=`cursor`, and Follow-up=`codex`. It waits for the Cursor Verification Turn to stop, delivers a Codex-generated Follow-up, completes Verification, and drains normally.

Both mixed scenarios assert the configured provider kind and version, distinct fresh conversation and Turn identities, the correct provider-specific frozen-prompt transport, and one provider-neutral Journal ordered by Work and Session. Provider events or MCP grants from one Session must not bind to the other adapter even if provider-native IDs collide. Back-off and daemon recovery must select the destination Role's configured provider with no fallback. Final SQLite, Git, pane, process, socket, profile/global-Cursor-integration, private Session-state, and grant invariants must match the single-provider matrices.

Both `make real-cursor-integration` and `make real-mixed-provider-integration` passed on 2026-08-28. The exact CLI version is therefore the supported allowlist entry, and status reports its full Cursor capabilities; any other version remains rejected rather than started in a partially automated mode.

## 14. Phase 10 — OpenCode evaluation

OpenCode remains independent post-Cursor work. Reuse the Provider Adapter Interface and the Phase 9 conformance shape, but make no compatibility claim or implementation commitment until its real CLI lifecycle, prompt transport, Tool output, and fresh-Session behavior have been researched and accepted separately.

## 15. Commit and review cadence

Each phase should be reviewable and revertible on its own:

- Commit schema and code that consume it together; never leave a migration with no reader or writer.
- Include the phase's tests and smoke driver in the same change as the behavior.
- Record design changes in the existing ADRs or add a new ADR before changing an accepted invariant.
- Do not mix refactoring for the next phase into the current phase's acceptance diff.
- At every phase end, update the design status with delivered behavior, remaining gaps, and exact verification evidence.

All five Phase 9 build slices and both credentialed real-provider gates pass. Cursor `2026.08.25-3e8eec8` is release-supported alongside Codex, including the two accepted static mixed-role assignments described above.
