defmodule Pika.ProgressSummary.PromptInput do
  @moduledoc "Builds the v2 Progress Summary Prompt from one frozen Summary Request."

  alias Pika.Agent.RolePrompts.ProgressSummary
  alias Pika.Agent.RolePrompts.ProgressSummary.Input
  alias Pika.ProgressSummary.Lifecycle

  @spec build(pos_integer(), Path.t()) :: {:ok, Input.t()} | {:error, term()}
  def build(request_id, context_file) do
    with {:ok, request} <- Lifecycle.fetch(request_id),
         :ok <- require_active(request) do
      root = Lifecycle.workdir(request)
      previous = Path.join(root, "previous-summary.md")

      {:ok,
       %Input{
         context_file: context_file,
         status_file: Path.join(root, "status.json"),
         messages_file: Path.join(root, "messages.jsonl"),
         summary_workdir: root,
         previous_summary_file: if(File.regular?(previous), do: previous, else: nil)
       }}
    end
  end

  @spec render(pos_integer(), Path.t()) :: {:ok, String.t()} | {:error, term()}
  def render(request_id, context_file) do
    with {:ok, input} <- build(request_id, context_file),
         do: ProgressSummary.system_prompt(input)
  end

  defp require_active(%{status: status}) when status in ["requested", "running"], do: :ok

  defp require_active(request),
    do: {:error, {:progress_summary_request_not_active, request.id, request.status}}
end
