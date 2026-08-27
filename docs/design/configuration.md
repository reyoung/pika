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
```

Configuration and instructions are user-owned mutable inputs. SQLite and generated evidence are runtime state. Reinstalling the plugin must preserve both directories.

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
kind = "codex"
model = "gpt-5.6-sol"
reasoning_effort = "high"

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

Exact defaults beyond the accepted five-minute inactivity timeout remain implementation choices and must be printed by `pika-go init` before commit.

## 3. Static Agent selection

Each core Role has one default Agent Configuration. Follow-up has one shared Agent Configuration even though it has three Instruction Profiles.

Commands that create a fresh Session may accept `--agent <configured-name>` as an explicit one-shot override, including Back-off. There is no automatic provider fallback chain. If launch fails, the Work remains recoverable and the error is surfaced.

Pika rejects configuration that enables automatic Follow-up for an Agent adapter lacking reliable turn-stop and conversation capabilities.

## 4. Initialization

`pika-go init` performs an interactive flow similar to the old Pika initializer but excludes Web concerns.

It configures:

- repository and writable worktree roots;
- default Agent Configuration for the four core Roles;
- one Follow-up Agent Configuration;
- Iteration concurrency and queue limits;
- reference projects;
- stop conditions and measurement defaults;
- initial instruction files;
- provider hook/profile installation;
- SQLite and Git workspace initialization.

It does not configure a Web host, port, token, LiveView, progress-summary Role, dynamic Role registry, guidance service, or provider fallback chain.

Successful init commits configuration, binds the caller pane, exits the CLI, and lets the daemon start Baseline Draft in that same pane. Failure terminates initialization and the associated instance.

## 5. Instruction editing and freezing

Instruction files are copied from plugin defaults only when absent. Upgrades never overwrite user edits silently.

At Agent Session creation, Pika records:

- instruction logical name;
- absolute source path;
- content digest;
- frozen rendered activation digest.

Editing a file affects only later Sessions. A recovery Session always reads the latest instruction file while also receiving immutable prior Work facts; this is an intentional opportunity for the operator to improve instructions between Sessions.

## 6. Codex managed profile

Initialization creates or updates only:

```text
$CODEX_HOME/pika-go-managed.config.toml
```

It never clones `CODEX_HOME` and does not rewrite the user's base config or Herdr `hooks.json` entry. The managed profile supplies Pika MCP definitions and inline `[hooks]` tables, and is selected with `--profile pika-go-managed` for Pika-launched Codex Agents. Codex loads these hooks additively with hooks from other active configuration layers.

The update is atomic and preserves the previous Pika-owned file as a recoverable backup until validation succeeds. If another product already owns the reserved profile path, init fails with a precise conflict rather than merging unknown content.

## 7. Cursor plugin staging

The distribution may include a Pika Cursor plugin directory and pass it with `cursor-agent --plugin-dir`. This is dormant until the pinned CLI passes the provider conformance suite. Pika does not modify global Cursor configuration for the initial release.

## 8. Validation

Before committing init or configuration edits, validate:

- all paths are absolute and within allowed roots;
- Agent kinds are supported by the installed Herdr version;
- required executables exist;
- concurrency and Follow-up counts are bounded positive integers;
- duration values parse and are not negative;
- every static Instruction Profile exists and is readable;
- Codex profile ownership and hook feature are valid;
- automatic Follow-up is enabled only for capable provider adapters;
- repository and Git preconditions are safe.

Configuration is versioned. Unknown keys are errors during init and warnings/errors according to a documented migration policy during daemon startup; they are never silently ignored if they can affect safety or completion semantics.
