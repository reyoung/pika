defmodule Pika.Repo.V2Migrations.CreateOptimizationSchema do
  use Ecto.Migration

  def change do
    create table(:optimizations, primary_key: false) do
      add(:id, :string, primary_key: true)
      add(:singleton_key, :integer, null: false, default: 1)
      add(:status, :string, null: false)
      add(:resume_status, :string)
      add(:repo_canonical_path, :string, null: false)
      add(:workspace_canonical_path, :string, null: false)
      add(:initial_sha, :string, null: false)
      add(:best_branch, :string, null: false, default: "pika/best")
      add(:best_sha, :string)
      add(:stop_reason, :text)
      add(:config_sha256, :string, null: false)
      add(:next_attempt_id, :integer, null: false, default: 1)
      add(:progress_summary_due, :boolean, null: false, default: false)
      add(:progress_summary_next_due_at, :integer)
      add(:inserted_at, :integer, null: false)
      add(:updated_at, :integer, null: false)
    end

    create(unique_index(:optimizations, [:singleton_key]))

    create table(:artifacts, primary_key: false) do
      add(:id, :string, primary_key: true)

      add(:optimization_id, references(:optimizations, type: :string, on_delete: :delete_all),
        null: false
      )

      add(:owner_type, :string, null: false)
      add(:owner_id, :string, null: false)
      add(:kind, :string, null: false)
      add(:relative_path, :string, null: false)
      add(:sha256, :string, null: false)
      add(:byte_size, :integer, null: false)
      add(:mime_type, :string, null: false)
      add(:metadata_json, :text, null: false, default: "{}")
      add(:created_at, :integer, null: false)
    end

    create(unique_index(:artifacts, [:optimization_id, :relative_path]))
    create(index(:artifacts, [:optimization_id, :owner_type, :owner_id, :kind]))

    create table(:baseline_revisions, primary_key: false) do
      add(:id, :integer, primary_key: true, autogenerate: true)

      add(:optimization_id, references(:optimizations, type: :string, on_delete: :delete_all),
        null: false
      )

      add(:revision, :integer, null: false)
      add(:status, :string, null: false)
      add(:work_relative_path, :string, null: false)
      add(:definition_artifact_id, references(:artifacts, type: :string, on_delete: :nilify_all))
      add(:definition_sha256, :string)
      add(:dependencies_sha256, :string)
      add(:development_sha, :string)
      add(:target_snapshot_id, :string)
      add(:review_feedback, :text)
      add(:terminal_reason, :text)
      add(:inserted_at, :integer, null: false)
      add(:updated_at, :integer, null: false)
    end

    create(unique_index(:baseline_revisions, [:optimization_id, :revision]))
    create(index(:baseline_revisions, [:optimization_id, :status]))

    create table(:target_snapshots, primary_key: false) do
      add(:id, :string, primary_key: true)

      add(:optimization_id, references(:optimizations, type: :string, on_delete: :delete_all),
        null: false
      )

      add(:baseline_revision_id, references(:baseline_revisions, on_delete: :delete_all),
        null: false
      )

      add(:provenance_json, :text, null: false)
      add(:relative_path, :string, null: false)

      add(:root_artifact_id, references(:artifacts, type: :string, on_delete: :restrict),
        null: false
      )

      add(:digest, :string, null: false)
      add(:entrypoint, :string, null: false)
      add(:created_at, :integer, null: false)
    end

    create(unique_index(:target_snapshots, [:baseline_revision_id]))
    create(index(:target_snapshots, [:optimization_id, :digest]))

    create table(:baseline_reviews, primary_key: false) do
      add(:id, :integer, primary_key: true, autogenerate: true)

      add(:optimization_id, references(:optimizations, type: :string, on_delete: :delete_all),
        null: false
      )

      add(:baseline_revision_id, references(:baseline_revisions, on_delete: :delete_all),
        null: false
      )

      add(:decision, :string, null: false)
      add(:definition_sha256, :string, null: false)
      add(:development_sha, :string, null: false)
      add(:dependencies_sha256, :string, null: false)
      add(:feedback, :text)
      add(:created_at, :integer, null: false)
    end

    create(index(:baseline_reviews, [:baseline_revision_id, :created_at]))

    create table(:baseline_question_batches, primary_key: false) do
      add(:id, :string, primary_key: true)

      add(:optimization_id, references(:optimizations, type: :string, on_delete: :delete_all),
        null: false
      )

      add(:baseline_revision_id, references(:baseline_revisions, on_delete: :delete_all),
        null: false
      )

      add(:session_id, :string)
      add(:status, :string, null: false)
      add(:questions_json, :text, null: false)
      add(:answers_json, :text)
      add(:created_at, :integer, null: false)
      add(:answered_at, :integer)
    end

    create(index(:baseline_question_batches, [:optimization_id, :status, :created_at]))

    create(
      unique_index(:baseline_question_batches, [:baseline_revision_id],
        name: :baseline_question_batches_one_pending,
        where: "status = 'pending'"
      )
    )

    create table(:baseline_verifications, primary_key: false) do
      add(:id, :integer, primary_key: true, autogenerate: true)

      add(:optimization_id, references(:optimizations, type: :string, on_delete: :delete_all),
        null: false
      )

      add(:baseline_revision_id, references(:baseline_revisions, on_delete: :delete_all),
        null: false
      )

      add(:result_artifact_id, references(:artifacts, type: :string, on_delete: :restrict),
        null: false
      )

      add(:outcome, :string, null: false)
      add(:verify_artifact_id, references(:artifacts, type: :string, on_delete: :nilify_all))
      add(:benchmark_artifact_id, references(:artifacts, type: :string, on_delete: :nilify_all))
      add(:development_sha, :string, null: false)
      add(:statistics_json, :text, null: false, default: "[]")
      add(:requested_changes_json, :text, null: false, default: "[]")
      add(:created_at, :integer, null: false)
    end

    create(index(:baseline_verifications, [:baseline_revision_id, :created_at]))

    create table(:benchmark_cases, primary_key: false) do
      add(:baseline_revision_id, references(:baseline_revisions, on_delete: :delete_all),
        primary_key: true
      )

      add(:case_id, :integer, primary_key: true)
      add(:name, :string, null: false)
      add(:description, :text)
      add(:inputs_json, :text, null: false)
      add(:weight, :float, null: false)
      add(:critical, :boolean, null: false, default: false)
    end

    create table(:metric_definitions, primary_key: false) do
      add(:baseline_revision_id, references(:baseline_revisions, on_delete: :delete_all),
        primary_key: true
      )

      add(:metric_id, :string, primary_key: true)
      add(:unit, :string, null: false)
      add(:direction, :string, null: false)
      add(:role, :string, null: false)
      add(:aggregation_json, :text, null: false, default: "{}")
    end

    create table(:sampling_revisions, primary_key: false) do
      add(:id, :integer, primary_key: true, autogenerate: true)

      add(:optimization_id, references(:optimizations, type: :string, on_delete: :delete_all),
        null: false
      )

      add(:baseline_revision_id, references(:baseline_revisions, on_delete: :restrict),
        null: false
      )

      add(:sequence, :integer, null: false)
      add(:cause, :string, null: false)
      add(:created_at, :integer, null: false)
    end

    create(unique_index(:sampling_revisions, [:baseline_revision_id, :sequence]))

    create table(:sampling_revision_cases, primary_key: false) do
      add(:sampling_revision_id, references(:sampling_revisions, on_delete: :delete_all),
        primary_key: true
      )

      add(:case_id, :integer, primary_key: true)
      add(:reason, :text, null: false)
    end

    create table(:best_revisions, primary_key: false) do
      add(:id, :integer, primary_key: true, autogenerate: true)

      add(:optimization_id, references(:optimizations, type: :string, on_delete: :delete_all),
        null: false
      )

      add(:sequence, :integer, null: false)
      add(:sha, :string, null: false)
      add(:source_kind, :string, null: false)
      add(:source_attempt_id, :integer)

      add(:baseline_revision_id, references(:baseline_revisions, on_delete: :restrict),
        null: false
      )

      add(:summary, :text, null: false)
      add(:created_at, :integer, null: false)
    end

    create(unique_index(:best_revisions, [:optimization_id, :sequence]))
    create(unique_index(:best_revisions, [:optimization_id, :sha]))

    create table(:best_metrics, primary_key: false) do
      add(:best_revision_id, references(:best_revisions, on_delete: :delete_all),
        primary_key: true
      )

      add(:case_id, :integer, primary_key: true)
      add(:metric_id, :string, primary_key: true)
      add(:target_value, :float, null: false)
      add(:development_value, :float, null: false)
      add(:normalized_ratio, :float, null: false)
      add(:mad, :float, null: false)
      add(:noise_tolerance, :float, null: false)
      add(:valid_pair_count, :integer, null: false)

      add(:source_artifact_id, references(:artifacts, type: :string, on_delete: :restrict),
        null: false
      )
    end

    create table(:guidance_revisions, primary_key: false) do
      add(:id, :integer, primary_key: true, autogenerate: true)

      add(:optimization_id, references(:optimizations, type: :string, on_delete: :delete_all),
        null: false
      )

      add(:sequence, :integer, null: false)
      add(:body, :text, null: false)
      add(:created_at, :integer, null: false)
    end

    create(unique_index(:guidance_revisions, [:optimization_id, :sequence]))

    create table(:attempts, primary_key: false) do
      add(:id, :integer, primary_key: true)

      add(:optimization_id, references(:optimizations, type: :string, on_delete: :delete_all),
        null: false
      )

      add(:status, :string, null: false)
      add(:work_relative_path, :string, null: false)
      add(:branch, :string, null: false)
      add(:slot_index, :integer)
      add(:base_best_revision, :integer, null: false)
      add(:base_sha, :string, null: false)

      add(:sampling_revision_id, references(:sampling_revisions, on_delete: :restrict),
        null: false
      )

      add(:guidance_revision_id, references(:guidance_revisions, on_delete: :restrict))
      add(:current_iteration_round, :integer, null: false, default: 1)
      add(:candidate_sha, :string)
      add(:summary, :text)
      add(:outcome, :string)
      add(:failure_reason, :text)
      add(:inserted_at, :integer, null: false)
      add(:updated_at, :integer, null: false)
    end

    create(index(:attempts, [:optimization_id, :status, :id]))

    create table(:iteration_rounds, primary_key: false) do
      add(:attempt_id, references(:attempts, on_delete: :delete_all), primary_key: true)
      add(:round, :integer, primary_key: true)
      add(:kind, :string, null: false)
      add(:base_sha, :string, null: false)
      add(:agent_session_id, :string)
      add(:result_artifact_id, references(:artifacts, type: :string, on_delete: :nilify_all))
      add(:status, :string, null: false)
      add(:created_at, :integer, null: false)
      add(:completed_at, :integer)
    end

    create table(:attempt_metrics, primary_key: false) do
      add(:attempt_id, references(:attempts, on_delete: :delete_all), primary_key: true)
      add(:case_id, :integer, primary_key: true)
      add(:metric_id, :string, primary_key: true)
      add(:target_value, :float, null: false)
      add(:candidate_value, :float, null: false)
      add(:best_relative_improvement, :float)
      add(:noise_tolerance, :float, null: false)
      add(:valid_pair_count, :integer, null: false)

      add(:source_artifact_id, references(:artifacts, type: :string, on_delete: :restrict),
        null: false
      )
    end

    create table(:integration_runs, primary_key: false) do
      add(:id, :integer, primary_key: true, autogenerate: true)

      add(:optimization_id, references(:optimizations, type: :string, on_delete: :delete_all),
        null: false
      )

      add(:attempt_id, references(:attempts, on_delete: :delete_all), null: false)
      add(:fifo_sequence, :integer, null: false)
      add(:status, :string, null: false)
      add(:expected_best_sha, :string, null: false)
      add(:validation_artifact_id, references(:artifacts, type: :string, on_delete: :nilify_all))
      add(:result_artifact_id, references(:artifacts, type: :string, on_delete: :nilify_all))
      add(:verify_artifact_id, references(:artifacts, type: :string, on_delete: :nilify_all))
      add(:benchmark_artifact_id, references(:artifacts, type: :string, on_delete: :nilify_all))
      add(:validation_sha256, :string)
      add(:statistics_json, :text, null: false, default: "[]")
      add(:judgement_json, :text, null: false, default: "{}")
      add(:sampling_feedback_json, :text, null: false, default: "[]")
      add(:outcome, :string)
      add(:created_at, :integer, null: false)
      add(:updated_at, :integer, null: false)
    end

    create(unique_index(:integration_runs, [:attempt_id]))
    create(unique_index(:integration_runs, [:optimization_id, :fifo_sequence]))
    create(index(:integration_runs, [:optimization_id, :status, :fifo_sequence]))

    create table(:operation_intents, primary_key: false) do
      add(:id, :string, primary_key: true)

      add(:optimization_id, references(:optimizations, type: :string, on_delete: :delete_all),
        null: false
      )

      add(:kind, :string, null: false)
      add(:owner_type, :string, null: false)
      add(:owner_id, :string, null: false)
      add(:state, :string, null: false)
      add(:expected_best_sha, :string)
      add(:candidate_sha, :string)
      add(:validation_receipt_sha256, :string)
      add(:idempotency_key, :string, null: false)
      add(:payload_json, :text, null: false, default: "{}")
      add(:created_at, :integer, null: false)
      add(:updated_at, :integer, null: false)
    end

    create(unique_index(:operation_intents, [:optimization_id, :idempotency_key]))
    create(index(:operation_intents, [:optimization_id, :owner_type, :owner_id, :state]))

    create table(:agent_sessions, primary_key: false) do
      add(:id, :string, primary_key: true)

      add(:optimization_id, references(:optimizations, type: :string, on_delete: :delete_all),
        null: false
      )

      add(:role, :string, null: false)
      add(:work_kind, :string, null: false)
      add(:work_id, :string, null: false)
      add(:session_sequence, :integer, null: false)
      add(:backend_config_json, :text, null: false)
      add(:system_prompt_sha256, :string, null: false)
      add(:context_sha256, :string, null: false)
      add(:provider_session_id, :string)
      add(:status, :string, null: false)
      add(:recovery_sequence, :integer, null: false, default: 0)
      add(:ended_reason, :text)
      add(:started_at, :integer, null: false)
      add(:ended_at, :integer)
    end

    create(
      unique_index(:agent_sessions, [
        :optimization_id,
        :role,
        :work_kind,
        :work_id,
        :session_sequence
      ])
    )

    create(index(:agent_sessions, [:optimization_id, :role, :work_kind, :work_id, :status]))

    create table(:conversation_turns, primary_key: false) do
      add(:id, :integer, primary_key: true, autogenerate: true)

      add(:optimization_id, references(:optimizations, type: :string, on_delete: :delete_all),
        null: false
      )

      add(:role, :string, null: false)
      add(:work_kind, :string, null: false)
      add(:work_id, :string, null: false)

      add(:session_id, references(:agent_sessions, type: :string, on_delete: :restrict),
        null: false
      )

      add(:session_sequence, :integer, null: false)
      add(:turn_sequence, :integer, null: false)
      add(:input_messages_json, :text, null: false, default: "[]")
      add(:output_messages_json, :text, null: false, default: "[]")
      add(:mcp_calls_json, :text, null: false, default: "[]")
      add(:ended_reason, :string)
      add(:partial, :boolean, null: false, default: true)
      add(:started_at, :integer, null: false)
      add(:ended_at, :integer)
    end

    create(unique_index(:conversation_turns, [:session_id, :turn_sequence]))
    create(index(:conversation_turns, [:optimization_id, :role, :work_kind, :work_id, :id]))

    create table(:followup_requests, primary_key: false) do
      add(:id, :integer, primary_key: true, autogenerate: true)

      add(:optimization_id, references(:optimizations, type: :string, on_delete: :delete_all),
        null: false
      )

      add(:target_role, :string, null: false)
      add(:target_work_kind, :string, null: false)
      add(:target_work_id, :string, null: false)

      add(:target_session_id, references(:agent_sessions, type: :string, on_delete: :restrict),
        null: false
      )

      add(:target_followup_sequence, :integer, null: false)
      add(:generator_attempt_sequence, :integer, null: false, default: 0)
      add(:target_max_followups, :integer, null: false)
      add(:generator_max_attempts, :integer, null: false)
      add(:generator_role, :string)
      add(:required_operation, :string, null: false)
      add(:status, :string, null: false)
      add(:message, :text)
      add(:relative_directory, :string, null: false)
      add(:failure_reason, :text)
      add(:created_at, :integer, null: false)
      add(:updated_at, :integer, null: false)
      add(:delivered_at, :integer)
      add(:completed_at, :integer)
    end

    create(index(:followup_requests, [:optimization_id, :target_role, :target_work_id, :status]))

    create(
      unique_index(
        :followup_requests,
        [:optimization_id, :target_role, :target_work_kind, :target_work_id],
        name: :followup_requests_one_active,
        where:
          "status IN ('requested', 'generating', 'generator_running', 'generated', 'delivered', 'target_turn_running')"
      )
    )

    create table(:progress_summary_requests, primary_key: false) do
      add(:id, :integer, primary_key: true, autogenerate: true)

      add(:optimization_id, references(:optimizations, type: :string, on_delete: :delete_all),
        null: false
      )

      add(:sequence, :integer, null: false)
      add(:status, :string, null: false)
      add(:attempt_sequence, :integer, null: false, default: 0)
      add(:max_attempts, :integer, null: false)
      add(:snapshot_cursor, :integer, null: false)
      add(:previous_summary_id, :integer)
      add(:relative_directory, :string, null: false)
      add(:failure_reason, :text)
      add(:created_at, :integer, null: false)
      add(:updated_at, :integer, null: false)
      add(:completed_at, :integer)
    end

    create(unique_index(:progress_summary_requests, [:optimization_id, :sequence]))
    create(index(:progress_summary_requests, [:optimization_id, :status]))

    create(
      unique_index(:progress_summary_requests, [:optimization_id],
        name: :progress_summary_requests_one_active,
        where: "status IN ('preparing', 'requested', 'running')"
      )
    )

    create table(:progress_summaries, primary_key: false) do
      add(:id, :integer, primary_key: true, autogenerate: true)

      add(:optimization_id, references(:optimizations, type: :string, on_delete: :delete_all),
        null: false
      )

      add(:request_id, references(:progress_summary_requests, on_delete: :delete_all),
        null: false
      )

      add(:summary_relative_path, :string, null: false)
      add(:summary_sha256, :string, null: false)
      add(:completed_at, :integer, null: false)
    end

    create(unique_index(:progress_summaries, [:request_id]))

    create table(:operation_receipts, primary_key: false) do
      add(:optimization_id, references(:optimizations, type: :string, on_delete: :delete_all),
        primary_key: true
      )

      add(:role, :string, primary_key: true)
      add(:work_kind, :string, primary_key: true)
      add(:work_id, :string, primary_key: true)
      add(:operation, :string, primary_key: true)
      add(:idempotency_key, :string, primary_key: true)
      add(:request_sha256, :string, null: false)
      add(:response_json, :text, null: false)
      add(:created_at, :integer, null: false)
    end

    create table(:domain_events, primary_key: false) do
      add(:sequence, :integer, primary_key: true, autogenerate: true)
      add(:event_id, :string, null: false)

      add(:optimization_id, references(:optimizations, type: :string, on_delete: :delete_all),
        null: false
      )

      add(:aggregate_type, :string, null: false)
      add(:aggregate_id, :string, null: false)
      add(:event_type, :string, null: false)
      add(:payload_json, :text, null: false, default: "{}")
      add(:created_at, :integer, null: false)
    end

    create(unique_index(:domain_events, [:event_id]))
    create(index(:domain_events, [:optimization_id, :aggregate_type, :aggregate_id, :sequence]))
  end
end
