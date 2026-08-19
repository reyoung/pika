# Phase 1 Recovery Report

## Result

Phase 1 passed on 2026-08-19 (Asia/Shanghai). Pika can start as a foreground source checkout or assembled Elixir Release, initialize one durable Campaign, survive `kill -9`, and recover the same singleton without reinitialization.

No startup, Cookie, API, or MCP Token value is retained in this report.

## Verified commands

```bash
mix check
MIX_ENV=prod mix release --overwrite
_build/prod/rel/pika/bin/pika serve \
  --workspace /tmp/pika-release-final.WORKSPACE \
  --config config/pika.example.yaml \
  --port 18655
```

Results:

- `mix check`: formatting, warnings-as-errors compilation, and tests passed; `76 passed, 1 excluded`. The excluded tests are the pre-existing Phase 0 external provider conformance suite.
- Production Release assembled successfully and its generated `bin/pika` advertised and ran the foreground `serve` command.
- Release HTTP status reported `drafting_spec`, `owned_repo`, `foreign_keys=1`, `journal_mode=wal`, `synchronous=2` (`FULL`), and `busy_timeout=5000`.

## Fault matrix

| Fault or invariant | Evidence | Result |
|---|---|---|
| Empty Workspace and second startup | `Pika.ConfigWorkspaceTest` | Fixed layout initialized; config hash and Git base survived recovery. |
| Invalid non-empty directory | `Pika.ConfigWorkspaceTest` | Rejected without changing the foreign file. |
| Dirty Managed Repo | `Pika.ManagedWorkspaceTest` | Rejected before creating a Workspace. |
| Two Pika OS processes for one Managed Repo | `Pika.Phase1ProcessRecoveryTest` | Contender exited with status 2; second Workspace remained absent. |
| Advisory lock ownership | `Pika.ManagedWorkspaceTest` | `.pika.lock` remained held by an OS `flock`; diagnostics contained Server UUID, OS PID, start time, and canonical Workspace. |
| Replaced symlink, repo inode, or Git common directory | `Pika.ManagedWorkspaceTest` | Each identity mutation was rejected during recovery. |
| SQLite transaction rollback | `Pika.PersistenceArtifactTest` | Neither Domain Event row nor PubSub message escaped the rolled-back transition. |
| State transition commit | `Pika.PersistenceArtifactTest` | Campaign state and Domain Event committed together; PubSub followed commit. |
| JSONL trailing half-line | `Pika.PersistenceArtifactTest` | Startup verification truncated only the incomplete suffix and retained complete records. |
| Artifact tampering | `Pika.PersistenceArtifactTest`, `Pika.Phase1ProcessRecoveryTest` | Hash/size mismatch was detected; restart entered `blocked` while HTTP diagnostics stayed available. |
| Lost Agent process skeleton | `Pika.PersistenceArtifactTest` | Running Session became `interrupted` once; repeated recovery reused the stable idempotency record. |
| Repeated Blocked recovery | `Pika.PersistenceArtifactTest` | One recovery Intent and one Domain Event were recorded for the same stable idempotency key. |
| Owned Repo `kill -9` | `Pika.Phase1ProcessRecoveryTest` | Same Campaign ID recovered; `campaigns` row count stayed 1. |
| Managed Repo `kill -9` | `Pika.Phase1ProcessRecoveryTest` | Lock released with the dead process and the same Campaign recovered after restart. |
| Old credentials after restart | `Pika.Phase1ProcessRecoveryTest` | Old URL Token, Cookie, JSON Bearer, and MCP Bearer all returned HTTP 401. |
| Authenticated shell and APIs | `Pika.Phase1ProcessRecoveryTest`, `PikaWeb.Phase1AuthTest` | HTML/LiveView shell, JSON, SSE boundary, and MCP share the startup authentication boundary. |

## Three-source recovery identity

- SQLite is authoritative for the singleton Campaign, orchestration status, config hash, Base/Best SHA, Artifact metadata, recovery Intents, idempotency records, and Domain Events.
- Git is authoritative for the actual repository, `pika/best`, HEAD SHA, Managed Repo directory identity, and Git common directory.
- The Artifact Workspace is authoritative for file bytes. Registered relative path, size, and SHA-256 are recomputed before automatic recovery proceeds.

Startup ordering is `config -> Managed Repo lock -> migration -> SQLite current state -> Git identity -> Artifact verification -> Agent supervisors/Endpoint`. A migration error stops the runtime child before either the Endpoint or Agent supervisors are started. An explainable JSONL half-line is repaired idempotently; an unexplained Git, SQLite, or Artifact discrepancy is rejected or persisted as `blocked`.
