defmodule Pika.ProgressSummaryStore do
  @moduledoc false

  alias Pika.Repo

  def insert(campaign_id, attrs) do
    id = Ecto.UUID.generate()
    now = System.system_time(:microsecond)
    attempt_ids = List.wrap(attrs.attempt_ids)

    transaction =
      Repo.transaction(fn ->
        Repo.query!(
          "INSERT INTO progress_summaries(id, campaign_id, phase, attempt_ids_json, backend, model, reasoning_effort, content, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)",
          [
            id,
            campaign_id,
            attrs.phase,
            Jason.encode!(attempt_ids),
            attrs.backend,
            attrs.model,
            attrs.reasoning_effort,
            attrs.content,
            now
          ]
        )

        event_id = Ecto.UUID.generate()

        payload = %{
          summary_id: id,
          phase: attrs.phase,
          attempt_ids: attempt_ids,
          content: attrs.content
        }

        [[sequence]] =
          Repo.query!(
            "INSERT INTO domain_events(event_id, aggregate_type, aggregate_id, event_type, payload_json, created_at) VALUES (?, 'campaign', ?, 'progress_summary_created', ?, ?) RETURNING sequence",
            [event_id, campaign_id, Jason.encode!(payload), now]
          ).rows

        {%{id: id, created_at: now},
         %{
           sequence: sequence,
           event_id: event_id,
           aggregate_type: "campaign",
           aggregate_id: campaign_id,
           event_type: "progress_summary_created",
           payload: payload,
           created_at: now,
           campaign_id: campaign_id
         }}
      end)

    case transaction do
      {:ok, {summary, event}} ->
        Phoenix.PubSub.broadcast(
          Pika.PubSub,
          Pika.Persistence.topic(campaign_id),
          {:domain_event, event}
        )

        Phoenix.PubSub.broadcast(
          Pika.PubSub,
          "pika:optimization:#{campaign_id}",
          {:optimization_event, event}
        )

        {:ok, summary}

      {:error, reason} ->
        {:error, reason}
    end
  rescue
    error -> {:error, {:progress_summary_persist_failed, Exception.message(error)}}
  end

  def list(campaign_id, limit \\ 50) do
    Repo.query!(
      "SELECT id, phase, attempt_ids_json, backend, model, reasoning_effort, content, created_at FROM progress_summaries WHERE campaign_id = ? ORDER BY created_at DESC LIMIT ?",
      [campaign_id, limit]
    ).rows
    |> Enum.map(fn [id, phase, attempts, backend, model, effort, content, created_at] ->
      %{
        id: id,
        phase: phase,
        attempt_ids: Jason.decode!(attempts),
        backend: backend,
        model: model,
        reasoning_effort: effort,
        content: content,
        created_at: created_at
      }
    end)
  end
end
