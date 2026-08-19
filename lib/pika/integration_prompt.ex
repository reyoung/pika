defmodule Pika.IntegrationPrompt do
  @moduledoc false

  alias Pika.PromptCatalog

  def render(attempt, context, workspace) do
    PromptCatalog.render(:integration, %{
      attempt: attempt,
      context: Map.drop(context, [:best_metrics]),
      best_worktree: workspace.repo,
      patch_path: Path.join(workspace.root, "artifacts/patches/#{attempt.id}/candidate.patch")
    })
  end
end
