package symphony

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
)

const schemaVersion = 20

func CurrentSchemaVersion() int { return schemaVersion }

const schemaV1 = `
CREATE TABLE optimizations (
    id TEXT PRIMARY KEY,
    status TEXT NOT NULL,
    revision INTEGER NOT NULL,
    repository TEXT NOT NULL,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
CREATE TABLE baseline_revisions (
    id TEXT PRIMARY KEY,
    optimization_id TEXT NOT NULL REFERENCES optimizations(id),
    number INTEGER NOT NULL,
    status TEXT NOT NULL,
    definition_json BLOB,
    definition_digest TEXT,
    predecessor_id TEXT REFERENCES baseline_revisions(id),
    created_at TEXT NOT NULL,
    submitted_at TEXT,
    completed_at TEXT,
    UNIQUE(optimization_id, number)
);
CREATE TABLE baseline_verifications (
    id TEXT PRIMARY KEY,
    baseline_revision_id TEXT NOT NULL UNIQUE REFERENCES baseline_revisions(id),
    status TEXT NOT NULL,
    accepted INTEGER,
    failure_kind TEXT,
    reason TEXT,
    requested_changes TEXT,
    evidence_json BLOB,
    created_at TEXT NOT NULL,
    finished_at TEXT
);
CREATE TABLE works (
    id TEXT PRIMARY KEY,
    optimization_id TEXT NOT NULL REFERENCES optimizations(id),
    baseline_revision_id TEXT NOT NULL REFERENCES baseline_revisions(id),
    role TEXT NOT NULL,
    status TEXT NOT NULL,
    generation INTEGER NOT NULL,
    created_at TEXT NOT NULL,
    finished_at TEXT
);
CREATE TABLE back_offs (
    id TEXT PRIMARY KEY,
    optimization_id TEXT NOT NULL REFERENCES optimizations(id),
    source_work_id TEXT NOT NULL REFERENCES works(id),
    successor_baseline_revision_id TEXT NOT NULL REFERENCES baseline_revisions(id),
    message TEXT NOT NULL,
    created_at TEXT NOT NULL
);
CREATE TABLE operation_receipts (
    request_id TEXT PRIMARY KEY,
    request_digest TEXT NOT NULL,
    receipt_id TEXT NOT NULL UNIQUE,
    command_type TEXT NOT NULL,
    revision INTEGER NOT NULL,
    result_json BLOB NOT NULL,
    created_at TEXT NOT NULL
);
CREATE TABLE domain_events (
    sequence INTEGER PRIMARY KEY AUTOINCREMENT,
    id TEXT NOT NULL UNIQUE,
    optimization_id TEXT NOT NULL REFERENCES optimizations(id),
    revision INTEGER NOT NULL,
    event_type TEXT NOT NULL,
    payload_json BLOB NOT NULL,
    created_at TEXT NOT NULL
);
CREATE TABLE runtime_outbox (
    sequence INTEGER PRIMARY KEY AUTOINCREMENT,
    id TEXT NOT NULL UNIQUE,
    optimization_id TEXT NOT NULL REFERENCES optimizations(id),
    effect_type TEXT NOT NULL,
    payload_json BLOB NOT NULL,
    status TEXT NOT NULL,
    created_at TEXT NOT NULL
);
`

const schemaV2 = `
CREATE TABLE init_diagnostics (
    sequence INTEGER PRIMARY KEY AUTOINCREMENT,
    id TEXT NOT NULL UNIQUE,
    message TEXT NOT NULL,
    created_at TEXT NOT NULL
);
`

const schemaV3 = `
CREATE TABLE agent_sessions (
    id TEXT PRIMARY KEY,
    work_id TEXT NOT NULL REFERENCES works(id),
    generation INTEGER NOT NULL,
    role TEXT NOT NULL,
    agent_kind TEXT NOT NULL,
    agent_name TEXT NOT NULL UNIQUE,
    status TEXT NOT NULL,
    created_at TEXT NOT NULL,
    ended_at TEXT
);
CREATE UNIQUE INDEX agent_sessions_one_current
    ON agent_sessions(work_id, generation)
    WHERE status IN ('starting', 'running');
CREATE TABLE pane_bindings (
    id TEXT PRIMARY KEY,
    agent_session_id TEXT NOT NULL UNIQUE REFERENCES agent_sessions(id),
    workspace_id TEXT NOT NULL,
    tab_id TEXT NOT NULL,
    pane_id TEXT NOT NULL,
    terminal_id TEXT NOT NULL,
    current INTEGER NOT NULL,
    last_observed_at TEXT NOT NULL
);
CREATE UNIQUE INDEX pane_bindings_current_terminal
    ON pane_bindings(terminal_id)
    WHERE current = 1;
`

const schemaV4 = `
ALTER TABLE runtime_outbox ADD COLUMN attempts INTEGER NOT NULL DEFAULT 0;
ALTER TABLE runtime_outbox ADD COLUMN claimed_at TEXT;
`

const schemaV5 = `
CREATE TABLE session_grants (
    id TEXT PRIMARY KEY,
    agent_session_id TEXT NOT NULL REFERENCES agent_sessions(id),
    token_sha256 TEXT NOT NULL UNIQUE,
    catalog_json BLOB NOT NULL,
    expires_at TEXT NOT NULL,
    revoked_at TEXT,
    created_at TEXT NOT NULL
);
CREATE UNIQUE INDEX session_grants_one_active
    ON session_grants(agent_session_id)
    WHERE revoked_at IS NULL;
CREATE TABLE instruction_snapshots (
    agent_session_id TEXT PRIMARY KEY REFERENCES agent_sessions(id),
    logical_name TEXT NOT NULL,
    source_path TEXT NOT NULL,
    content_sha256 TEXT NOT NULL,
    content BLOB NOT NULL,
    activation_sha256 TEXT NOT NULL,
    created_at TEXT NOT NULL
);
CREATE TABLE evidence_artifacts (
    id TEXT PRIMARY KEY,
    work_id TEXT NOT NULL REFERENCES works(id),
    receipt_id TEXT,
    relative_path TEXT NOT NULL,
    byte_size INTEGER NOT NULL,
    content_sha256 TEXT NOT NULL,
    contract_version INTEGER NOT NULL,
    created_at TEXT NOT NULL
);
`

const schemaV6 = `
ALTER TABLE optimizations ADD COLUMN iteration_concurrency INTEGER NOT NULL DEFAULT 4;
ALTER TABLE optimizations ADD COLUMN max_pending_attempts INTEGER NOT NULL DEFAULT 8;
ALTER TABLE works ADD COLUMN attempt_id TEXT;
ALTER TABLE works ADD COLUMN iteration_round INTEGER;
ALTER TABLE works ADD COLUMN integration_id TEXT;
CREATE TABLE best_revisions (
    id TEXT PRIMARY KEY,
    optimization_id TEXT NOT NULL REFERENCES optimizations(id),
    sequence INTEGER NOT NULL,
    commit_sha TEXT NOT NULL,
    source_attempt_id TEXT,
    evidence_json BLOB,
    created_at TEXT NOT NULL,
    UNIQUE(optimization_id, sequence)
);
CREATE TABLE attempts (
    id TEXT PRIMARY KEY,
    optimization_id TEXT NOT NULL REFERENCES optimizations(id),
    slot_index INTEGER NOT NULL,
    status TEXT NOT NULL,
    base_best_sequence INTEGER NOT NULL,
    base_sha TEXT NOT NULL,
    current_iteration_round INTEGER NOT NULL,
    candidate_sha TEXT,
    summary TEXT,
    failure_reason TEXT,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
CREATE TABLE iteration_rounds (
    id TEXT PRIMARY KEY,
    attempt_id TEXT NOT NULL REFERENCES attempts(id),
    round INTEGER NOT NULL,
    kind TEXT NOT NULL,
    base_sha TEXT NOT NULL,
    status TEXT NOT NULL,
    back_off_message TEXT,
    created_at TEXT NOT NULL,
    finished_at TEXT,
    UNIQUE(attempt_id, round)
);
CREATE TABLE integrations (
    sequence INTEGER PRIMARY KEY AUTOINCREMENT,
    id TEXT NOT NULL UNIQUE,
    attempt_id TEXT NOT NULL REFERENCES attempts(id),
    iteration_round INTEGER NOT NULL,
    status TEXT NOT NULL,
    candidate_sha TEXT NOT NULL,
    expected_best_sha TEXT NOT NULL,
    intent_id TEXT,
    validation_json BLOB,
    result_json BLOB,
    created_at TEXT NOT NULL,
    finished_at TEXT
);
CREATE TABLE git_intents (
    id TEXT PRIMARY KEY,
    integration_id TEXT NOT NULL UNIQUE REFERENCES integrations(id),
    state TEXT NOT NULL,
    expected_best_sha TEXT NOT NULL,
    candidate_sha TEXT NOT NULL,
    payload_json BLOB NOT NULL,
    created_at TEXT NOT NULL,
    applied_at TEXT
);
`

const schemaV7 = `
ALTER TABLE integrations ADD COLUMN fifo_position INTEGER;
UPDATE integrations SET fifo_position = sequence;
CREATE UNIQUE INDEX attempts_one_active_per_slot
    ON attempts(optimization_id, slot_index)
    WHERE status = 'iterating';
CREATE INDEX integrations_fifo ON integrations(fifo_position, sequence);
`

const schemaV8 = `
ALTER TABLE agent_sessions ADD COLUMN provider TEXT;
ALTER TABLE agent_sessions ADD COLUMN provider_session_id TEXT;
ALTER TABLE agent_sessions ADD COLUMN provider_ended_at TEXT;
CREATE UNIQUE INDEX agent_sessions_provider_identity
    ON agent_sessions(provider, provider_session_id)
    WHERE provider_session_id IS NOT NULL;
CREATE TABLE provider_events (
    sequence INTEGER PRIMARY KEY AUTOINCREMENT,
    id TEXT NOT NULL UNIQUE,
    provider TEXT NOT NULL,
    agent_session_id TEXT NOT NULL REFERENCES agent_sessions(id),
    provider_session_id TEXT NOT NULL,
    provider_turn_id TEXT,
    hook_event_name TEXT NOT NULL,
    event_digest TEXT NOT NULL,
    raw_json BLOB NOT NULL,
    observed_at TEXT NOT NULL,
    UNIQUE(provider, event_digest)
);
CREATE TABLE conversation_turns (
    id TEXT PRIMARY KEY,
    agent_session_id TEXT NOT NULL REFERENCES agent_sessions(id),
    work_id TEXT NOT NULL REFERENCES works(id),
    provider_session_id TEXT NOT NULL,
    provider_turn_id TEXT NOT NULL,
    status TEXT NOT NULL,
    user_message TEXT,
    assistant_message TEXT,
    started_at TEXT NOT NULL,
    stopped_at TEXT,
    UNIQUE(provider_session_id, provider_turn_id)
);
CREATE TABLE tool_events (
    id TEXT PRIMARY KEY,
    conversation_turn_id TEXT NOT NULL REFERENCES conversation_turns(id),
    provider_session_id TEXT NOT NULL,
    provider_turn_id TEXT NOT NULL,
    provider_tool_use_id TEXT NOT NULL,
    tool_name TEXT NOT NULL,
    input_json BLOB NOT NULL,
    output_json BLOB,
    observed_at TEXT NOT NULL,
    UNIQUE(provider_session_id, provider_tool_use_id)
);
`

const schemaV9 = `
ALTER TABLE works ADD COLUMN parent_work_id TEXT REFERENCES works(id);
ALTER TABLE works ADD COLUMN followup_request_id TEXT;
CREATE TABLE followup_requests (
    sequence INTEGER PRIMARY KEY AUTOINCREMENT,
    id TEXT NOT NULL UNIQUE,
    target_work_id TEXT NOT NULL REFERENCES works(id),
    target_agent_session_id TEXT NOT NULL REFERENCES agent_sessions(id),
    target_provider_turn_id TEXT NOT NULL,
    target_role TEXT NOT NULL,
    request_sequence INTEGER NOT NULL,
    status TEXT NOT NULL,
    inactivity_timeout_ms INTEGER NOT NULL,
    due_at TEXT NOT NULL,
    last_observed_pane_activity_at TEXT,
    activity_source TEXT,
    generator_work_id TEXT REFERENCES works(id),
    message TEXT,
    delivery_id TEXT,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    delivered_at TEXT,
    UNIQUE(target_agent_session_id, target_provider_turn_id, request_sequence)
);
CREATE UNIQUE INDEX followup_one_active_per_target
    ON followup_requests(target_work_id)
    WHERE status IN ('waiting', 'generating', 'ready', 'dispatching', 'delivery_unknown');
CREATE INDEX followup_due ON followup_requests(status, due_at, sequence);
`

const schemaV10 = `
ALTER TABLE instruction_snapshots ADD COLUMN system_prompt BLOB NOT NULL DEFAULT X'';
`

const schemaV11 = `
ALTER TABLE followup_requests ADD COLUMN target_max_messages INTEGER NOT NULL DEFAULT 1;
ALTER TABLE followup_requests ADD COLUMN generator_max_attempts INTEGER NOT NULL DEFAULT 1;
ALTER TABLE followup_requests ADD COLUMN generator_attempts INTEGER NOT NULL DEFAULT 0;
`

const schemaV12 = `
ALTER TABLE baseline_revisions ADD COLUMN repository_sha TEXT;
`

const schemaV13 = `
CREATE TABLE provider_events_v13 (
    sequence INTEGER PRIMARY KEY AUTOINCREMENT,
    id TEXT NOT NULL UNIQUE,
    provider TEXT NOT NULL,
    agent_session_id TEXT NOT NULL REFERENCES agent_sessions(id),
    provider_session_id TEXT NOT NULL,
    provider_turn_id TEXT,
    hook_event_name TEXT NOT NULL,
    event_digest TEXT NOT NULL,
    raw_json BLOB NOT NULL,
    observed_at TEXT NOT NULL,
    UNIQUE(provider, agent_session_id, event_digest)
);
INSERT INTO provider_events_v13(sequence, id, provider, agent_session_id, provider_session_id, provider_turn_id, hook_event_name, event_digest, raw_json, observed_at)
    SELECT sequence, id, provider, agent_session_id, provider_session_id, provider_turn_id, hook_event_name, event_digest, raw_json, observed_at
    FROM provider_events;

CREATE TABLE conversation_turns_v13 (
    id TEXT PRIMARY KEY,
    provider TEXT NOT NULL,
    agent_session_id TEXT NOT NULL REFERENCES agent_sessions(id),
    work_id TEXT NOT NULL REFERENCES works(id),
    provider_session_id TEXT NOT NULL,
    provider_turn_id TEXT NOT NULL,
    status TEXT NOT NULL,
    user_message TEXT,
    assistant_message TEXT,
    started_at TEXT NOT NULL,
    stopped_at TEXT,
    UNIQUE(provider, provider_session_id, provider_turn_id)
);
INSERT INTO conversation_turns_v13(id, provider, agent_session_id, work_id, provider_session_id, provider_turn_id, status, user_message, assistant_message, started_at, stopped_at)
    SELECT ct.id, COALESCE(s.provider, s.agent_kind), ct.agent_session_id, ct.work_id, ct.provider_session_id, ct.provider_turn_id,
           ct.status, ct.user_message, ct.assistant_message, ct.started_at, ct.stopped_at
    FROM conversation_turns ct JOIN agent_sessions s ON s.id = ct.agent_session_id;

CREATE TABLE tool_events_v13 (
    id TEXT PRIMARY KEY,
    provider TEXT NOT NULL,
    conversation_turn_id TEXT NOT NULL REFERENCES conversation_turns_v13(id),
    provider_session_id TEXT NOT NULL,
    provider_turn_id TEXT NOT NULL,
    provider_tool_use_id TEXT NOT NULL,
    tool_name TEXT NOT NULL,
    input_json BLOB NOT NULL,
    output_json BLOB,
    status TEXT NOT NULL,
    error_message TEXT,
    failure_type TEXT,
    duration_ms INTEGER,
    is_interrupt INTEGER NOT NULL DEFAULT 0,
    observed_at TEXT NOT NULL,
    UNIQUE(provider, provider_session_id, provider_tool_use_id)
);
INSERT INTO tool_events_v13(id, provider, conversation_turn_id, provider_session_id, provider_turn_id, provider_tool_use_id, tool_name,
    input_json, output_json, status, observed_at)
    SELECT te.id, ct.provider, te.conversation_turn_id, te.provider_session_id, te.provider_turn_id, te.provider_tool_use_id, te.tool_name,
           te.input_json, te.output_json, 'completed', te.observed_at
    FROM tool_events te JOIN conversation_turns_v13 ct ON ct.id = te.conversation_turn_id;

DROP TABLE tool_events;
DROP TABLE conversation_turns;
DROP TABLE provider_events;
ALTER TABLE provider_events_v13 RENAME TO provider_events;
ALTER TABLE conversation_turns_v13 RENAME TO conversation_turns;
ALTER TABLE tool_events_v13 RENAME TO tool_events;

CREATE TABLE tool_event_supplements (
    sequence INTEGER PRIMARY KEY AUTOINCREMENT,
    id TEXT NOT NULL UNIQUE,
    provider TEXT NOT NULL,
    agent_session_id TEXT NOT NULL REFERENCES agent_sessions(id),
    conversation_turn_id TEXT NOT NULL REFERENCES conversation_turns(id),
    provider_session_id TEXT NOT NULL,
    provider_turn_id TEXT NOT NULL,
    supplement_kind TEXT NOT NULL,
    tool_name TEXT,
    server_name TEXT,
    input_json BLOB,
    output_json BLOB NOT NULL,
    duration_ms INTEGER,
    observed_at TEXT NOT NULL
);
`

const schemaV14 = `
ALTER TABLE agent_sessions ADD COLUMN provider_version TEXT;
ALTER TABLE agent_sessions ADD COLUMN provider_capabilities_json BLOB;

DROP INDEX agent_sessions_provider_identity;
CREATE INDEX agent_sessions_provider_identity ON agent_sessions(provider, provider_session_id);

CREATE TABLE conversation_turns_v14 (
    id TEXT PRIMARY KEY,
    provider TEXT NOT NULL,
    agent_session_id TEXT NOT NULL REFERENCES agent_sessions(id),
    work_id TEXT NOT NULL REFERENCES works(id),
    provider_session_id TEXT NOT NULL,
    provider_turn_id TEXT NOT NULL,
    status TEXT NOT NULL,
    user_message TEXT,
    assistant_message TEXT,
    started_at TEXT NOT NULL,
    stopped_at TEXT,
    UNIQUE(provider, agent_session_id, provider_session_id, provider_turn_id)
);
INSERT INTO conversation_turns_v14
    SELECT * FROM conversation_turns;

CREATE TABLE tool_events_v14 (
    id TEXT PRIMARY KEY,
    provider TEXT NOT NULL,
    agent_session_id TEXT NOT NULL REFERENCES agent_sessions(id),
    conversation_turn_id TEXT NOT NULL REFERENCES conversation_turns_v14(id),
    provider_session_id TEXT NOT NULL,
    provider_turn_id TEXT NOT NULL,
    provider_tool_use_id TEXT NOT NULL,
    tool_name TEXT NOT NULL,
    input_json BLOB NOT NULL,
    output_json BLOB,
    status TEXT NOT NULL,
    error_message TEXT,
    failure_type TEXT,
    duration_ms INTEGER,
    is_interrupt INTEGER NOT NULL DEFAULT 0,
    observed_at TEXT NOT NULL,
    UNIQUE(provider, agent_session_id, provider_session_id, provider_tool_use_id)
);
INSERT INTO tool_events_v14(id, provider, agent_session_id, conversation_turn_id, provider_session_id, provider_turn_id,
    provider_tool_use_id, tool_name, input_json, output_json, status, error_message, failure_type, duration_ms, is_interrupt, observed_at)
    SELECT te.id, te.provider, ct.agent_session_id, te.conversation_turn_id, te.provider_session_id, te.provider_turn_id,
           te.provider_tool_use_id, te.tool_name, te.input_json, te.output_json, te.status, te.error_message, te.failure_type,
           te.duration_ms, te.is_interrupt, te.observed_at
    FROM tool_events te JOIN conversation_turns ct ON ct.id = te.conversation_turn_id;

CREATE TABLE tool_event_supplements_v14 (
    sequence INTEGER PRIMARY KEY AUTOINCREMENT,
    id TEXT NOT NULL UNIQUE,
    provider TEXT NOT NULL,
    agent_session_id TEXT NOT NULL REFERENCES agent_sessions(id),
    conversation_turn_id TEXT NOT NULL REFERENCES conversation_turns_v14(id),
    provider_session_id TEXT NOT NULL,
    provider_turn_id TEXT NOT NULL,
    supplement_kind TEXT NOT NULL,
    tool_name TEXT,
    server_name TEXT,
    input_json BLOB,
    output_json BLOB NOT NULL,
    duration_ms INTEGER,
    observed_at TEXT NOT NULL
);
INSERT INTO tool_event_supplements_v14
    SELECT * FROM tool_event_supplements;

DROP TABLE tool_event_supplements;
DROP TABLE tool_events;
DROP TABLE conversation_turns;
ALTER TABLE conversation_turns_v14 RENAME TO conversation_turns;
ALTER TABLE tool_events_v14 RENAME TO tool_events;
ALTER TABLE tool_event_supplements_v14 RENAME TO tool_event_supplements;
`

const schemaV15 = `
CREATE TABLE workspace_identity (
    singleton INTEGER PRIMARY KEY CHECK(singleton = 1),
    workspace_id TEXT NOT NULL UNIQUE,
    root TEXT NOT NULL,
    source_repository TEXT NOT NULL,
    git_common_dir TEXT NOT NULL,
    git_common_dir_device INTEGER NOT NULL,
    git_common_dir_inode INTEGER NOT NULL,
    initial_sha TEXT NOT NULL,
    created_at TEXT NOT NULL
);
CREATE TABLE git_worktrees (
    role TEXT NOT NULL,
    attempt_id TEXT NOT NULL DEFAULT '',
    iteration_round INTEGER NOT NULL DEFAULT 0,
    branch TEXT NOT NULL UNIQUE,
    repository TEXT NOT NULL UNIQUE,
    head_sha TEXT NOT NULL,
    state TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    PRIMARY KEY(role, attempt_id, iteration_round)
);
`

const schemaV16 = `
CREATE TABLE context_snapshots (
    agent_session_id TEXT PRIMARY KEY REFERENCES agent_sessions(id),
    schema_version INTEGER NOT NULL,
    context_relative_path TEXT NOT NULL,
    context_sha256 TEXT NOT NULL,
    context_bytes INTEGER NOT NULL,
    messages_relative_path TEXT NOT NULL,
    messages_sha256 TEXT NOT NULL,
    messages_bytes INTEGER NOT NULL,
    message_records INTEGER NOT NULL,
    created_at TEXT NOT NULL
);
`

const schemaV17 = `
ALTER TABLE optimizations ADD COLUMN scheduler_status TEXT NOT NULL DEFAULT 'running';
ALTER TABLE optimizations ADD COLUMN scheduler_paused_at TEXT;
ALTER TABLE optimizations ADD COLUMN scheduler_epoch INTEGER NOT NULL DEFAULT 0;

CREATE TABLE scheduler_control_cycles (
    id TEXT PRIMARY KEY,
    optimization_id TEXT NOT NULL REFERENCES optimizations(id),
    epoch INTEGER NOT NULL,
    action TEXT NOT NULL,
    status TEXT NOT NULL,
    created_at TEXT NOT NULL,
    completed_at TEXT,
    UNIQUE(optimization_id, epoch)
);
CREATE TABLE session_control_actions (
    id TEXT PRIMARY KEY,
    cycle_id TEXT NOT NULL REFERENCES scheduler_control_cycles(id),
    agent_session_id TEXT NOT NULL REFERENCES agent_sessions(id),
    work_id TEXT NOT NULL REFERENCES works(id),
    role TEXT NOT NULL,
    agent_kind TEXT NOT NULL,
    agent_name TEXT NOT NULL,
    action TEXT NOT NULL,
    message TEXT,
    status TEXT NOT NULL,
    observed_agent_status TEXT,
    error_message TEXT,
    started_at TEXT,
    completed_at TEXT,
    observed_at TEXT,
    suppress_activity_until TEXT,
    created_at TEXT NOT NULL,
    UNIQUE(cycle_id, agent_session_id)
);
CREATE INDEX scheduler_control_actions_cycle_status
    ON session_control_actions(cycle_id, status);
`

const schemaV18 = `
ALTER TABLE optimizations ADD COLUMN iteration_history_limit INTEGER NOT NULL DEFAULT 20 CHECK(iteration_history_limit >= 0);
ALTER TABLE attempts ADD COLUMN history_limit INTEGER NOT NULL DEFAULT 20 CHECK(history_limit >= 0);
`

const schemaV19 = `
ALTER TABLE optimizations ADD COLUMN iteration_case_set_version INTEGER;
ALTER TABLE iteration_rounds ADD COLUMN iteration_case_set_version INTEGER;
ALTER TABLE iteration_rounds ADD COLUMN evidence_json BLOB;
ALTER TABLE integrations ADD COLUMN regression_cases_json BLOB;

CREATE TABLE iteration_cases (
    optimization_id TEXT NOT NULL REFERENCES optimizations(id),
    case_id TEXT NOT NULL,
    ordinal INTEGER NOT NULL CHECK(ordinal >= 0),
    added_in_version INTEGER NOT NULL CHECK(added_in_version >= 1),
    source TEXT NOT NULL CHECK(source IN ('seed', 'integration_regression', 'manual_migration')),
    source_integration_id TEXT REFERENCES integrations(id),
    source_rank INTEGER,
    created_at TEXT NOT NULL,
    PRIMARY KEY(optimization_id, case_id),
    UNIQUE(optimization_id, ordinal)
);
CREATE TABLE iteration_round_cases (
    attempt_id TEXT NOT NULL,
    round INTEGER NOT NULL,
    ordinal INTEGER NOT NULL CHECK(ordinal >= 0),
    case_id TEXT NOT NULL,
    PRIMARY KEY(attempt_id, round, case_id),
    UNIQUE(attempt_id, round, ordinal),
    FOREIGN KEY(attempt_id, round) REFERENCES iteration_rounds(attempt_id, round)
);
`

const schemaV20 = `
ALTER TABLE baseline_revisions ADD COLUMN measurement_contract_version INTEGER CHECK(measurement_contract_version = 1);

CREATE TABLE benchmark_metric_definitions (
    baseline_revision_id TEXT NOT NULL REFERENCES baseline_revisions(id),
    metric_id TEXT NOT NULL,
    label TEXT NOT NULL,
    unit TEXT NOT NULL,
    role TEXT NOT NULL CHECK(role IN ('primary', 'guard', 'informational')),
    direction TEXT NOT NULL CHECK(direction IN ('lower_is_better', 'higher_is_better')),
    sample_statistic TEXT NOT NULL,
    aggregation TEXT NOT NULL CHECK(aggregation IN ('weighted_geomean_of_ratios', 'ratio_of_weighted_arithmetic_means')),
    PRIMARY KEY(baseline_revision_id, metric_id)
);
CREATE UNIQUE INDEX benchmark_metric_one_primary
    ON benchmark_metric_definitions(baseline_revision_id)
    WHERE role = 'primary';

CREATE TABLE benchmark_case_weights (
    baseline_revision_id TEXT NOT NULL REFERENCES baseline_revisions(id),
    case_id TEXT NOT NULL,
    weight REAL NOT NULL CHECK(weight > 0),
    ordinal INTEGER NOT NULL CHECK(ordinal >= 0),
    PRIMARY KEY(baseline_revision_id, case_id),
    UNIQUE(baseline_revision_id, ordinal)
);

CREATE TABLE benchmark_measurement_sets (
    id TEXT PRIMARY KEY,
    baseline_revision_id TEXT NOT NULL REFERENCES baseline_revisions(id),
    work_id TEXT REFERENCES works(id),
    integration_id TEXT REFERENCES integrations(id),
    best_sequence INTEGER,
    kind TEXT NOT NULL CHECK(kind IN ('development_baseline', 'reference', 'candidate')),
    created_at TEXT NOT NULL
);
CREATE TABLE benchmark_case_values (
    measurement_set_id TEXT NOT NULL REFERENCES benchmark_measurement_sets(id),
    case_id TEXT NOT NULL,
    metric_id TEXT NOT NULL,
    value REAL NOT NULL CHECK(value > 0),
    PRIMARY KEY(measurement_set_id, case_id, metric_id)
);
CREATE TABLE benchmark_derived_comparisons (
    integration_id TEXT NOT NULL REFERENCES integrations(id),
    case_id TEXT NOT NULL DEFAULT '',
    metric_id TEXT NOT NULL,
    reference_value REAL,
    candidate_value REAL,
    speedup REAL NOT NULL CHECK(speedup > 0),
    regression INTEGER NOT NULL CHECK(regression IN (0, 1)),
    regression_fraction REAL NOT NULL CHECK(regression_fraction >= 0),
    aggregate_speedup REAL,
    max_case_speedup REAL,
    max_case_id TEXT,
    PRIMARY KEY(integration_id, case_id, metric_id)
);
`

var schemaMigrations = []struct {
	version int
	sql     string
}{
	{version: 1, sql: schemaV1},
	{version: 2, sql: schemaV2},
	{version: 3, sql: schemaV3},
	{version: 4, sql: schemaV4},
	{version: 5, sql: schemaV5},
	{version: 6, sql: schemaV6},
	{version: 7, sql: schemaV7},
	{version: 8, sql: schemaV8},
	{version: 9, sql: schemaV9},
	{version: 10, sql: schemaV10},
	{version: 11, sql: schemaV11},
	{version: 12, sql: schemaV12},
	{version: 13, sql: schemaV13},
	{version: 14, sql: schemaV14},
	{version: 15, sql: schemaV15},
	{version: 16, sql: schemaV16},
	{version: 17, sql: schemaV17},
	{version: 18, sql: schemaV18},
	{version: 19, sql: schemaV19},
	{version: 20, sql: schemaV20},
}

func migrate(ctx context.Context, db *sql.DB, now string) error {
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS migrations (
        version INTEGER PRIMARY KEY,
        checksum TEXT NOT NULL,
        applied_at TEXT NOT NULL
    )`); err != nil {
		return fmt.Errorf("create migrations table: %w", err)
	}
	var highestVersion int
	if err := db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM migrations`).Scan(&highestVersion); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	if highestVersion > schemaVersion {
		return fmt.Errorf("database schema version %d is newer than supported version %d", highestVersion, schemaVersion)
	}

	for _, migration := range schemaMigrations {
		checksumBytes := sha256.Sum256([]byte(migration.sql))
		checksum := hex.EncodeToString(checksumBytes[:])
		var stored string
		err := db.QueryRowContext(ctx, `SELECT checksum FROM migrations WHERE version = ?`, migration.version).Scan(&stored)
		switch {
		case err == nil:
			if stored != checksum {
				return fmt.Errorf("schema migration %d checksum mismatch", migration.version)
			}
			continue
		case err != sql.ErrNoRows:
			return fmt.Errorf("read schema migration %d: %w", migration.version, err)
		}

		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin schema migration %d: %w", migration.version, err)
		}
		if _, err := tx.ExecContext(ctx, migration.sql); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("apply schema migration %d: %w", migration.version, err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO migrations(version, checksum, applied_at) VALUES (?, ?, ?)`, migration.version, checksum, now); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("record schema migration %d: %w", migration.version, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit schema migration %d: %w", migration.version, err)
		}
	}
	return nil
}
