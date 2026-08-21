defmodule Pika.Persistence.AgentSession do
  use Ecto.Schema

  @primary_key {:id, Ecto.UUID, autogenerate: false}
  @timestamps_opts false

  schema "agent_sessions" do
    field(:campaign_id, Ecto.UUID)
    field(:attempt_id, :string)
    field(:sync_run_id, :string)
    field(:role, :string)
    field(:slot_index, :integer)
    field(:profile_json, :string)
    field(:backend, :string)
    field(:backend_protocol, :string)
    field(:backend_version, :string)
    field(:provider_session_id, :string)
    field(:backend_capabilities_json, :string)
    field(:model, :string)
    field(:reasoning_effort, :string)
    field(:status, :string)
    field(:process_pid, :string)
    field(:process_started_at, :integer)
    field(:mcp_token_hash, :string)
    field(:log_artifact_id, Ecto.UUID)
    field(:required_operations_json, :string)
    field(:work_kind, :string)
    field(:work_id, :string)
    field(:role_contract_revision, :integer)
    field(:session_mode, :string)
    field(:role_definition_sha256, :string)
    field(:template_sha256, :string)
    field(:instructions_sha256, :string)
    field(:instructions_artifact_id, Ecto.UUID)
    field(:last_turn_sequence, :integer)
    field(:last_event_seq, :integer)
    field(:started_at, :integer)
    field(:ended_at, :integer)
  end
end
