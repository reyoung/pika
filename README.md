# Pika-Go

Pika-Go is a Herdr plugin for long-running automatic optimization. The implementation is being delivered in the vertical slices defined by [the development plan](docs/design/development-plan.md).

Current implementation status: the Phase 0–8 first-release plan and Codex release gate pass. Phase 9 adds a provider-neutral adapter, additive Cursor MCP/Hook integration, frozen per-Session prompt injection, normalized Cursor journaling, static per-Role Codex/Cursor selection, and fresh-Session recovery. The pinned Cursor release and both mirror-image mixed-provider matrices pass their credentialed real-model gates; those commands remain explicit because they consume model quota. See the development plan for exact evidence.

Role System Prompts are immutable resources embedded in the binary. At each fresh Agent Session, Pika materializes a read-only Context Bundle, then freezes the Role prompt together with the bundle's exact paths, SHA-256 digests, complete versioned JSON Schemas, and an optional per-Role user instruction overlay. User instruction files are empty by default and can be edited with `pika-go edit-instruction <name>`.

## Quick start

Install/register the plugin once, then start Pika-Go from a shell pane inside Herdr:

```bash
pika-go install
cd /path/to/repository
pika-go kick-off
```

`kick-off` first verifies that Herdr native Agent restore is disabled. If the required setting is missing, it asks before atomically updating the Herdr configuration and reloading the server; declining leaves the file untouched and creates no workspace. It then creates a durable sibling directory named `<repository>-pika-workspace`, creates a Pika-owned base linked worktree at `repo/`, and launches a replaceable Herdr workspace with the durable Workspace as its cwd. The daemon, `pika-go init`, and every Agent therefore run outside the user's source checkout.

Pause and continue the Scheduler without replacing current Work or Agent Sessions:

```bash
pika-go pause
pika-go resume
```

Pause freezes new Session starts and Follow-up clocks and interrupts active Agent turns. Resume sends `继续` to surviving pending-Work Sessions before releasing held scheduling. `pika-go status --json` reports durable Scheduler state and the latest per-Session control results.

The Workspace is the resume point for the complete autotune process:

```text
kernel-pika-workspace/
  workspace.json        immutable Workspace and source-Git identity
  pika.toml             user-owned provider and scheduler configuration
  pika.db               SQLite history, journal, receipts, and recovery state
  repo/                 Pika base linked worktree
  best/repo/            accepted Best linked worktree
  attempts/.../repo/    isolated Attempt linked worktrees
  instructions/         user-owned Role overlays
  contexts/<session-id>/
    context.json        committed domain projection
    messages.jsonl      complete normalized Work conversation history
    attempt-history/    bounded terminal Attempt summaries and detailed histories
  evidence/ logs/ runtime/
  herdr/binding.json    current replaceable Herdr layout binding
```

Each Agent reads its Context Bundle before acting. `messages.jsonl` spans all observed Sessions for the relevant Work and preserves complete normalized messages, shell/tool inputs and outputs, and MCP receipts; provider-specific raw hook events remain in SQLite and are not duplicated. A later Iteration Round receives the immediately preceding Round's full history for recovery while running in its own branch and Git worktree. Iteration bundles also expose the frozen N most-recent terminal Attempt summaries and digest-addressed detail files for selective reading. Retrying one Agent Session verifies and reuses byte-identical files, while a replacement Session receives a new reviewable snapshot.

Run `pika-go open /path/to/kernel-pika-workspace`, or run `pika-go kick-off` anywhere inside it, to open existing durable state. Existing configuration is reused and init is not repeated. A Workspace is intentionally not relocatable because it records the absolute source repository and Git common-directory filesystem identity.

During first init, backend, model, reasoning effort, and Cursor launch permissions are numbered choices rather than free-form IDs or raw argv. Cursor permissions separately cover command approval, MCP approval, and persistent workspace trust. Each provider exposes an explicit default model choice: Cursor shows `auto-routing (default)` and invokes its real `auto` model ID without a reasoning-effort override; Codex `default` preserves the Codex CLI's configured model and effort. Use `--defaults` for a non-interactive all-Codex configuration or `--config PATH` for a complete Workspace TOML.

Pre-Workspace instances are never migrated automatically. Discover and import a stopped instance explicitly:

```bash
pika-go workspace legacy-list
pika-go workspace import --instance INSTANCE_ID --workspace /absolute/path/to/workspace
pika-go open /absolute/path/to/workspace
```

Import preserves the legacy SQLite history, configuration, instructions, artifacts, linked worktrees, and source/Attempt HEAD, index, staged, unstaged, and untracked state. The legacy checkout is left in place.

## Build and verify

```bash
make install
make verify
make real-codex-integration  # opt-in; consumes model quota
make real-cursor-integration  # opt-in; consumes model quota
make real-mixed-provider-integration  # opt-in; consumes both providers' quota
make real-pause-resume-integration  # opt-in; full Codex/Cursor/mixed gates with Scheduler control
```

`make install` compiles the native `pika-go` binary into the plugin root and copies the CLI into `/usr/local/bin` by default. Override `PREFIX` to choose another installation prefix, or set `PREFIX=` to only build the plugin-root binary.

For a standalone binary installation, run:

```bash
./pika-go install
./pika-go kick-off --repository /path/to/repository
```

`pika-go install` copies the running binary and its embedded Herdr manifest to `$XDG_DATA_HOME/pika-go/plugin`, or `~/.local/share/pika-go/plugin` when `XDG_DATA_HOME` is unset, then registers that stable directory with Herdr. Use `--dir <absolute-path>` to override the destination or `--herdr <path>` to select a Herdr executable.

Build all first-release targets:

```bash
make dist VERSION=0.1.0-dev
```

`make dist` produces four `.tar.gz` release archives plus `dist/SHA256SUMS`. See [install, backup, and upgrade](docs/release.md) for operational steps.

## Local Herdr smoke

```bash
make install
herdr plugin link .
herdr plugin pane open --plugin pika-go --entrypoint symphony
./pika-go status --json
```

When run inside Herdr, daemon and status derive the same temporary transport socket from the Herdr server socket and Herdr workspace ID. Durable identity comes from `workspace.json`, not that temporary Herdr ID. An explicit path can be supplied with `--socket` or `PIKA_GO_SOCKET` for diagnostics and tests.

The daemon publishes durable `pika_instance` Workspace metadata and uses it to resolve the instance socket for later CLI and MCP traffic.
