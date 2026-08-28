# Configuration

## 1. Ownership and paths

Normal operation uses one first-class Optimization Workspace, independent of Herdr's global plugin config/state roots:

```text
<repository>-pika-workspace/
  workspace.json
  pika.toml
  pika.db
  .pika.lock
  repo/
  best/repo/
  attempts/<attempt>/rounds/<round>/repo/
  instructions/
    baseline.md
    baseline-verify.md
    iteration.md
    integration.md
    follow-up/
      baseline-verify.md
      iteration.md
      integration.md
  contexts/
  evidence/
  logs/
  runtime/cursor-sessions/<agent-session>/
  herdr/binding.json
```

`workspace.json` is the immutable bootstrap identity. It records the absolute Workspace root, Source Repository, Git common-directory device/inode, initial SHA, and random 128-bit Workspace ID. The Source Repository remains user-owned; Pika assigns Agents only to linked worktrees beneath the Workspace. Branches are namespaced as `pika/<workspace-id>/base`, `/best`, and `/attempt/<attempt>/<round>`, allowing multiple Workspaces for one source Git repository.

`pika.toml` and instruction overlays are user-owned mutable inputs. Canonical Role System Prompts remain embedded in the binary. Herdr's plugin paths contain only global plugin registration/integration material and legacy pre-Workspace instances; changing Herdr workspace/tab IDs never changes Optimization identity. `herdr/binding.json` records the current replaceable terminal layout.

## 2. Example configuration

```toml
version = 1

[optimization]
repository = "/absolute/path/to/repo"

[scheduler]
iteration_concurrency = 4
max_pending_attempts = 8

[context.iteration]
history_limit = 20

[agents.baseline]
kind = "codex"
model = "gpt-5.6-sol"
reasoning_effort = "high"

[agents.baseline_verify]
kind = "codex"
model = "gpt-5.6-sol"
reasoning_effort = "high"

[agents.iteration]
kind = "cursor"
model = "gpt-5.6-sol"
reasoning_effort = "high"
args = ["--force", "--approve-mcps"]

[agents.integration]
kind = "codex"
model = "gpt-5.6-sol"
reasoning_effort = "high"

[agents.follow_up]
kind = "codex"
model = "gpt-5.6-terra"
reasoning_effort = "medium"

[follow_up]
pane_idle_timeout = "5m"
generator_concurrency = 1

[follow_up.baseline_verify]
max_messages = 8
generator_max_attempts = 3

[follow_up.iteration]
max_messages = 5
generator_max_attempts = 3

[follow_up.integration]
max_messages = 8
generator_max_attempts = 3

[[reference_projects]]
path = "/absolute/path/to/reference"
description = "Known-correct implementation"
```

This example deliberately mixes providers; the shipped `--defaults` configuration remains all-Codex. Cursor accepts reasoning effort `low`, `medium`, `high`, `xhigh`, or `max`; `ultra` is Codex-only. Its explicit provider-default configuration is `model = "auto"` with `reasoning_effort = ""`; init displays that real Cursor model as `auto-routing (default)` and does not ask for an effort. Codex's explicit provider default uses empty model and effort strings, leaving both choices to the Codex CLI configuration. Cursor `args` is an argv array, never a shell string. Pika supplies workspace, Plugin, and model. It defaults unattended Cursor Sessions to `--yolo`; an explicit `--sandbox enabled` in the Role's `args` selects the restricted sandbox instead. Configuration cannot request `--resume`, `--continue`, `--print`, an initial prompt, or disabled sandboxing.

Exact defaults beyond the accepted five-minute inactivity timeout remain implementation choices and are printed by interactive `pika-go init` before commit.

If `pika.toml` already exists, init treats it as user-owned input: it validates version and repository identity, preserves the file byte-for-byte, and rejects invalid scheduler, Context, Follow-up, or Role Agent fields. `iteration_concurrency`, `max_pending_attempts`, and `context.iteration.history_limit` are copied into the durable Optimization during init. Every Attempt freezes the current history limit when it is created; later edits affect only a future Optimization rather than silently changing running work. A limit of `0` disables cross-Attempt history injection.

## 3. Static Agent selection

Each core Role has one default Agent Configuration. Follow-up has one shared Agent Configuration even though it has three target-specific System Prompts and three matching user-instruction overlays.

Commands that create a fresh Session may accept `--agent <configured-name>` as an explicit one-shot override, including Back-off. There is no automatic provider fallback chain. If launch fails, the Work remains recoverable and the error is surfaced.

Pika rejects configuration that enables automatic Follow-up for an Agent adapter lacking reliable turn-stop and conversation capabilities.

## 4. Initialization

`pika-go init` performs an interactive flow similar to the old Pika initializer but excludes Web concerns. For a new interactive instance, it asks independently for backend, model, reasoning effort, and Cursor launch permissions for all five static Roles. All of these inputs are numbered lists. The backend list contains only providers that passed daemon probing; Codex model choices come from its visible local model cache, and Cursor choices are derived from the pinned CLI's `--list-models` output. Effort choices are narrowed to the selected model, and arbitrary provider or model IDs are not accepted. Cursor command approval offers automatic allow (`--force`, the unattended default), Cursor auto-review, or interactive approval; MCP approval and persistent workspace trust are separate choices. The default remains `--force --approve-mcps` without changing workspace trust. Raw Cursor argv is available only through a complete `--config` file for advanced use.

New non-interactive or `--json` initialization requires exactly one of:

- `--defaults`, which renders the all-Codex default configuration;
- `--config PATH`, which reads a complete candidate TOML for the same canonical repository.

The CLI first reads `GET /v1/init/options`, including each available provider's model/effort catalog. It sends the complete candidate to `POST /v1/init`; the daemon validates it and probes exactly the distinct referenced providers before any durable write. Codex-only init does not require Cursor, Cursor-only init does not require Codex, and mixed init rolls back atomically if either provider fails. Existing configuration is never regenerated or overwritten and therefore skips the questions and rejects both flags.

It configures:

- repository and writable worktree roots;
- default Agent Configuration for the four core Roles;
- one Follow-up Agent Configuration;
- Iteration concurrency and queue limits;
- reference projects;
- stop conditions and measurement defaults;
- empty per-Role instruction overlay files;
- provider hook/profile installation;
- SQLite and Git workspace initialization.

It does not configure a Web host, port, token, LiveView, progress-summary Role, dynamic Role registry, guidance service, or provider fallback chain.

Successful init commits configuration, binds the caller pane, exits the CLI, and lets the daemon start Baseline Draft in that same pane. Failure terminates initialization and the associated instance.

## 5. Instruction editing and freezing

Instruction overlay files are created empty only when absent. Upgrades never overwrite user edits silently. `pika-go edit-instruction <name>` edits only this overlay; it cannot display or modify the embedded Role System Prompt.

At Agent Session creation, Pika records:

- instruction logical name;
- absolute source path;
- user-instruction content and digest, including the valid empty value;
- the complete rendered System Prompt and digest.

The rendered System Prompt has three ordered layers:

1. immutable Role policy embedded in the binary;
2. the frozen Context Bundle locator, SHA-256 digests, and complete versioned JSON Schemas for `context.json`, `messages.jsonl`, and terminal Attempt `summary.jsonl` records;
3. the current user instruction overlay, appended only when non-empty.

`context.json` contains the exact Work/generation, Baseline Revision, Definition digest, submitted Repository Snapshot SHA, assigned repository, terminal operation, and Role-specific Attempt/Best/Integration/Follow-up identities. `messages.jsonl` contains the complete normalized cross-Session history for that Work, including full observable tool and shell payloads. Iteration additionally receives up to its frozen N most-recent accepted/rejected Attempts, presented chronologically with summaries plus digest-addressed `attempt-history/<attempt-id>/messages.jsonl` and `summary.jsonl` files. A Follow-up generator receives the target Work's projection and the same frozen Iteration history policy. The complete three-layer prompt and files are frozen with the Agent Session. Editing an overlay affects only later Sessions; retrying the same Session verifies and reuses its stored prompt and Context Bundle byte-for-byte, while a fresh recovery Session snapshots the latest overlay and committed facts.

## 6. Codex managed profile

Initialization creates or updates only:

```text
$CODEX_HOME/pika-go-managed.config.toml
```

It never clones `CODEX_HOME` and does not rewrite the user's base config or Herdr `hooks.json` entry. The managed profile supplies Pika MCP definitions, explicit `PIKA_GO_SOCKET`/`PIKA_MCP_GRANT` forwarding, Role-catalog MCP auto-approval, and inline `[hooks]` tables. It is selected with `--profile pika-go-managed` for Pika-launched Codex Agents. The instance wrapper also passes the frozen per-Session prompt through Codex `developer_instructions` and sets `--ask-for-approval never` so an autonomous Work cannot block on a shell approval; the Codex `workspace-write` sandbox remains active and is not widened to `danger-full-access`. Role-required Git metadata writes use `commit_changes` or `apply_best_update` in Pika's grant-scoped control plane. Herdr `agent.prompt` carries only the short kickoff User Turn and later Follow-up/User steering. Codex loads these hooks additively with hooks from other active configuration layers.

Production launch never bypasses first-use project or hook trust. After the user reviews the five Pika-managed Codex hooks, Codex writes per-hook SHA-256 trust state into the managed profile. Later Pika initialization preserves only valid trust entries whose keys match those exact five hooks in that exact profile; foreign entries are discarded, and a changed hook hash requires review again. Tests may set a test-only bypass while using a disposable trusted repository; that setting is not persisted in managed configuration.

The update is atomic and preserves the previous Pika-owned file as a recoverable backup until validation succeeds. If another product already owns the reserved profile path, init fails with a precise conflict rather than merging unknown content.

## 7. Cursor integration and Session state

When any Role selects Cursor, init atomically merges Pika-owned entries into the authenticated user's existing `~/.cursor/mcp.json` and `~/.cursor/hooks.json`. Existing servers, Cursor hooks, and Herdr hooks are preserved. The reserved `pika_go` MCP entry is static but parameterized with `${env:PIKA_GO_EXECUTABLE}`, `${env:PIKA_GO_SOCKET}`, `${env:PIKA_MCP_GRANT}`, and `${env:PIKA_SESSION_ID}`; Hook commands are likewise parameterized by the launched Session environment. This permits concurrent Pika Sessions without rewriting shared configuration. A conflicting reserved entry fails; an init rollback restores the exact original bytes or removes a newly created file.

For each Cursor Agent Session, the Provider Adapter creates a private `0700` directory containing a `0600` frozen System Prompt and argv snapshot. `sessionStart` returns the prompt through `additional_context`. Its binary-owned provider bootstrap requires `GetDynamicTools(namespace="pika_go")` before Role MCP calls; it is not an editable instruction overlay. Cursor keeps the real `HOME` and authentication state. Cleanup occurs only after the child exits or Herdr confirms the pane absent; startup removes stale directories not referenced by an active Pika Agent Session. A retry reuses the frozen launch, while recovery always creates new private state for a new Session.

## 8. Herdr fresh-Session configuration

The active Herdr configuration must explicitly contain:

```toml
[session]
resume_agents_on_restore = false
```

`pika-go kick-off` checks this setting before creating a Workspace. When it is missing or not false, the command asks permission to atomically update only this setting and reload Herdr; refusal or non-interactive EOF leaves the file unchanged. A failed reload restores the exact original file and attempts to reload that restored configuration. Direct `pika-go init` remains read-only: it validates the resolved Herdr configuration and requires `server.reload_config` to return `applied`. Codex and Cursor launch wrappers also reject native resume arguments. This keeps provider session IDs as journal correlation only and makes every daemon recovery a fresh Pika Agent Session.

## 9. Validation

Before committing init or configuration edits, validate:

- all paths are absolute and within allowed roots;
- Agent kinds resolve through the Provider Adapter registry;
- referenced executables exist, are authenticated, and report the required exact-compatible capabilities;
- concurrency and Follow-up counts are bounded positive integers;
- duration values parse and are not negative;
- every static embedded Role System Prompt is non-empty and its matching instruction overlay exists and is readable;
- Codex profile ownership and hook feature are valid when Codex is referenced;
- Cursor model, effort, reserved argv, additive global MCP/Hook ownership, private Session state, strict tool schemas, and hook contracts are valid when Cursor is referenced;
- Herdr native Agent restore is explicitly disabled and the live server accepts a configuration reload;
- automatic Follow-up is enabled only for capable provider adapters;
- repository and Git preconditions are safe.

Configuration is versioned. Unknown keys are errors during init and warnings/errors according to a documented migration policy during daemon startup; they are never silently ignored if they can affect safety or completion semantics.
