# Pika-Go

Pika-Go is a Herdr plugin for long-running automatic optimization. The implementation is being delivered in the vertical slices defined by [the development plan](docs/design/development-plan.md).

Current implementation status: the Phase 0–8 first-release plan is implemented and its release gate passes. A disposable real-Codex Optimization passes Baseline Draft/Verification, automatic Follow-up delivery, fresh-Session daemon recovery, parallel sibling-isolated cancellation, Integration Back-off, scoped Git commits, Best application, journal verification, and graceful drain. Repeated process-crash boundaries, cross-builds, checksums, and isolated plugin-link smokes pass; remote Linux/macOS CI confirms verify, cross-build, and clean-install behavior. See the development plan for exact evidence.

Role System Prompts are immutable resources embedded in the binary. At each fresh Agent Session, Pika freezes the Role prompt together with committed dynamic Work context and an optional per-Role user instruction overlay. User instruction files are empty by default and can be edited with `pika-go edit-instruction <name>`.

## Build and verify

```bash
make build
make verify
make real-codex-integration  # opt-in; consumes model quota
```

Build all first-release targets:

```bash
make dist VERSION=0.1.0-dev
```

`make dist` produces four `.tar.gz` release archives plus `dist/SHA256SUMS`. See [install, backup, and upgrade](docs/release.md) for operational steps.

## Local Herdr smoke

```bash
make build
herdr plugin link .
herdr plugin pane open --plugin pika-go --entrypoint symphony
./pika-go status --json
```

When run inside Herdr, daemon and status derive the same temporary bootstrap socket from the Herdr session socket and Workspace identity. An explicit path can be supplied with `--socket` or `PIKA_GO_SOCKET` for diagnostics and tests.

The daemon publishes durable `pika_instance` Workspace metadata and uses it to resolve the instance socket for later CLI and MCP traffic.
