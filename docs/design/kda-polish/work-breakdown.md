# Work Breakdown

This breakdown is ordered by dependency. Each work package should land as an
independently reviewable commit and must satisfy its completion criteria before
the next package relies on it. Package numbering is not an estimate.

## 0. Contract fixtures and flow version

### Work

- Add canonical JSON fixtures and schemas for skill snapshots, Diagnosis,
  Experiments, knowledge records, and Context Bundle v4.
- Add `optimizations.flow_version` with migration default 1; new Optimization
  creation writes 2.
- Route command validation and Context schema selection through the frozen flow
  version without changing flow-v1 behavior.
- Add schema migrations for skill snapshots, Diagnosis, Experiments, and the
  Round current checkpoint.

### Completion criteria

- A migrated flow-v1 database has byte-equivalent domain projections and can
  resume its existing Work.
- A newly initialized database reports flow version 2.
- Unsupported schema or flow versions fail before scheduling.
- Migration rollback tests leave the prior database recoverable.

## 1. Skill snapshot acquisition

### Work

- Implement the `SkillSnapshotManager` deep Module and its Git/filesystem
  internal seams.
- Resolve and clone KernelWiki `master` and ncu-report-skill `main`
  independently from the fixed upstream URLs.
- Validate commit identity, `SKILL.md` metadata, safe paths, repository content,
  and absence of injected plugin/hook/MCP manifests.
- Generate the Agent Plugin manifest and Pika snapshot manifest.
- Publish by digest-addressed atomic rename with user-only staging and read-only
  final content.
- Persist snapshot identity and entries as part of flow-v2 initialization.
- Reuse a verified snapshot after daemon restart without network access.

### Completion criteria

- Fake remotes can move both branches between Optimizations; each Optimization
  records its observed commits and never changes them afterward.
- Partial clone, wrong origin, missing skill, unsafe symlink, malformed
  frontmatter, manifest write failure, and publish collision all fail atomically.
- No Baseline Work or Agent Session exists after acquisition failure.
- A published snapshot passes both repository cleanliness and content-digest
  verification.

## 2. Provider skill injection

### Work

- Extend provider-neutral `SessionActivation` with frozen skill references.
- Make activation preparation attach the snapshot only for Baseline Draft,
  Baseline Verification, Diagnosis, Iteration, and Integration.
- During Workspace creation, reserve and ignore the two Pika-owned Codex
  discovery-link paths without changing tracked repository files.
- In the Codex Adapter, atomically mount and clean up both frozen skill
  directories under repo-local `.agents/skills` discovery paths.
- In the Cursor Adapter, add the snapshot root through `--plugin-dir`.
- Reserve the provider arguments owned by Pika so Role configuration cannot
  shadow or remove the injected skills.
- Extend provider probing to validate the required skill-injection feature only
  when that provider is referenced by a coding Role.

### Completion criteria

- Adapter tests assert exact argv arrays, including paths containing spaces and
  shell metacharacters, without shell evaluation.
- Codex mount tests cover tracked-path collisions, hostile pre-existing links,
  stale owned links, crash recovery, cleanup, and unchanged Git cleanliness.
- Existing user hooks, MCP configuration, discovered skills, and allowed plugin
  arguments remain present.
- Follow-up Sessions contain no KDA skill injection.
- No test observes writes to `$CODEX_HOME`, `~/.agents/skills`, or
  `~/.cursor/plugins`.
- Credentialed Codex and Cursor smoke Sessions can enumerate both frozen skill
  names and report their expected commit identities.

## 3. Diagnosis vertical slice

### Work

- Add Diagnosis Role, Work identity, immutable System Prompt, instruction
  overlay, MCP catalog, Context projection, and Follow-up prompt.
- Project Diagnosis after Baseline acceptance and initial Iteration Case Set
  seeding; block Iteration projection until Diagnosis is terminal.
- Create a protected Work-owned Diagnosis artifact directory and expose its
  path in Context.
- Implement `finish_diagnosis` validation, stable artifact reads, repository
  postcondition checks, receipts, domain events, and successor outbox effects.
- Add `ready`, `unavailable`, cancellation, recovery, and Follow-up exhaustion
  transitions.

### Completion criteria

- `ready` cannot finish without a fully linked observation, bottleneck, and
  ranked hypothesis.
- `unavailable` cannot finish without a factual attempted-command failure.
- Diagnosis source changes, HEAD drift, artifact escape, or digest instability
  reject the terminal command without advancing state.
- Both terminal outcomes release normal Iteration scheduling exactly once.
- Cancellation or Follow-up exhaustion pauses for intervention and never
  fabricates Diagnosis evidence.

## 4. Experiment Ledger vertical slice

### Work

- Add Round current checkpoint and ordered Experiment persistence.
- Implement `record_iteration_experiment` and its discoverable strict schema.
- Reuse benchmark-integrity validation for kept Experiment exact Case coverage.
- Verify performance Case coverage, Baseline gate claims, artifact receipts,
  Git ancestry, current HEAD, clean worktree, and permitted committed paths.
- Make the transaction advance the checkpoint only for `kept`.
- Require a flow-v2 Candidate to reference its latest kept Experiment; preserve
  the flow-v1 terminal interface.
- Include Experiment identity in Integration Context and verdict projection.

### Completion criteria

- Concurrent/reordered calls serialize to one deterministic Round sequence.
- Same-key replay is stable; same-key/different-payload is rejected.
- A kept record with incomplete Cases, a sideways commit, dirty worktree, stale
  parent, or failed gate cannot advance the checkpoint.
- Rejected and inconclusive records require restoration to the parent
  checkpoint.
- No Candidate reaches Integration without a matching latest kept Experiment.

## 5. Artifact-first Context and knowledge

### Work

- Materialize and freeze `diagnosis.json`, `experiments.jsonl`, and
  `knowledge.jsonl` with digest-addressed references in Context Bundle v4.
- Implement deterministic knowledge projection and confidence transitions from
  Diagnosis, Experiment, and Integration facts.
- Change the Iteration System Prompt to read artifacts first and distinguish
  same-Work recovery from new-Round exploration.
- Retain complete current, prior-Round, and selected terminal-Attempt journals;
  make historical transcript detail on-demand instead of mandatory.
- Update Follow-up projection so it can see target Diagnosis and Experiments
  without loading unrelated skill contents.

### Completion criteria

- Rebuilding a Context projection from identical SQLite state produces
  byte-identical files and digests.
- A replacement Session receives complete current-Work history and resumes an
  unfinished Experiment safely.
- A new Round can explain prior accepted and rejected work from structured
  artifacts without reading a transcript.
- Integration acceptance promotes only scoped matching records to `verified`;
  rejection never creates a global technique ban.
- History limit zero disables cross-Attempt knowledge and transcript injection
  while retaining current Diagnosis and current-Round Experiments.

## 6. Operator surfaces and documentation integration

### Work

- Add flow, skill snapshot, Diagnosis, Experiment, checkpoint, and knowledge
  summaries to human and JSON status.
- Add Diagnosis to pause/resume, cancel-work, graceful drain, recovery, pane
  naming, and instruction editing.
- Extend backup/restore and Workspace validation to cover toolkit provenance and
  evidence files without embedding the cloned repositories in SQLite.
- Update the accepted top-level design documents only after implementation
  behavior passes its release gate; until then this directory remains the
  proposed source.

### Completion criteria

- Status reveals exact skill commits and all new lifecycle states without
  printing evidence contents.
- Pause/resume and daemon crash tests preserve Diagnosis and Experiment
  identity.
- Backup validation proves SQLite and provenance refer to a present, verified
  skill snapshot or reports the missing external directory precisely.
- Help and instruction-name validation include Diagnosis without changing
  flow-v1 Work.

## 7. End-to-end gates

### Local deterministic gate

- Run all unit and contract tests.
- Run fake-provider lifecycle from init through Diagnosis, multiple Experiments,
  rejected Integration regression feedback, and accepted Best advancement.
- Run migration fixtures for every supported historical schema.
- Run corruption fixtures for both skill repositories, manifests, evidence, and
  Context files.

### Credentialed provider gate

- Codex-only flow v2: both skills are visible in every coding Role, Diagnosis
  completes, a recorded Experiment becomes a Candidate, and Integration
  advances Best.
- Cursor-only flow v2: the generated Agent Plugin exposes exactly both skills
  through `--plugin-dir` and completes the same lifecycle.
- Mixed-provider mirror matrices: provider switching preserves one snapshot,
  one Diagnosis, Experiment identities, Context digests, and recovery behavior.
- Crash once during skill acquisition, Diagnosis, Experiment recording, and
  Integration; every restart must either cleanly abort initialization or resume
  from committed domain state without duplicate mutation.

### Release criterion

Flow v2 becomes the new-Optimization default only when all local gates and the
three credentialed provider matrices pass. Flow v1 compatibility remains in
the same release and has an explicit resume test; it is not inferred from new
flow success.

## 8. Suggested commit sequence

```text
docs: specify KDA-inspired optimization polish
feat: version optimization flows and add storage contracts
feat: freeze latest kernel optimization skills per optimization
feat: inject frozen skills into codex and cursor sessions
feat: add profiler diagnosis work
feat: record iteration experiments and checkpoints
feat: project artifact-first optimization knowledge
feat: expose flow-v2 lifecycle and recovery status
test: gate codex cursor and mixed flow-v2 lifecycles
docs: accept KDA polish into the main design baseline
```

Each commit after the documentation commit must keep the repository buildable
and flow-v1 tests green. If one package requires a temporary compatibility
adapter, remove it in the same package that introduces its permanent caller;
do not accumulate parallel old and new implementations behind a shallow seam.
