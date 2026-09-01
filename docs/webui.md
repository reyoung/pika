# Read-only lineage Workbench

`pika-go webui` is an HTTP sidecar attached to one live Workspace daemon. It does not read SQLite or Herdr directly: all domain state and runtime observation come from the daemon's `/v1/workbench/*` Unix-socket projection.

```bash
pika-go webui \
  --workspace /absolute/path/to/kernel-pika-workspace \
  --listen 0.0.0.0:8080
```

The command creates `runtime/webui/` with mode `0700`, creates or reuses `runtime/webui/token` with mode `0600`, and records the sidecar PID in `runtime/webui/pid`. `--rotate-token` atomically replaces the token. The printed URL uses a fragment, so the token is not sent in the initial HTTP request; the application imports it into `sessionStorage` and clears the fragment.

All `/api/*` requests require `Authorization: Bearer <token>`. Only GET requests to the Workbench version, snapshot, node, and registered-artifact endpoints are proxied. CORS is disabled and responses carry a strict Content Security Policy.

The default snapshot contains the complete Best spine, every active branch, and the 25 most recent terminal Attempts. Each selected Round expands into ordered Experiment nodes with outcome state, daemon-derived measurement summaries, checkpoint ancestry, Candidate-to-Integration promotion, and receipt-backed evidence links. The UI can select 5 through 100 terminal Attempts. Refresh defaults to 60 seconds with a minimum of 5 seconds; manual refresh is always available. A failed refresh keeps the last coherent snapshot visible and marks it stale.

Artifacts are restricted to records registered by the daemon. The daemon revalidates root containment, symlink resolution, size, and SHA-256 on every read. JSON, text, and Markdown files no larger than 1 MiB may be previewed; other registered artifacts are download-only.

## Structured benchmark measurements

New Baseline Revisions freeze a `benchmark_measurements` version 1 contract: Case weights plus metric identity, label, unit, role, direction, sample statistic, aggregation, and `iteration_performance_gate`. Exactly one metric is primary. Experiment and Integration transactions submit raw per-Case reference/candidate values; Pika computes ratios, regressions, aggregates, maximum Case speedup, and the 10x retest requirement inside the transaction before persisting normalized rows associated with Experiment, receipt, and scope Best SHA.

Existing Baseline Revisions migrated from an earlier schema remain valid but are not heuristically backfilled. The Workbench labels them as legacy evidence without structured metrics.
