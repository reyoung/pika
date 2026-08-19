defmodule Pika.Persistence.Campaign do
  use Ecto.Schema
  import Ecto.Changeset

  @primary_key {:id, Ecto.UUID, autogenerate: true}
  @timestamps_opts false

  schema "campaigns" do
    field(:singleton_key, :integer, default: 1)
    field(:status, :string)
    field(:resume_state, :string)
    field(:dispatch_gate, :string)
    field(:workspace_mode, :string)
    field(:repo_relative_path, :string, default: "repo")
    field(:managed_repo_canonical_path, :string)
    field(:git_common_dir, :string)
    field(:base_sha, :string)
    field(:best_branch, :string, default: "pika/best")
    field(:best_sha, :string)
    field(:current_spec_revision_id, :string)
    field(:attempts_created, :integer, default: 0)
    field(:max_attempts, :integer)
    field(:plan_enabled, :boolean, default: true)
    field(:history_limit, :integer, default: 10)
    field(:stop_mode, :string, default: "all_goals")
    field(:config_hash, :string)
    field(:lock_version, :integer, default: 1)
    field(:inserted_at, :integer)
    field(:updated_at, :integer)
  end

  def changeset(campaign, attrs) do
    campaign
    |> cast(attrs, [
      :singleton_key,
      :status,
      :resume_state,
      :dispatch_gate,
      :workspace_mode,
      :repo_relative_path,
      :managed_repo_canonical_path,
      :git_common_dir,
      :base_sha,
      :best_branch,
      :best_sha,
      :current_spec_revision_id,
      :attempts_created,
      :max_attempts,
      :plan_enabled,
      :history_limit,
      :stop_mode,
      :config_hash,
      :lock_version,
      :inserted_at,
      :updated_at
    ])
    |> validate_required([
      :singleton_key,
      :status,
      :workspace_mode,
      :repo_relative_path,
      :git_common_dir,
      :base_sha,
      :best_branch,
      :best_sha,
      :attempts_created,
      :plan_enabled,
      :history_limit,
      :stop_mode,
      :config_hash,
      :lock_version,
      :inserted_at,
      :updated_at
    ])
    |> validate_inclusion(
      :status,
      ~w(drafting_spec awaiting_confirmation building_baseline optimizing draining completed paused blocked stopped awaiting_spec_confirmation)
    )
    |> validate_inclusion(:workspace_mode, ~w(owned_repo managed_repo))
    |> validate_inclusion(:stop_mode, ~w(all_goals any_goal))
    |> validate_format(:base_sha, ~r/^(?:[0-9a-f]{40}|[0-9a-f]{64})$/)
    |> validate_format(:best_sha, ~r/^(?:[0-9a-f]{40}|[0-9a-f]{64})$/)
    |> validate_number(:attempts_created, greater_than_or_equal_to: 0)
    |> validate_number(:max_attempts, greater_than_or_equal_to: 0)
    |> validate_number(:history_limit, greater_than_or_equal_to: 0)
    |> validate_format(:config_hash, ~r/^[0-9a-f]{64}$/)
    |> unique_constraint(:singleton_key)
    |> optimistic_lock(:lock_version)
  end
end
