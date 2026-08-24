defmodule Pika.Followup.PromptInput do
  @moduledoc "Builds a v2 Follow-up generator Prompt from a frozen Follow-up Request."

  alias Pika.Agent.RolePrompt.FollowupInput
  alias Pika.Agent.RolePromptRegistry
  alias Pika.Followup.Lifecycle

  @spec build(pos_integer(), Path.t()) :: {:ok, module(), FollowupInput.t()} | {:error, term()}
  def build(request_id, context_file) do
    with {:ok, request} <- Lifecycle.fetch(request_id),
         :ok <- require_generator(request),
         {:ok, prompt_module} <- RolePromptRegistry.fetch(request.generator_role) do
      root = Lifecycle.workdir(request)

      {:ok, prompt_module,
       %FollowupInput{
         context_file: context_file,
         target_messages_file: Path.join(root, "messages.jsonl"),
         target_state_file: Path.join(root, "state.json")
       }}
    end
  end

  @spec render(pos_integer(), Path.t()) :: {:ok, String.t()} | {:error, term()}
  def render(request_id, context_file) do
    with {:ok, module, input} <- build(request_id, context_file), do: module.system_prompt(input)
  end

  defp require_generator(%{generator_role: role}) when is_binary(role), do: :ok
  defp require_generator(_request), do: {:error, :followup_generator_not_configured}
end
