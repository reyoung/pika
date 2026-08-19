defmodule Pika.SyncPrompt do
  @moduledoc false

  def render(run, context) do
    Pika.PromptCatalog.render(:sync, %{
      sync_run: Pika.JSONSafe.json_safe(run),
      context: Pika.JSONSafe.json_safe(context)
    })
  end
end
