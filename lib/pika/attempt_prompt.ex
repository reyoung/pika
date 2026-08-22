defmodule Pika.AttemptPrompt do
  @moduledoc false

  alias Pika.AttemptStore, as: Store
  alias Pika.PromptCatalog

  def render(role, attempt, context, workspace \\ nil) when role in [:plan, :iteration] do
    history = Store.terminal_history(attempt.campaign_id, context.history_limit)
    history_summary = Pika.AttemptHistorySummary.render(history, workspace: workspace)
    guidance = Store.guidance_for_attempt(attempt.campaign_id, attempt.id, attempt.created_at)
    important_events = Store.important_campaign_events(attempt.campaign_id)

    PromptCatalog.render(role, %{
      attempt: attempt,
      context: context,
      history: history,
      history_summary: history_summary,
      guidance: guidance,
      important_events: important_events
    })
  end
end
