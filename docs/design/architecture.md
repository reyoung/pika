# Architecture

## 0. Feasibility verdict

Herdr's API is sufficient for the accepted first release, with two explicit qualifications:

- Herdr supplies the terminal runtime Pika needs: a visible long-running plugin pane, managed config/state directories, workspace/pane/Agent discovery, pane creation and closure, `agent.start`, `agent.prompt`, event subscriptions, snapshots, and workspace metadata.
- Herdr does not supply Pika's workflow, transactional completion, complete conversation journal, or a faithful human-input event. Pika provides those pieces through SQLite, Role-scoped MCP, and provider hooks. The accepted Follow-up timer uses `pane.updated` only as a documented best-effort approximation.

Pika therefore remains an executable plugin using the public CLI/socket surface. No Herdr fork, Rust linkage, terminal scraping protocol, or native plugin UI is required.

## 1. System boundary

Pika-Go is a local workflow daemon embedded in a Herdr Workspace. It does not wrap the coding agent's terminal interaction: the user works directly in Herdr, while Pika observes, supplies MCP, persists facts, and schedules the next static Role.

```text
Herdr Workspace = one Optimization

┌─────────────────────────────────────────────────────────┐
│ daemon pane                                             │
│   pika-go daemon                                        │
│       │                                                 │
│       ├── SQLite                                        │
│       ├── Unix socket HTTP/JSON                         │
│       ├── Herdr socket subscription                    │
│       └── Work reconciler                               │
│                                                         │
│ Agent pane(s)                                           │
│   Codex / OpenCode / Cursor                             │
│       ├── direct user interaction                       │
│       ├── provider hook ───────────────► daemon         │
│       └── pika-go mcp-proxy ───────────► daemon         │
│                                                         │
│ control shell pane                                      │
│   pika-go status | back-off | cancel-work | shutdown    │
└─────────────────────────────────────────────────────────┘
```

The common interactive layout may show the current Agent above a control shell, but layout is presentation. Domain identity never depends on a particular split or Pane ID.

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
- Build immutable Role activation and bounded Context Bundles.
- Serve the CLI control protocol and Role-scoped MCP application.
- Normalize provider hooks into Agent Sessions, Turns, messages, and tool events.
- Generate Follow-up Requests and deliver their messages to the still-running target Agent.
- Persist every domain mutation and idempotency receipt.

### Coding agent

- Work inside its assigned Git workspace.
- Read Pika context through MCP and referenced files.
- Produce evidence and call exactly one valid terminal operation.
- Remain open for direct user input and automatic Follow-up until Work terminates.

### User

- Steer the live coding agent directly in its Herdr pane.
- Edit durable Role instructions with `$EDITOR`.
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

type ProviderTelemetry interface {
    Normalize(ctx context.Context, binding PaneBinding, raw HookEvent) ([]JournalEvent, error)
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

### Daemon startup

The Herdr plugin opens `pika-go daemon` in a normal pane. A plugin startup hook is not used as a process supervisor.

The daemon:

1. Requires Herdr's plugin config/state paths and current Workspace context.
2. Opens or creates the instance SQLite database in the plugin state directory.
3. Reuses a valid `pika_instance` already bound to the Workspace, or chooses a new instance ID for an unbound Workspace, then listens at `/tmp/pika-go-$UID/<instance>.sock` with user-only permissions.
4. Publishes or refreshes `pika_instance=<instance>` in Herdr Workspace metadata.
5. Subscribes to Herdr runtime events, then reads a snapshot to close the bootstrap gap.
6. Reconciles persisted desired Work with live panes.
7. Accepts CLI, hook, and MCP proxy traffic.

### `pika-go init`

`init` runs in the shell pane that will become the Baseline Agent pane:

1. Discover the Workspace and daemon socket through Herdr metadata.
2. Validate the repository and initialize Pika configuration, SQLite domain state, Git workspaces, instructions, and provider overlays.
3. Send the caller Pane ID to the daemon.
4. Exit, returning that pane to its shell prompt.
5. The daemon waits until Herdr reports an available shell and starts a fresh Baseline Agent in the same pane.

Initialization is atomic from the user's point of view. If any required step fails, Pika records diagnostics, removes partial instance bindings where safe, and exits the instance instead of leaving a half-configured Optimization.

The caller receives a non-zero exit, the daemon performs its failure cleanup and exits, and no Agent is left running.

## 6. Normal scheduling

The Work projector is pure: it derives runnable Work from committed domain state. A reconciler claims runnable Work subject to static concurrency limits and performs runtime effects.

```text
Baseline Draft --terminal MCP--> Baseline Verification
Baseline Verification --accepted--> Iteration scheduling
Baseline Verification --rejected--> new Baseline Revision
Iteration Round --terminal MCP--> Integration FIFO
Integration --terminal MCP--> Best decision, then more Iteration
```

Normal stages auto-chain. There is no separate Web review gate. During Baseline Draft the user reviews and steers directly in the pane; submission of the Baseline Definition is the explicit handoff to independent verification.

Concurrency:

- Baseline Draft: 1
- Baseline Verification: 1
- Iteration: configurable `N`, one pane per active Work
- Integration: 1, strict FIFO
- Follow-up generation: one shared Agent Configuration; queue/concurrency is independently bounded

When a terminal MCP transaction succeeds, the daemon closes that Agent pane or returns the designated reusable pane to its shell, records the Agent Session end, and immediately projects the next Work.

## 7. Human control

### Direct steering

Pika does not proxy steering. The user types directly into the Codex/OpenCode/Cursor pane using Herdr. Provider hooks record submitted user messages where supported.

### Editing instructions

`pika-go edit-instruction <name>` opens the selected Markdown file with `$EDITOR`. Changes affect newly created Agent Sessions; they do not rewrite a frozen activation already running.

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
4. Start a fresh Follow-up Agent Session using the shared Follow-up Agent Configuration and the target-specific Instruction Profile.
5. The Follow-up Agent calls `submit_followup_message`.
6. If still current, the daemon sends the message to the target Agent with Herdr `agent.prompt`.

If observed pane activity occurs while the generator is running, the request becomes `superseded_by_observed_activity`. Pika does not kill the generator; its eventual message is accepted for audit but discarded, and the inactivity deadline starts again.

### Deliberate approximation

Herdr currently has no subscription event that means “a human forwarded input to this pane.” The accepted MVP uses `pane.updated` as a best-effort proxy and does not modify Herdr.

Source inspection shows local key/text/paste forwarding and `pane.send_input` do not themselves emit `pane.updated`. Therefore unsubmitted typing may fail to reset the timer, while metadata/title changes may reset it without human input. This limitation must be visible in `pika-go status` and covered by a real acceptance test; the product does not claim strict five-minute human inactivity detection.

## 9. Recovery and reconciliation

An Optimization, Work, Agent Session, and Pane Binding are separate identities.

On daemon restart or runtime loss:

1. SQLite supplies desired non-terminal Work and immutable history.
2. Herdr snapshot supplies live panes, processes, statuses, and session references.
3. Pika matches only explicit persisted Pane Bindings; a reused name or Pane ID is insufficient.
4. An incompatible, missing, or abandoned binding is closed or marked lost according to ownership.
5. Pika creates a fresh Agent Session with a Recovery Context Bundle.

Provider session IDs are retained in the journal but never used to resume. Switching from Codex to another configured coding agent during recovery is allowed if the target Role's required capability level is satisfied.

## 10. Graceful shutdown

`pika-go shutdown` is a draining operation:

1. Persist `draining` and reject new primary Work starts and Back-off requests.
2. Keep existing Agent Sessions and MCP endpoints alive.
3. Permit Follow-up needed to help an active Agent reach its terminal operation.
4. As each Agent calls its terminal MCP, perform the normal transition and normal pane close, but do not start another primary stage.
5. Exit only after all child Agents have exited through their normal lifecycle and state is flushed.

Shutdown does not send interrupt keys, cancel turns, or forcibly close child Agent panes. Consequently it may wait indefinitely for an Agent or user. Closing the daemon pane directly is a crash, not graceful shutdown; the next daemon start reconciles the remaining runtime.

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
title = "Pika-Go Symphony"
placement = "split"
command = ["./pika-go", "daemon"]
```

The daemon pane starts with the plugin root as its working directory; `pika-go init` later supplies the Optimization repository. A release archive contains the correct binary for its OS/architecture and the manifest. Local development uses `herdr plugin link`; release installation can use a platform-specific package or a Herdr plugin repository whose build step installs the matching binary.
