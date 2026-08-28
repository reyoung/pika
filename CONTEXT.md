# Pika Optimization

Pika coordinates a long-running automatic optimization effort while keeping human intervention, agent execution, and domain decisions distinct.

## Optimization

**Optimization**:
The complete tuning effort for one Target, from establishing a Development Baseline through repeated Attempts and Best updates.
_Avoid_: Campaign, run, job

**Target**:
The program, kernel, model path, or other subject whose behavior the Optimization intends to improve.
_Avoid_: Baseline, candidate

**Development Baseline**:
The independent starting implementation used for measurement and subsequent modification; it retains its own identity even when initially identical to the Target.
_Avoid_: Target copy, original

**Baseline Revision**:
An immutable definition of cases, metrics, measurement procedure, correctness requirements, and stopping conditions proposed for the Optimization.
_Avoid_: Baseline draft, config version

**Repository Snapshot**:
The immutable Git commit containing the complete repository state submitted with a Baseline Revision. It is distinct from both Definition identity and the earlier Development Baseline.
_Avoid_: Baseline commit, Definition SHA, Development Baseline

**Baseline Verification**:
The independent evaluation of one Baseline Revision that either accepts it or requests a new revision.
_Avoid_: Review, smoke test

**Best**:
The currently accepted implementation and evidence against which new Attempts are evaluated.
_Avoid_: Latest, winner

## Optimization Work

**Attempt**:
A durable candidate improvement that may pass through multiple Iteration Rounds before it is accepted, rejected, or cancelled.
_Avoid_: Agent session, pane, trial run

**Iteration Round**:
One execution of an Attempt against a particular Best and Baseline Revision. A stale result or Back-off creates another round without rewriting earlier history.
_Avoid_: Retry, resumed session

**Integration**:
The serialized evaluation that decides whether an Attempt may update Best.
_Avoid_: Merge, verification

**Work**:
A durable unit of optimization activity assigned one Role and completed only by that Role's terminal operation.
_Avoid_: Turn, process, session

**Back-off**:
An explicit user-requested transition to an allowed earlier phase that creates a new revision or round while preserving prior history.
_Avoid_: Rollback, reset

**Cancel Work**:
A request to stop one Work's logical continuation. It is distinct from interrupting a single Agent turn.
_Avoid_: Cancel turn, shutdown

**Scheduler Pause**:
An operator-requested suspension of new Agent Session launches and Follow-up clocks that preserves current Work and Agent Session identities.
_Avoid_: Optimization Pause, Cancel Work, shutdown

## Agent Contracts

**Role**:
A static contract defining one kind of Work, its allowed operations, completion rule, Role System Prompt, and User Instructions selection.
_Avoid_: Agent configuration, provider

**Role System Prompt**:
Pika-owned immutable policy for one Role or Follow-up target kind. It is product behavior and is not user-editable.
_Avoid_: Instructions, Instruction Profile, guidance

**Dynamic System Context**:
A Session-frozen projection of committed Work identities appended to a Role System Prompt by Pika.
_Avoid_: User Instructions, live status, guidance

**User Instructions**:
An empty-by-default, user-editable Markdown overlay that can add requirements for future Agent Sessions without replacing Role policy.
_Avoid_: System Prompt, Instruction Profile, guidance record

**Agent Configuration**:
The provider, executable kind, model, and launch settings used to create an Agent Session. Multiple Role System Prompts and User Instructions selections may share one Agent Configuration.
_Avoid_: Role, instructions

**Agent Session**:
One fresh provider conversation executing one Work or Follow-up Request. Pika never treats provider resume as domain recovery.
_Avoid_: Work, pane

**Follow-up Request**:
A durable request for the Follow-up Role to produce one concrete message after an eligible Work remains incomplete and its pane appears inactive.
_Avoid_: User guidance, retry

**Terminal Operation**:
A Role-scoped MCP operation whose successful domain transaction completes the current Work.
_Avoid_: Final answer, idle state

## Runtime and History

**Optimization Workspace**:
The durable filesystem root that contains one Optimization's configuration, SQLite history, evidence, and Pika-owned Git Worktrees.
_Avoid_: Herdr Workspace, plugin state directory, repository

**Source Repository**:
The user-owned Git worktree whose common Git directory and starting commit anchor an Optimization; Pika does not assign Agent Work to this checkout.
_Avoid_: Base Worktree, Optimization Workspace

**Base Worktree**:
The Pika-owned linked Git worktree in which Baseline Work develops the initial accepted implementation.
_Avoid_: Source Repository, Best, target repo

**Herdr Workspace**:
The replaceable terminal workspace that hosts Pika panes and processes for an Optimization Workspace; its runtime ID is never Optimization identity.
_Avoid_: Optimization Workspace, instance

**Pane Binding**:
The current association between a Work, an Agent Session, and a Herdr pane. It is runtime location, not domain identity.
_Avoid_: Work identity

**Runtime Observation**:
Herdr's report about pane or Agent interaction state. It never proves domain completion.
_Avoid_: Work status, result

**Conversation Journal**:
The ordered user messages, Agent messages, and observable tool activity associated with Agent Sessions for one Work.
_Avoid_: Terminal transcript, source of truth

**Context Bundle**:
A bounded, role-specific projection of domain facts, prior conversation, and evidence supplied to a new Agent Session.
_Avoid_: Full database dump, resumed session
