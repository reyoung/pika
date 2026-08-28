# Pika-Go

Pika-Go is a Herdr plugin for long-running automatic optimization. The implementation is being delivered in the vertical slices defined by [the development plan](docs/design/development-plan.md).

Current implementation status: the Phase 0–8 first-release plan and Codex release gate pass. Phase 9 adds a provider-neutral adapter, additive Cursor MCP/Hook integration, frozen per-Session prompt injection, normalized Cursor journaling, static per-Role Codex/Cursor selection, and fresh-Session recovery. The pinned Cursor release and both mirror-image mixed-provider matrices pass their credentialed real-model gates; those commands remain explicit because they consume model quota. See the development plan for exact evidence.

Role System Prompts are immutable resources embedded in the binary. At each fresh Agent Session, Pika freezes the Role prompt together with committed dynamic Work context and an optional per-Role user instruction overlay. User instruction files are empty by default and can be edited with `pika-go edit-instruction <name>`.

## Build and verify

```bash
make install
make verify
make real-codex-integration  # opt-in; consumes model quota
make real-cursor-integration  # opt-in; consumes model quota
make real-mixed-provider-integration  # opt-in; consumes both providers' quota
```

`make install` compiles the native `pika-go` binary into the plugin root. That is the Herdr plugin build step. Pass `PREFIX=/usr/local` to also copy the CLI into `$(PREFIX)/bin`.

For a standalone binary installation, run:

```bash
./pika-go install
herdr plugin pane open --plugin pika-go --entrypoint symphony
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
