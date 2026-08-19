defmodule Pika.AttemptPrompt do
  @moduledoc false

  alias Pika.AttemptStore, as: Store
  alias Pika.PromptCatalog

  def render(role, attempt, context) when role in [:plan, :iteration] do
    history = Store.terminal_history(attempt.campaign_id, context.history_limit)
    guidance = Store.guidance_for_attempt(attempt.campaign_id, attempt.id, attempt.created_at)
    important_events = Store.important_campaign_events(attempt.campaign_id)

    PromptCatalog.render(role, %{
      attempt: attempt,
      context: context,
      history: history,
      guidance: guidance,
      important_events: important_events
    })
  end
end
