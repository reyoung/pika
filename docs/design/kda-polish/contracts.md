# Contracts

This document is the single source of truth for flow-v2 data and command
interfaces. Examples illustrate required shape; implementation must publish
complete JSON Schemas through the existing MCP and Context Bundle mechanisms.

## 1. Versioning

Flow v2 introduces these independently versioned contracts:

| Contract | Initial version |
| --- | ---: |
| Optimization flow | 2 |
| Skill snapshot manifest | 1 |
| Diagnosis report | 1 |
| Iteration Experiment | 1 |
| Optimization knowledge record | 1 |
| Context Bundle | 4 |

Schema versions are exact integers. Readers reject a newer unsupported version
instead of guessing. Existing flow-v1 commands and Context Bundle v3 remain
supported for Optimizations frozen to flow version 1.

## 2. Skill snapshot

`pika-snapshot.json` is immutable after publication:

```json
{
  "schema_version": 1,
  "snapshot_id": "sha256-of-canonical-snapshot-manifest",
  "resolved_at": "2026-08-31T00:00:00Z",
  "skills": [
    {
      "name": "KernelWiki",
      "repository": "https://github.com/mit-han-lab/KernelWiki.git",
      "branch": "master",
      "commit_sha": "40-hex-git-sha",
      "path": "skills/KernelWiki",
      "content_sha256": "64-hex-sha256"
    },
    {
      "name": "ncu-report-skill",
      "repository": "https://github.com/mit-han-lab/ncu-report-skill.git",
      "branch": "main",
      "commit_sha": "40-hex-git-sha",
      "path": "skills/ncu-report-skill",
      "content_sha256": "64-hex-sha256"
    }
  ]
}
```

Rules:

- the array contains exactly those two names in the displayed order;
- repository and branch must equal the flow-v2 product constants;
- each commit is the branch head resolved during initialization;
- `content_sha256` covers all files exposed to the Agent, excluding `.git`;
- paths are relative to the plugin root and cannot contain symlink escapes;
- `snapshot_id` covers the canonical manifest fields except itself;
- Context Bundles use absolute paths plus the recorded content digest; they do
  not duplicate skill contents.

The generated `plugin.json` is also immutable:

```json
{
  "$schema": "https://agent-plugins.org/schemas/1.0.0/plugin.schema.json",
  "name": "pika-kda-skills",
  "description": "Frozen kernel optimization skills for one Pika Optimization",
  "version": "1.0.0",
  "author": {"name": "pika-go"}
}
```

The manifest declares identity only. Agent Plugin folder discovery must expose
exactly `skills/KernelWiki/SKILL.md` and
`skills/ncu-report-skill/SKILL.md`; validation rejects any other discovered
skill, MCP manifest, hook, rule, command, or Agent definition.

## 3. Diagnosis

### 3.1 Domain identity

One accepted Baseline Revision in flow v2 owns exactly one Diagnosis record and
one Diagnosis Work. Its terminal status is `ready`, `unavailable`, or
`cancelled`. A retry with the same idempotency request returns the original
receipt.

### 3.2 `finish_diagnosis`

The Diagnosis Role has one MCP operation:

```text
finish_diagnosis  # terminal: ready | unavailable
```

Input:

```json
{
  "idempotency_key": "unique-request-key",
  "outcome": "ready",
  "report": {
    "schema_version": 1,
    "subject": {
      "baseline_revision_id": "baseline-id",
      "best_sha": "git-sha",
      "hardware": {},
      "software": {}
    },
    "coverage": {
      "case_ids": ["case-a"],
      "dispatch_paths": ["kernel-or-dispatch-identity"]
    },
    "artifacts": [
      {"path": "reports/full-case-a.ncu-rep", "kind": "ncu-report"}
    ],
    "observations": [
      {
        "id": "observation-1",
        "case_ids": ["case-a"],
        "metric": "gpu__time_duration.sum",
        "value": 12.5,
        "unit": "us",
        "source_artifacts": ["reports/full-case-a.ncu-rep"],
        "summary": "measured fact"
      }
    ],
    "bottlenecks": [
      {
        "id": "bottleneck-1",
        "class": "tail-effect",
        "confidence": "high",
        "observation_ids": ["observation-1"],
        "summary": "evidence-backed diagnosis"
      }
    ],
    "hypotheses": [
      {
        "id": "hypothesis-1",
        "rank": 1,
        "summary": "concrete optimization direction",
        "mechanism": "why the change addresses the bottleneck",
        "target_case_ids": ["case-a"],
        "expected_effect": "metric and direction",
        "risk": "correctness or regression risk",
        "bottleneck_ids": ["bottleneck-1"],
        "knowledge_refs": ["KernelWiki page id or path"]
      }
    ],
    "limitations": []
  }
}
```

Validation:

- `subject` must identify the active accepted Baseline and Best revision 0;
- `coverage.case_ids` is a non-empty subset of the initial Iteration Case Set;
- observation, bottleneck, and hypothesis IDs are unique within the report;
- every reference resolves inside the report or to a submitted artifact;
- `rank` is a dense sequence beginning at one;
- confidence is `high`, `medium`, or `low`;
- every artifact path resolves inside the Work-owned Diagnosis evidence root;
- stable reads record artifact size, SHA-256, contract version, Work, and
  terminal receipt;
- source repository HEAD and cleanliness match the accepted Baseline snapshot;
- `ready` requires at least one observation, bottleneck, and hypothesis;
- `unavailable` requires at least one limitation with attempted command,
  failure category, exit status when available, and factual error text. It may
  contain lower-confidence static observations and hypotheses.

The daemon does not interpret arbitrary NCU metric names. It validates
identity, provenance, reference integrity, and structural completeness; the
Integration Agent remains responsible for the technical conclusion.

## 4. Iteration Experiments

### 4.1 Identity and order

An Experiment belongs to exactly one Attempt and Iteration Round. Pika assigns
its ID and strictly increasing Round-local sequence when
`record_iteration_experiment` commits. The parent checkpoint must equal the
Round's current checkpoint at that transaction.

Experiment outcome is one of:

- `kept`: correctness and the frozen Iteration gates pass, the measured change
  is worth preserving, and the checkpoint advances;
- `rejected`: evidence shows the change should not be preserved;
- `inconclusive`: evidence is insufficient to decide, and the change is not
  preserved.

### 4.2 `record_iteration_experiment`

The Iteration catalog adds one non-terminal MCP operation:

```text
commit_changes
record_iteration_experiment
finish_iteration
```

Input:

```json
{
  "idempotency_key": "unique-request-key",
  "experiment": {
    "schema_version": 1,
    "parent_checkpoint_sha": "git-sha",
    "hypothesis": {
      "diagnosis_hypothesis_id": "hypothesis-1",
      "summary": "specific claim tested by this change"
    },
    "change": {
      "summary": "what changed",
      "paths": ["relative/source/path"],
      "mechanism": "why it should affect the measured metric"
    },
    "outcome": "kept",
    "checkpoint_sha": "git-sha",
    "correctness": {
      "benchmark_integrity": {
        "schema_version": 1,
        "cases": []
      }
    },
    "performance": {
      "primary_metric": "latency",
      "aggregation": "workload-weighted or Baseline-defined name",
      "direction": "minimize",
      "parent_value": 12.5,
      "candidate_value": 11.0,
      "unit": "us",
      "ratio": 1.136,
      "gate_passed": true,
      "cases": [
        {
          "case_id": "case-a",
          "parent_value": 12.5,
          "candidate_value": 11.0,
          "unit": "us",
          "ratio": 1.136,
          "guard_passed": true
        }
      ]
    },
    "artifacts": [
      {"path": "experiments/exp-1/benchmark.json", "kind": "benchmark"}
    ],
    "summary": "measured conclusion and remaining caveats"
  }
}
```

`hypothesis` must contain either a Diagnosis hypothesis ID or an inline
hypothesis summary. It may contain both when the experiment refines a Diagnosis
hypothesis.

`performance.direction` is exactly `minimize` or `maximize`. Ratios and gate
comparisons follow that declared direction and the frozen Baseline Definition;
the daemon rejects internally inconsistent values.

For `kept`:

- `checkpoint_sha`, correctness, and performance are required;
- correctness must satisfy the existing benchmark-integrity contract for
  exactly the Round-frozen Iteration Case Set;
- performance cases must cover the same Case IDs exactly once;
- the Baseline-defined primary and guard gates must pass;
- `checkpoint_sha` must be the clean current HEAD, descend from
  `parent_checkpoint_sha`, and contain only paths committed through the
  existing `commit_changes` grant;
- the transaction advances the Round current checkpoint to `checkpoint_sha`.

For `rejected` and `inconclusive`:

- `checkpoint_sha` is absent;
- correctness and performance may be partial but cannot claim gates that were
  not measured;
- current HEAD must equal `parent_checkpoint_sha` and the worktree must be
  clean;
- the Round current checkpoint does not change.

All outcomes require a concrete summary. Submitted artifacts are read stably
and recorded through the existing evidence-artifact mechanism. Idempotent
replay returns the original Experiment; reuse of one key with different input
is rejected.

### 4.3 Candidate terminal operation

Flow-v2 Candidate input adds `experiment_id`:

```json
{
  "idempotency_key": "finish-key",
  "outcome": "candidate",
  "experiment_id": "experiment-id",
  "candidate_sha": "git-sha",
  "summary": "candidate summary",
  "evidence": {}
}
```

The referenced Experiment must:

- belong to the active Attempt and Round;
- have outcome `kept`;
- be the latest kept checkpoint after all recorded Experiments;
- have `checkpoint_sha == candidate_sha`;
- carry correctness evidence byte-equivalent to the terminal Candidate's
  benchmark-integrity evidence.

The terminal evidence may add detail but cannot contradict the Experiment.
Flow-v1 Candidate input remains unchanged. A rejected flow-v2 Attempt does not
require `experiment_id`, allowing infrastructure or feasibility failures before
a meaningful experiment.

## 5. Knowledge projection

Pika deterministically projects `knowledge.jsonl`; no additional Agent writes
it. Each line is one record:

```json
{
  "schema_version": 1,
  "knowledge_id": "stable-id",
  "kind": "hypothesis",
  "status": "provisional",
  "scope": {
    "case_ids": ["case-a"],
    "best_sha": "git-sha",
    "hardware": {}
  },
  "summary": "fact or bounded claim",
  "source": {
    "diagnosis_id": "diagnosis-id",
    "experiment_id": "experiment-id",
    "integration_id": "integration-id"
  },
  "evidence_artifact_ids": ["artifact-id"]
}
```

Allowed `kind` values are `observation`, `bottleneck`, `hypothesis`,
`experiment-result`, and `integration-verdict`. Allowed status values are:

| Source event | Status |
| --- | --- |
| Diagnosis observation/hypothesis | `provisional` |
| locally kept Experiment | `provisional` |
| rejected or inconclusive Experiment | `observed-negative` or `inconclusive` |
| accepted Integration | corresponding positive records become `verified` |
| rejected Integration | concrete implementation becomes `integration-rejected` |

An Integration rejection never turns a general optimization technique into a
universal prohibition. Scope always retains Case IDs, Best SHA, environment,
and source identities.

## 6. Context Bundle v4

Flow-v2 Context adds these top-level references:

```json
{
  "skill_snapshot": {
    "snapshot_id": "...",
    "manifest": {"path": "...", "sha256": "...", "bytes": 1}
  },
  "diagnosis": {
    "status": "ready",
    "report": {"path": "...", "sha256": "...", "bytes": 1}
  },
  "knowledge": {"path": "...", "sha256": "...", "bytes": 1, "records": 1},
  "iteration_context": {
    "experiments": {"path": "...", "sha256": "...", "bytes": 1, "records": 1}
  }
}
```

Materialization rules:

- skill manifest and Diagnosis report are always referenced for the coding
  Roles after they exist;
- Iteration receives the complete Experiment list for its current Round and a
  bounded knowledge projection from recent terminal Attempts;
- `context.iteration.history_limit` continues to bound selected historical
  Attempts, now selecting their knowledge and Experiment summaries before
  transcript references;
- current Work `messages.jsonl` remains complete and mandatory for replacement
  Session recovery;
- previous-Round and terminal-Attempt transcripts remain digest-addressed,
  on-demand references and are no longer mandatory pre-modification reading;
- every generated file is frozen per Session and validated by digest as today.

The rendered Iteration prompt states an explicit read order: context,
Diagnosis, knowledge, current Experiments, then recovery history when the
Session is replacing unfinished Work.

## 7. Storage additions

Conceptual tables:

| Table | Purpose |
| --- | --- |
| `skill_snapshots` | one frozen snapshot identity and manifest per Optimization |
| `skill_snapshot_entries` | repository, branch, commit, path, and content digest per skill |
| `diagnoses` | Baseline-owned terminal status and immutable report JSON |
| `iteration_experiments` | ordered Round-local Experiment JSON and checkpoint transition |

Evidence files continue to use `evidence_artifacts`; no second artifact table
is introduced. Domain events and operation receipts are written in the same
transaction as Diagnosis and Experiment mutations.

`optimizations.flow_version` defaults migrated rows to 1 and is required for
new rows. `iteration_rounds.current_checkpoint_sha` is the control-plane
ratchet cursor and begins at the Round Base SHA.

## 8. Observability

Human and JSON status views add:

- flow version;
- skill snapshot ID and both resolved commits;
- Diagnosis status and number of hypotheses;
- current Round Experiment count and current checkpoint;
- count of verified, provisional, negative, and inconclusive knowledge records.

Status prints identities and counts only. Full profiler output, source excerpts,
tool payloads, and potentially sensitive evidence remain in protected files and
SQLite.
