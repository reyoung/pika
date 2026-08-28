# Configuration

## 1. Ownership and paths

Herdr supplies two writable plugin locations:

```text
$HERDR_PLUGIN_CONFIG_DIR
$HERDR_PLUGIN_STATE_DIR
```

Pika uses them as follows:

```text
$HERDR_PLUGIN_CONFIG_DIR/
  instances/<instance>/
    config.toml
    instructions/
      baseline.md
      baseline-verify.md
      iteration.md
      integration.md
      follow-up/
        baseline-verify.md
        iteration.md
        integration.md

$HERDR_PLUGIN_STATE_DIR/
  instances/<instance>/
    pika.db
    logs/
    contexts/
    evidence/
    worktrees/
    runtime/
      cursor-sessions/<agent-session>/
        system-prompt.md
        cursor.args
```

Configuration and instruction overlays are user-owned mutable inputs. Every installed `instructions/*.md` file is empty by default; it exists only so the user can append Role-specific requirements. The canonical Role System Prompts are plugin-owned resources embedded in the `pika-go` binary and are never copied into the config directory. SQLite and generated evidence are runtime state. Reinstalling the plugin must preserve both writable directories.

## 2. Example configuration

```toml
version = 1

[optimization]
repository = "/absolute/path/to/repo"

[scheduler]
iteration_concurrency = 4
max_pending_attempts = 8

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

This example deliberately mixes providers; the shipped `--defaults` configuration remains all-Codex. Cursor accepts reasoning effort `low`, `medium`, `high`, `xhigh`, or `max`; `ultra` is Codex-only. Its explicit provider-default configuration is `model = "auto"` with `reasoning_effort = ""`; init displays that real Cursor model as `auto-routing (default)` and does not ask for an effort. Codex's explicit provider default uses empty model and effort strings, leaving both choices to the Codex CLI configuration. Cursor `args` is an argv array, never a shell string. Pika supplies workspace, Plugin, model, and `--sandbox enabled`, so configuration cannot override those values or request `--resume`, `--continue`, `--print`, an initial prompt, or disabled sandboxing.

Exact defaults beyond the accepted five-minute inactivity timeout remain implementation choices and are printed by interactive `pika-go init` before commit.

If `config.toml` already exists, init treats it as user-owned input: it validates version and repository identity, preserves the file byte-for-byte, and rejects invalid scheduler, Follow-up, or Role Agent fields. `iteration_concurrency` and `max_pending_attempts` are copied into the durable Optimization during init; later edits affect only a future Optimization rather than silently changing the running scheduler.

## 3. Static Agent selection

Each core Role has one default Agent Configuration. Follow-up has one shared Agent Configuration even though it has three target-specific System Prompts and three matching user-instruction overlays.

Commands that create a fresh Session may accept `--agent <configured-name>` as an explicit one-shot override, including Back-off. There is no automatic provider fallback chain. If launch fails, the Work remains recoverable and the error is surfaced.

Pika rejects configuration that enables automatic Follow-up for an Agent adapter lacking reliable turn-stop and conversation capabilities.

## 4. Initialization

`pika-go init` performs an interactive flow similar to the old Pika initializer but excludes Web concerns. For a new interactive instance, it asks independently for backend, model, reasoning effort, and Cursor argv for all five static Roles. Backend, model, and reasoning effort are numbered lists. The backend list contains only providers that passed daemon probing; Codex model choices come from its visible local model cache, and Cursor choices are derived from the pinned CLI's `--list-models` output. Effort choices are narrowed to the selected model, and arbitrary provider or model IDs are not accepted. Cursor defaults to `--force --approve-mcps`; appending `--trust` is a separate question that defaults to no because it changes Cursor's persistent workspace trust.

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
2. daemon-rendered dynamic System Context from committed Work state;
3. the current user instruction overlay, appended only when non-empty.

The dynamic layer includes the exact Work/generation, Baseline Revision, Definition digest, submitted Repository Snapshot SHA, assigned repository, terminal MCP, and Role-specific Attempt/Best/Integration/Follow-up identities. A successor Baseline Draft also receives the predecessor Revision's failure kind, reason, and requested changes; full verification evidence remains behind `get_context`. Larger and more volatile facts remain behind `get_context`. The complete three-layer prompt is frozen with the Agent Session. Editing a file affects only later Sessions; retrying the same Session reuses its stored prompt byte-for-byte, while a fresh recovery Session reads the latest overlay and current committed Work facts.

## 6. Codex managed profile

Initialization creates or updates only:

```text
$CODEX_HOME/pika-go-managed.config.toml
```

It never clones `CODEX_HOME` and does not rewrite the user's base config or Herdr `hooks.json` entry. The managed profile supplies Pika MCP definitions, explicit `PIKA_GO_SOCKET`/`PIKA_MCP_GRANT` forwarding, Role-catalog MCP auto-approval, and inline `[hooks]` tables. It is selected with `--profile pika-go-managed` for Pika-launched Codex Agents. The instance wrapper also passes the frozen per-Session prompt through Codex `developer_instructions` and sets `--ask-for-approval never` so an autonomous Work cannot block on a shell approval; the Codex `workspace-write` sandbox remains active and is not widened to `danger-full-access`. Role-required Git metadata writes use `commit_changes` or `apply_best_update` in Pika's grant-scoped control plane. Herdr `agent.prompt` carries only the short kickoff User Turn and later Follow-up/User steering. Codex loads these hooks additively with hooks from other active configuration layers.

Production launch never bypasses first-use project or hook trust. Tests may set a test-only bypass while using a disposable trusted repository; that setting is not persisted in managed configuration.

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
