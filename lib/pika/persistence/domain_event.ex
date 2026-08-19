defmodule Pika.Persistence.DomainEvent do
  use Ecto.Schema
  import Ecto.Changeset

  @primary_key {:sequence, :id, autogenerate: true}
  @timestamps_opts false

  schema "domain_events" do
    field(:event_id, Ecto.UUID)
    field(:aggregate_type, :string)
    field(:aggregate_id, :string)
    field(:event_type, :string)
    field(:payload_json, :string, default: "{}")
    field(:created_at, :integer)
  end

  def changeset(event, attrs) do
    event
    |> cast(attrs, [
      :event_id,
      :aggregate_type,
      :aggregate_id,
      :event_type,
      :payload_json,
      :created_at
    ])
    |> validate_required([
      :event_id,
      :aggregate_type,
      :aggregate_id,
      :event_type,
      :payload_json,
      :created_at
    ])
    |> unique_constraint(:event_id)
  end
end
