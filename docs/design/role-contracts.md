# Role Contracts

Roles are static. Each Role freezes its instruction content, Context Bundle, MCP catalog, Work identity, and completion rule when an Agent Session is created.

## 1. Instruction files

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

Core Roles each have their own default Agent Configuration. Follow-up is one Role with one shared Agent Configuration and three target-specific Instruction Profiles.

Instruction files are ordinary Markdown and are the durable user customization surface. They are edited with:

```bash
pika-go edit-instruction baseline
pika-go edit-instruction baseline-verify
pika-go edit-instruction iteration
pika-go edit-instruction integration
pika-go edit-instruction follow-up/baseline-verify
pika-go edit-instruction follow-up/iteration
pika-go edit-instruction follow-up/integration
```

## 2. Shared contract

Every Role instruction must state:

- exact Work identity and Git root;
- domain facts that are frozen and facts that may change;
- allowed MCP operations and the terminal operation;
- evidence/file contracts;
- that natural-language completion is insufficient;
- that reasonable long-running commands must be awaited rather than abandoned;
- that the user may intervene directly in the terminal;
- that another session may continue from the journal if this session is lost.

`get_context` returns bounded structured facts and absolute paths to larger context files. It does not return secrets or unrelated Work.

## 3. Baseline Role

Purpose: define a measurable, correct Optimization contract and establish the Development Baseline without conflating it with Target.

MCP catalog:

```text
get_context
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

The user may discuss and steer this Role directly. There is no automatic Baseline Follow-up and no separate Web review gate. Successful submission closes the draft Agent and starts Baseline Verification.

Migration source in the sibling legacy checkout: `../pika/priv/v2/roles/baseline_alignment.ex` (path expressed from the Pika-Go repository root). Preserve Target/Development/Oracle separation and measurement rigor; remove LiveView-specific question tooling.

## 4. Baseline Verification Role

Purpose: independently verify one immutable Baseline Revision and either accept it or request a successor revision.

MCP catalog:

```text
get_context
finish_baseline_verification        # terminal: accepted | rejected
```

The Role must not silently repair the submitted definition. A rejected result records concrete failure kind, evidence, reason, and requested changes. Acceptance creates the frozen Baseline Snapshot and initial Best.

This Role is Follow-up eligible.

Migration source in the sibling legacy checkout: `../pika/priv/v2/roles/baseline_verify.ex`.

## 5. Iteration Role

Purpose: improve one Attempt in its isolated Git workspace and produce a measured candidate or a durable rejection.

MCP catalog:

```text
get_context
finish_iteration                    # terminal: candidate | rejected
```

The Role may modify only its assigned workspace. It cannot update Best. It reads current Baseline, Best, relevant accepted/rejected history, Attempt hypothesis, Back-off message, and any stale-round recovery facts.

Candidate submission includes correctness and performance evidence for the required case set plus the exact Git state. Rejection preserves what was tried and why. This Role is Follow-up eligible.

Migration source in the sibling legacy checkout: `../pika/priv/v2/roles/iteration.ex`.

## 6. Integration Role

Purpose: serialize final evidence review and, only after authorization, advance Best.

MCP catalog:

```text
get_context
prepare_best_update                 # non-terminal, returns a bounded Git intent
finish_integration                  # terminal: accepted | rejected
```

The Role:

- processes FIFO;
- verifies the full required case set, regression guards, noise, and Git preconditions;
- must not modify Best before a valid intent;
- applies only the authorized Git mutation;
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

The activation provides read-only context paths and identifies the target kind, required terminal operation, Follow-up sequence, remaining budget, and generator attempt. The generator must not make the target Role's domain decision itself.

Instruction selection:

| Target Role | Instruction Profile | Required target operation |
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
