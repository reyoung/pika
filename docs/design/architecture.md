# Architecture

## 0. Feasibility verdict

Herdr's API is sufficient for the accepted first release, with two explicit qualifications:

- Herdr supplies the terminal runtime Pika needs: a visible long-running plugin pane, managed config/state directories, workspace/pane/Agent discovery, pane creation and closure, `agent.start`, `agent.prompt`, event subscriptions, snapshots, and workspace metadata.
- Herdr does not supply Pika's workflow, transactional completion, complete conversation journal, or a faithful human-input event. Pika provides those pieces through SQLite, Role-scoped MCP, and provider hooks. The accepted Follow-up timer uses `pane.updated` only as a documented best-effort approximation.

Pika therefore remains an executable plugin using the public CLI/socket surface. No Herdr fork, Rust linkage, terminal scraping protocol, or native plugin UI is required.

## 1. System boundary

Pika-Go is a local workflow daemon bound to a durable Optimization Workspace and displayed through a replaceable Herdr Workspace. It does not wrap the coding agent's terminal interaction: the user works directly in Herdr, while Pika observes, supplies MCP, persists facts, and schedules the next static Role.

```text
Optimization Workspace = one durable Optimization

┌─────────────────────────────────────────────────────────┐
│ workspace.json / pika.toml / pika.db                    │
│ repo/ / best/repo/ / attempts/.../repo                  │
│ contexts/ / evidence/ / logs/ / runtime/                │
│                                                         │
│ Replaceable Herdr Workspace                             │
│ daemon pane                                             │
│   pika-go daemon                                        │
│       │                                                 │
│       ├── SQLite                                        │
│       ├── Unix socket HTTP/JSON                         │
│       ├── Herdr socket subscription                    │
│       └── Work reconciler                               │
│                                                         │
│ Agent pane(s)                                           │
│   Codex / Cursor                                        │
│       ├── direct user interaction                       │
│       ├── provider hook ───────────────► daemon         │
│       └── pika-go mcp-proxy ───────────► daemon         │
│                                                         │
│ control shell pane                                      │
│   pika-go status | pause | resume | back-off | shutdown │
└─────────────────────────────────────────────────────────┘
```

The common interactive layout may show the current Agent above a control shell, but layout is presentation. Domain identity comes from `workspace.json` and SQLite, never a Herdr workspace, split, tab, or Pane ID.

## 2. Two authorities

| Concern | Authority | Never authoritative |
| --- | --- | --- |
| Optimization, Baseline, Attempt, Integration, Best | Pika SQLite domain transaction | Herdr state or terminal text |
| Work completion | Successful Role terminal MCP operation | Natural-language final answer, `idle`, `done` |
| Pane/process/session presence | Herdr snapshot and events | SQLite desired state alone |
| Conversation and tool journal | Provider hooks normalized by Pika | Provider transcript format alone |
| Git truth | Repository inspection plus accepted Pika Git intent | Agent claim |

The daemon reconciles these authorities; it never collapses them. For example, `idle` means that an Agent can receive input. If its Work lacks a terminal operation, Pika keeps the Work active and may schedule Follow-up.

## 3. Responsibilities

### Herdr

- Own workspace, tab, pane, terminal process, focus, and direct input.
- Start a supported Agent in an existing idle shell pane.
- Send prompts and keys, expose current Agent status, and publish runtime events.
- Report provider session references installed by Herdr integrations.
- Keep the daemon and Agent processes visible and manually operable.

### Pika daemon

- Own the Optimization state machine, scheduler, concurrency, and FIFO Integration queue.
- Create and close Agent panes through Herdr.
- Build immutable Role activation and complete, per-Session Context Bundles.
- Serve the CLI control protocol and Role-scoped MCP application.
- Normalize provider hooks into Agent Sessions, Turns, messages, and tool events.
- Generate Follow-up Requests and deliver their messages to the still-running target Agent.
- Persist every domain mutation and idempotency receipt.

### Coding agent

- Work inside its assigned Git workspace.
- Fully read the frozen local `context.json` and `messages.jsonl` described by the System Prompt schemas.
- Produce evidence and call exactly one valid terminal operation.
- Remain open for direct user input and automatic Follow-up until Work terminates.

### User

- Steer the live coding agent directly in its Herdr pane.
- Edit durable per-Role user instruction overlays with `$EDITOR`.
- Request an allowed Back-off, cancel one Work, inspect status, or request graceful shutdown from the control shell.

## 4. Deep module interfaces

The domain layer must not expose Pane IDs, Herdr JSON, hook payloads, or provider argv.

```go
type Symphony interface {
    Init(ctx context.Context, req InitRequest) (OptimizationView, error)
    Apply(ctx context.Context, command Command) (Receipt, error)
    Inspect(ctx context.Context, query Query) (View, error)
}

type WorkRuntime interface {
    Start(ctx context.Context, spec SessionSpec) (PaneBinding, error)
    Prompt(ctx context.Context, binding PaneBinding, message string) error
    Close(ctx context.Context, binding PaneBinding) error
    Snapshot(ctx context.Context) (RuntimeSnapshot, error)
    Events(ctx context.Context) (RuntimeEventStream, error)
}

type ProviderAdapter interface {
    Validate(configuration AgentConfiguration) error
    Probe(ctx context.Context, request ProbeRequest) (ProviderCapabilities, error)
    PrepareSession(ctx context.Context, activation SessionActivation) (ProviderLaunch, error)
    Normalize(ctx context.Context, binding SessionBinding, raw HookEvent) ([]JournalEvent, error)
}

type ToolApplication interface {
    Catalog(grant Grant) []Tool
    Invoke(ctx context.Context, grant Grant, call ToolCall) (ToolResult, error)
}
```

`Symphony.Apply` accepts a closed set of domain/control commands. The initial set is:

- initialize Optimization;
- Back-off with a user message;
- cancel Work;
- request graceful daemon shutdown;
- record a Role terminal operation;
- record provider telemetry;
- record Follow-up generation and delivery.

There is no dynamic `register_role`, `enqueue_work`, `steer_work`, or `append_guidance` command.

## 5. Process startup and initialization

### `pika-go kick-off`

The normal user entrypoint runs from an existing Herdr shell pane. It validates Herdr and its fresh-Session configuration before creating anything. It then discovers an existing `workspace.json` or creates a sibling `<repository>-pika-workspace` with a stable random ID and `repo/` base linked worktree. The Source Repository must be clean for normal creation and is never assigned to an Agent. The plugin pane is opened with the durable Workspace as cwd plus `PIKA_GO_WORKSPACE`; its PATH is prefixed with the directory of the invoking executable, so the manifest's `pika-go daemon` entrypoint cannot accidentally resolve relative to the optimized repository.

`kick-off` records the new replaceable Herdr workspace/tab/control-pane/daemon-pane IDs in `herdr/binding.json`, waits for daemon health, and sends a visible init command only when `pika.toml` is absent. Otherwise it recovers SQLite/outbox state. `pika-go open [WORKSPACE]` is the explicit equivalent; running `kick-off` inside a Workspace auto-opens it.

If daemon startup or initialization fails, the command reports the Workspace and daemon Pane IDs and leaves the visible Workspace open for diagnostics. It does not silently fall back to a non-persistent daemon or initialize the original Workspace.

### Daemon startup

The Herdr plugin opens `pika-go daemon` in a normal pane. A plugin startup hook is not used as a process supervisor.

The daemon:

1. Opens `PIKA_GO_WORKSPACE/workspace.json`, rejects relocation or source Git common-directory identity drift, and acquires `.pika.lock`.
2. Opens `pika.db`, validates `workspace_identity`, registers the base/Best/Attempt worktrees, and loads `pika.toml` plus `instructions/`.
3. Publishes `pika_instance=<workspace-id>` in the current Herdr Workspace metadata and constructs provider/runtime adapters rooted at `runtime/`.
4. Binds `/tmp/pika-go-$UID/<herdr-derived-instance>.sock` with user-only permissions. The socket is transport, not durable identity.
5. Retires every prior active Session that still owns non-terminal Work and dispatches ordered close/fresh-start recovery effects without resetting, cleaning, rebasing, or checking out an active worktree.
6. Subscribes to Herdr runtime events with a snapshot bootstrap and enters the steady-state scheduling loop.

### `pika-go init`

`init` runs in the shell pane that will become the Baseline Agent pane:

1. Discover the daemon socket through Herdr metadata; the daemon already owns the Optimization Workspace.
2. Validate the Source Repository identity and initialize `pika.toml`, SQLite domain state, empty instruction overlays, and provider overlays inside the Workspace.
3. Send the caller Pane ID to the daemon.
4. Exit, returning that pane to its shell prompt.
5. The daemon waits until Herdr reports an available shell and starts a fresh Baseline Agent in the same pane without injecting a User Turn. The operator supplies the Baseline target, case set, oracle, measurement protocol, and stop condition directly in that pane.

Initialization is atomic from the user's point of view. If any required step fails, Pika records diagnostics, removes partial instance bindings where safe, and exits the instance instead of leaving a half-configured Optimization.

The caller receives a non-zero exit, the daemon performs its failure cleanup and exits, and no Agent is left running. Before durable init, Pika also requires Herdr's active configuration to set `session.resume_agents_on_restore = false`, reloads that configuration through the socket API, and probes exactly the providers referenced by the five static Role configurations.

## 6. Normal scheduling

The Work projector is pure: it derives runnable Work from committed domain state. A reconciler claims runnable Work subject to static concurrency limits and performs runtime effects.

```text
Baseline Draft --terminal MCP--> Baseline Verification
Baseline Verification --accepted clean Repository Snapshot--> Iteration scheduling
Baseline Verification --rejected--> new Baseline Revision
Iteration Round --terminal MCP--> Integration FIFO
Integration --terminal MCP--> Best decision, then more Iteration
```

Normal stages auto-chain. There is no separate Web review gate. During Baseline Draft the user reviews and steers directly in the pane; submission of the Baseline Definition is the explicit handoff to independent verification.

Baseline acceptance deterministically seeds an ordered Iteration Case Set with `min(10, Full Case Set size)` Cases by SHA-256 of Case ID. Every Iteration Round freezes the current set before its Work is launched, so recovery and in-flight execution never observe later changes. A rejected Integration may rank all case-specific gate regressions; the same transaction appends at most three previously unseen Cases before future Work is projected. The Optimization-level set is versioned and monotonic, while Baseline Verification and Integration continue to require the complete Full Case Set.

Concurrency:

- Baseline Draft: 1
- Baseline Verification: 1
- Iteration: configurable `N`, one pane per active Work
- Integration: 1, strict FIFO
- Follow-up generation: one shared Agent Configuration; queue/concurrency is independently bounded

When a terminal MCP transaction succeeds, the daemon closes that Agent pane or returns the designated reusable pane to its shell, records the Agent Session end, and immediately projects the next Work.

## 7. Human control

### Scheduler Pause and resume

`pika-go pause` durably stops new Agent Session starts and Follow-up delivery/promotion, freezes waiting Follow-up clocks, and interrupts every active `working` or `blocked` Pika Session through its Provider Adapter. It preserves Work, Agent Session, and pane identity. `idle` or `done` Sessions are safe skips; unknown runtime state is reported as a failed delivery and receives no unsafe key sequence.

`pika-go resume` first sends the exact prompt `继续` to every still-active Session whose Work remains pending. Only after that bounded control cycle finishes does the daemon release held starts and Follow-up deliveries. Repeated pause or resume requests in the requested state are successful no-ops; they do not advance revision or resend control. A crash after delivery begins records `delivery_unknown` and recovery never blindly repeats that action.

The `SchedulerController` is the deep Module that serializes the durable transition, exact high-priority control effect, and ordinary runtime dispatcher behind one interface. Per-Session delivery is limited to 15 seconds with at most 32 concurrent actions; the complete command is bounded at 60 seconds. Partial delivery remains a committed Scheduler transition and is exposed through status and the command response.

### Direct steering

Pika does not proxy steering. The user types directly into the Codex/OpenCode/Cursor pane using Herdr. Provider hooks record submitted user messages where supported.

### Editing instructions

`pika-go edit-instruction <name>` opens the selected empty-by-default user overlay with `$EDITOR`. It cannot edit the binary-owned System Prompt. Changes affect newly created Agent Sessions; they do not rewrite a frozen activation already running.

### Agent activation

Before starting a Session, the daemon materializes its Context Bundle and renders three frozen layers: the binary-owned Role System Prompt; exact bundle paths, digests, and complete versioned JSON Schemas; and the non-empty user instruction overlay. Codex receives that value as `developer_instructions` through the instance wrapper. Cursor stores it in private per-Session state and returns it as `sessionStart.additional_context`; Cursor's frozen layer also bootstraps the environment-scoped `pika_go` MCP namespace. Cursor uses a positional initial prompt for automatically kicked-off Work. The first Baseline Draft deliberately has no initial User Turn for either provider; later Herdr prompt injection is reserved for operator/Follow-up messages and is not used to emulate a System Prompt.

### Back-off

`pika-go back-off -m '<message>'` applies the allowed earlier-phase transition for the current context. Back-off preserves prior revisions/results and creates new Work:

- Baseline Verification to a new Baseline Revision;
- Integration to a new Iteration Round of the same Attempt;
- other transitions only when explicitly declared by the state machine.

Every resulting execution uses a fresh Agent Session. The message becomes durable context for the newly created Work.

### Cancel Work

Cancellation applies to the whole Work, not an individual turn:

- Baseline Draft or Verification cancellation pauses the Optimization in that phase.
- Iteration cancellation rejects/cancels that Attempt without affecting siblings.
- Integration cancellation cancels that Attempt's integration path.

The daemon may close the Work's pane as part of explicit cancellation. Cancellation is not used for ordinary Back-off or daemon shutdown.

## 8. Follow-up lifecycle

Baseline Verification, Iteration, and Integration are Follow-up eligible. Baseline Draft is deliberately user-driven and has no automatic Follow-up.

After an eligible Agent turn stops without a terminal operation:

1. Persist the incomplete Turn and arm a configurable pane inactivity deadline; default `5m`.
2. Reset the deadline when a subscribed `pane.updated` event is associated with the target pane.
3. At the deadline, reserve one Follow-up Request unless the Work became terminal or exhausted its policy.
4. Start a fresh Follow-up Agent Session using the shared Follow-up Agent Configuration, target-specific System Prompt, dynamic target context, and matching user overlay.
5. The Follow-up Agent calls `submit_followup_message`.
6. If still current, the daemon marks the request `dispatching` and sends the message to the target Agent with Herdr `agent.prompt`; the resulting self-caused `pane.updated` and provider user-message echo do not supersede that dispatch.

If observed pane activity occurs while the generator is running, the request becomes `superseded_by_observed_activity`. Pika does not kill the generator; its eventual message is accepted for audit but discarded, and the inactivity deadline starts again.

### Deliberate approximation

Herdr currently has no subscription event that means “a human forwarded input to this pane.” The accepted MVP uses `pane.updated` as a best-effort proxy and does not modify Herdr.

Source inspection shows local key/text/paste forwarding and `pane.send_input` do not themselves emit `pane.updated`. Therefore unsubmitted typing may fail to reset the timer, while metadata/title changes may reset it without human input. This limitation must be visible in `pika-go status` and covered by a real acceptance test; the product does not claim strict five-minute human inactivity detection.

## 9. Recovery and reconciliation

An Optimization, Work, Agent Session, and Pane Binding are separate identities.

On daemon restart or runtime loss:

1. SQLite supplies desired non-terminal Work and immutable history.
2. Herdr snapshot supplies live panes, processes, statuses, and session references.
3. Every Agent Session left active by the previous daemon is retired; its grant is revoked and its explicit persisted Pane Binding is used only to close the Pika-owned old pane, never to resume the provider session.
4. The durable outbox orders that old-Session close before replacement start. A missing old pane makes close idempotently successful; a close failure blocks the fresh start rather than allowing two live owners of one Work.
5. Pika creates a fresh Agent Session with a newly frozen Recovery Context Bundle. Within one continuously running daemon, ordinary `pane.updated` snapshots may still update a moved pane's binding without replacing the Session.

Provider session IDs are retained in the journal but never used to resume. Recovery resolves the current Work's static Role configuration again and starts that configured provider; it never falls back to another provider. A Back-off may cross providers only because its destination Role has a different static configuration.

## 10. Graceful shutdown

`pika-go shutdown` is a draining operation:

1. Persist `draining` and reject new primary Work starts and Back-off requests.
2. Keep existing Agent Sessions and MCP endpoints alive.
3. Permit Follow-up needed to help an active Agent reach its terminal operation.
4. As each Agent calls its terminal MCP, perform the normal transition and normal pane close, but do not start another primary stage.
5. Remain reachable while any pending Work, undispatched runtime effect, or active Agent Session exists.
6. After all three sets are empty, flush state, remove the Unix socket, and exit automatically.

Shutdown does not send interrupt keys, cancel turns, or forcibly close child Agent panes. Consequently it may wait indefinitely for an Agent or user. Closing the daemon pane directly is a crash, not graceful shutdown; the next daemon start reconciles the remaining runtime.

Graceful shutdown is rejected while the Scheduler is paused, because a paused Scheduler cannot drain held starts or Follow-up deliveries. Scheduler pause/resume is rejected after the Optimization enters `draining`.

## 11. Technology and distribution

- Go single binary.
- Standard-library HTTP/JSON over Unix sockets for CLI, hooks, and MCP proxy-to-daemon traffic.
- Official Go MCP SDK for the stdio-facing MCP server.
- `database/sql` with a pure-Go SQLite driver, WAL, foreign keys, and explicit transactions.
- Herdr raw NDJSON socket client for subscriptions and capability checks; CLI wrappers remain useful for diagnostics.
- Herdr plugin config/state directories for durable files; plugin root is read-only package content.
- macOS and Linux first. Windows requires a transport and process-launch design rather than accidental partial support.

The minimum plugin package is one platform-specific `pika-go` executable plus this manifest shape:

```toml
id = "pika-go"
name = "Pika-Go"
version = "0.1.0"
min_herdr_version = "0.8.2"
description = "Long-running automatic optimization orchestrator"
platforms = ["linux", "macos"]

[[panes]]
id = "symphony"
title = "Daemon"
placement = "split"
command = ["./pika-go", "daemon"]
```

The daemon pane starts with the plugin root as its working directory; `pika-go kick-off` supplies the Optimization repository through `init`. The kick-off layout names its tab `Optimization`, its interactive root pane `Setup`, and the plugin pane `Daemon`. Once Work starts, Agent panes are renamed for their Role (`Baseline`, `Verify`, `Iteration R<n>`, `Integration`, or `Follow-up`); concurrent Iteration panes include their short Herdr pane ID, such as `Iteration R2 · p4`, and an unknown Role falls back to `p<n>`. A release archive contains the correct binary for its OS/architecture and the manifest. Local development uses `herdr plugin link`; release installation can use a platform-specific package or a Herdr plugin repository whose build step installs the matching binary.
