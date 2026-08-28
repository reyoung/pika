# Pika-Go

Pika-Go is a Herdr plugin for long-running automatic optimization. The implementation is being delivered in the vertical slices defined by [the development plan](docs/design/development-plan.md).

Current implementation status: the Phase 0–8 first-release plan and Codex release gate pass. Phase 9 adds a provider-neutral adapter, additive Cursor MCP/Hook integration, frozen per-Session prompt injection, normalized Cursor journaling, static per-Role Codex/Cursor selection, and fresh-Session recovery. The pinned Cursor release and both mirror-image mixed-provider matrices pass their credentialed real-model gates; those commands remain explicit because they consume model quota. See the development plan for exact evidence.

Role System Prompts are immutable resources embedded in the binary. At each fresh Agent Session, Pika freezes the Role prompt together with committed dynamic Work context and an optional per-Role user instruction overlay. User instruction files are empty by default and can be edited with `pika-go edit-instruction <name>`.

## Quick start

Install/register the plugin once, then start Pika-Go from a shell pane inside Herdr:

```bash
pika-go install
cd /path/to/repository
pika-go kick-off
```

`kick-off` first verifies that Herdr native Agent restore is disabled. If the required setting is missing, it asks before atomically updating the Herdr configuration and reloading the server; declining leaves the file untouched and creates no workspace. It then creates a dedicated Herdr workspace, opens its daemon pane, waits for daemon health, starts a visible `pika-go init` command in the new workspace's root pane using the same binary that launched `kick-off`, and focuses that workspace, tab, and pane. Complete interactive setup there. Backend, model, and reasoning-effort prompts are numbered lists sourced from the available providers and their current model catalogs, so setup cannot submit a free-form provider or model ID. Each provider exposes an explicit default model choice: Cursor shows `auto-routing (default)` and invokes its real `auto` model ID without a reasoning-effort override; Codex `default` preserves the Codex CLI's configured model and effort. Use `--defaults` for a non-interactive all-Codex configuration or `--config PATH` for a complete instance TOML; those commands also run visibly in the new workspace.

## Build and verify

```bash
make install
make verify
make real-codex-integration  # opt-in; consumes model quota
make real-cursor-integration  # opt-in; consumes model quota
make real-mixed-provider-integration  # opt-in; consumes both providers' quota
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

When run inside Herdr, daemon and status derive the same temporary bootstrap socket from the Herdr session socket and Workspace identity. An explicit path can be supplied with `--socket` or `PIKA_GO_SOCKET` for diagnostics and tests.

The daemon publishes durable `pika_instance` Workspace metadata and uses it to resolve the instance socket for later CLI and MCP traffic.
