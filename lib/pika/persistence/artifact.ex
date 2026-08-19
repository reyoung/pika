defmodule Pika.Persistence.Artifact do
  use Ecto.Schema
  import Ecto.Changeset

  @primary_key {:id, Ecto.UUID, autogenerate: true}
  @timestamps_opts false

  schema "artifacts" do
    field(:campaign_id, Ecto.UUID)
    field(:owner_type, :string)
    field(:owner_id, :string)
    field(:kind, :string)
    field(:relative_path, :string)
    field(:sha256, :string)
    field(:byte_size, :integer)
    field(:mime_type, :string)
    field(:metadata_json, :string, default: "{}")
    field(:created_at, :integer)
  end

  def changeset(artifact, attrs) do
    artifact
    |> cast(attrs, [
      :campaign_id,
      :owner_type,
      :owner_id,
      :kind,
      :relative_path,
      :sha256,
      :byte_size,
      :mime_type,
      :metadata_json,
      :created_at
    ])
    |> validate_required([
      :campaign_id,
      :owner_type,
      :owner_id,
      :kind,
      :relative_path,
      :sha256,
      :byte_size,
      :mime_type,
      :metadata_json,
      :created_at
    ])
    |> validate_format(:sha256, ~r/^[0-9a-f]{64}$/)
    |> validate_number(:byte_size, greater_than_or_equal_to: 0)
    |> unique_constraint(:relative_path)
    |> foreign_key_constraint(:campaign_id)
  end
end
