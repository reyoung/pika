# Role Contracts

Roles are static. Each Agent Session freezes a complete developer-level System Prompt, MCP catalog, Work identity, and completion rule when it is created.

## 1. System Prompt and instruction layers

The canonical System Prompt for each Role is compiled into the `pika-go` binary. It is product behavior, not user configuration, and cannot be changed with `$EDITOR`. Before activation the daemon writes a read-only, per-Session Context Bundle and appends its exact paths, SHA-256 digests, and complete versioned JSON Schemas to the System Prompt.

The user customization surface is a separate set of empty-by-default Markdown overlays:

```text
instructions/
  baseline.md
  baseline-verify.md
  iteration.md
  integration.md
  follow-up/
    baseline-verify.md
    iteration.md
    integration.md
```

Core Roles each have their own default Agent Configuration. Follow-up is one Role with one shared Agent Configuration, three target-specific immutable System Prompts, and three matching instruction overlays.

Instruction files contain only user additions. They are ordinary Markdown and are edited with:

```bash
pika-go edit-instruction baseline
pika-go edit-instruction baseline-verify
pika-go edit-instruction iteration
pika-go edit-instruction integration
pika-go edit-instruction follow-up/baseline-verify
pika-go edit-instruction follow-up/iteration
pika-go edit-instruction follow-up/integration
```

For a new Session the ordering is: immutable Role System Prompt, frozen Context Bundle contract, then non-empty user additions. A user edit affects only later Sessions. The same Session always receives the byte-identical stored prompt and Context Bundle on dispatch retry.

The bundle is rendered from a single committed projection. It is not an instruction file and is not user-editable:

| Session | Frozen `context.json` fields before the first tool call |
| --- | --- |
| All Roles | Optimization ID/status/revision, Work ID/generation/Role, Baseline ID/number/status/Definition digest, frozen Repository Snapshot SHA when submitted, assigned repository, terminal MCP |
| Successor Baseline Draft | predecessor Baseline Revision ID plus rejected/superseded verification failure kind, reason, requested changes, and evidence |
| Iteration | Attempt ID, Round, kind, Base SHA, current Best SHA/revision, Back-off message when present |
| Integration | Integration ID/FIFO/status, Attempt ID, Candidate/Base SHA, expected Best, current Best/revision, existing Git Intent ID/state when present |
| Follow-up | Request and target Work/Role, sequence, inactivity deadline, target-message budget, generator attempt/max, and target Role identities |

`context.json` contains domain facts and a reference to `messages.jsonl`. The JSONL file contains every normalized Turn across the relevant Work's Sessions, including complete observable tool and shell inputs and outputs and MCP receipts. Raw provider-hook events stay in SQLite and are not duplicated. A replacement Session receives a newly frozen projection, while a dispatch retry of the same Session verifies and reuses the stored bytes.

## 2. Shared contract

Every complete rendered System Prompt must state:

- exact Work identity and Git root;
- domain facts that are frozen and facts that may change;
- allowed MCP operations and the terminal operation;
- evidence/file contracts;
- that natural-language completion is insufficient;
- that reasonable long-running commands must be awaited rather than abandoned;
- that the user may intervene directly in the terminal;
- that another session may continue from the journal if this session is lost.

The immutable Role policy defines these rules and identity types. The System Prompt supplies the exact Context Bundle locations, digests, and schemas needed before the first tool call; the Agent must read both complete files rather than infer their shape or skip a large tail. Bundle files are protected as user-only because observable tool output may contain sensitive data, and never include raw grants or provider credentials.

## 3. Baseline Role

Purpose: define a measurable, correct Optimization contract and establish the Development Baseline without conflating it with Target.

MCP catalog:

```text
commit_changes                     # non-terminal, scoped/idempotent Git commit
submit_baseline_definition          # terminal
```

The submitted definition covers at least:

- Target and Development Baseline identity;
- cases and case criticality;
- correctness oracle and tolerances;
- metrics, aggregation, and regression guards;
- canonical harness and smoke procedure;
- measurement environment and repeat policy;
- stop conditions;
- evidence paths and digests.

The Definition is durably owned by its Baseline Revision. Its stored-byte digest identifies its content. Work and Agent Session IDs identify one execution only and must not be embedded as cross-Role validity requirements: the independent Verification necessarily has a different Work and Session. A successor Draft receives the prior verification failure fields and evidence rather than guessing why the predecessor was rejected. Repository commits use `commit_changes`; the Agent's ordinary shell does not need permission to write Git metadata.

`submit_baseline_definition` freezes the clean repository HEAD as a separate Repository Snapshot SHA. The Definition may identify an earlier Development Baseline, but it must not attempt to contain the SHA of the commit that contains that same tracked Definition: that is self-referential. Verification receives both the Definition digest and Repository Snapshot SHA from committed dynamic context.

The user may discuss and steer this Role directly. There is no automatic Baseline Follow-up and no separate Web review gate. Successful submission closes the draft Agent and starts Baseline Verification.

Migration source in the sibling legacy checkout: `../pika/priv/v2/roles/baseline_alignment.ex` (path expressed from the Pika-Go repository root). Preserve Target/Development/Oracle separation and measurement rigor; remove LiveView-specific question tooling.

## 4. Baseline Verification Role

Purpose: independently verify one immutable Baseline Revision and either accept it or request a successor revision.

MCP catalog:

```text
finish_baseline_verification        # terminal: accepted | rejected
```

The Role must not silently repair the submitted definition. A rejected result records concrete failure kind, evidence, reason, and requested changes. Acceptance creates the initial Best from the already frozen Repository Snapshot SHA. The verifier rejects if repository HEAD drifted after submission.

Verification treats the daemon's Definition digest as the identity of the stored JSON bytes. It does not materialize an inline Definition into the repository or compare that digest with an independently formatted file unless the Definition/artifact receipt explicitly declares the file as the submission source. The verifier must not reject a Baseline for repository dirtiness that its own temporary files created.

Baseline Verification establishes Development Baseline measurements and proves that the frozen protocol can judge future Candidates. Candidate improvement thresholds and stop conditions do not apply to the Development Baseline itself; a zero improvement relative to itself is expected. They apply in Iteration and Integration. The Baseline is rejected only when the Development Baseline is incorrect, unmeasurable, incomplete, unstable or implausible, or when the declared gate cannot be computed from the evidence.

This Role is Follow-up eligible.

Migration source in the sibling legacy checkout: `../pika/priv/v2/roles/baseline_verify.ex`.

## 5. Iteration Role

Purpose: improve one Attempt in its isolated Git workspace and produce a measured candidate or a durable rejection.

MCP catalog:

```text
commit_changes                     # non-terminal, scoped/idempotent Git commit
finish_iteration                    # terminal: candidate | rejected
```

The Role may modify only its assigned workspace. It cannot update Best. It reads current Baseline, Best, Back-off message, stale-round recovery facts, and the frozen recent terminal Attempt history. Pika selects the newest N accepted/rejected Attempts without outcome rebalancing, excludes current/non-terminal/cancelled Attempts, then presents the selection chronologically. Each entry has a durable summary and digest-addressed full normalized history; an empty Journal remains a valid zero-record file. A rejection proves only that execution failed under its recorded conditions and does not permanently ban the direction.

Candidate submission includes correctness and performance evidence for the required case set plus the exact Git state. The Candidate SHA comes from `commit_changes`, which stages only explicit relative paths in the assigned Attempt worktree and reports whether it is clean. Rejection preserves what was tried and why. This Role is Follow-up eligible.

Migration source in the sibling legacy checkout: `../pika/priv/v2/roles/iteration.ex`.

## 6. Integration Role

Purpose: serialize final evidence review and, only after authorization, advance Best.

MCP catalog:

```text
prepare_best_update                 # non-terminal, returns a bounded Git intent
apply_best_update                   # non-terminal, idempotently applies that intent in Pika
finish_integration                  # terminal: accepted | rejected
```

The Role:

- processes FIFO;
- verifies the full required case set, regression guards, noise, and Git preconditions;
- must not modify Best before a valid intent;
- applies only the authorized Git mutation through the grant-scoped Pika MCP control plane, never by connecting to the daemon from an ordinary Agent shell;
- supplies post-mutation Git evidence to `finish_integration`;
- never pushes or changes a user source branch.

Staleness or user Back-off creates a new Iteration Round of the same Attempt with a fresh Agent Session. This Role is Follow-up eligible.

Migration source in the sibling legacy checkout: `../pika/priv/v2/roles/integration.ex`.

## 7. Follow-up Role

Purpose: read the target Work's current facts and Conversation Journal and produce one concrete user message that helps the existing target Agent reach its missing terminal operation.

MCP catalog:

```text
submit_followup_message              # terminal
```

The Follow-up Context Bundle identifies the target Work/Role, required generator operation, Follow-up sequence and inactivity deadline, and supplies the target's current domain state and complete normalized Conversation Journal. The generator must not make the target Role's domain decision itself.

Instruction selection:

| Target Role | System Prompt / instruction overlay | Required target operation |
| --- | --- | --- |
| Baseline Verification | `follow-up/baseline-verify.md` | `finish_baseline_verification` |
| Iteration | `follow-up/iteration.md` | `finish_iteration` |
| Integration | `follow-up/integration.md` | `finish_integration` |

All three use the same configured coding-agent kind/model. Each generation attempt is a fresh Agent Session. If generation ends without `submit_followup_message`, Pika retries with another fresh generator session up to its separate generator limit.

Migration sources:

- `../pika/priv/v2/roles/baseline_verify_followup.ex`
- `../pika/priv/v2/roles/iteration_followup.ex`
- `../pika/priv/v2/roles/integration_followup.ex`

## 8. Completion and pane lifecycle

For every Role:

1. Validate MCP grant, Role, Work generation, and input schema.
2. Validate referenced files and idempotency key.
3. Commit result, operation receipt, domain events, and runtime outbox atomically.
4. Return the stable MCP result.
5. Close the Agent through the normal runtime lifecycle.
6. Project and schedule successor Work.

If the same terminal request is replayed, return the original result. If a different request reuses its idempotency key, reject it. A provider `Stop` before this sequence only makes the Agent idle and Follow-up eligible.
