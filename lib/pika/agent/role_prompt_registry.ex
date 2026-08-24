defmodule Pika.Agent.RolePromptRegistry do
  @moduledoc "Compile-time registry of v2 Role-owned system prompt implementations."

  @prompts %{
    "baseline_alignment" => Pika.Agent.RolePrompts.BaselineAlignment,
    "baseline_verify" => Pika.Agent.RolePrompts.BaselineVerify,
    "baseline_verify_followup" => Pika.Agent.RolePrompts.BaselineVerifyFollowup,
    "integration" => Pika.Agent.RolePrompts.Integration,
    "integration_followup" => Pika.Agent.RolePrompts.IntegrationFollowup,
    "iteration" => Pika.Agent.RolePrompts.Iteration,
    "iteration_followup" => Pika.Agent.RolePrompts.IterationFollowup,
    "progress_summary" => Pika.Agent.RolePrompts.ProgressSummary
  }

  @spec all() :: %{String.t() => module()}
  def all, do: @prompts

  @spec fetch(atom() | String.t()) ::
          {:ok, module()} | {:error, {:unknown_role_prompt, String.t()}}
  def fetch(role_id) when is_atom(role_id), do: fetch(Atom.to_string(role_id))

  def fetch(role_id) when is_binary(role_id) do
    case Map.fetch(@prompts, role_id) do
      {:ok, module} -> {:ok, module}
      :error -> {:error, {:unknown_role_prompt, role_id}}
    end
  end

  @spec schemas() ::
          {:ok, Pika.Agent.RolePrompt.Schemas.t()}
          | {:error, {:missing_schema_files, [Path.t()]}}
  def schemas do
    Pika.Agent.RolePrompt.Schemas.from_dir(schema_dir())
  end

  @spec schema_dir() :: Path.t()
  def schema_dir, do: Application.app_dir(:pika, "priv/v2/roles/schemas")

  @spec validate() :: :ok | {:error, term()}
  def validate do
    with :ok <- validate_prompts(),
         {:ok, _schemas} <- schemas() do
      :ok
    end
  end

  @spec validate!() :: :ok
  def validate! do
    case validate() do
      :ok -> :ok
      {:error, reason} -> raise "invalid v2 Role Prompt registry: #{inspect(reason)}"
    end
  end

  defp validate_prompts do
    Enum.reduce_while(@prompts, :ok, fn {role_id, module}, :ok ->
      cond do
        not Code.ensure_loaded?(module) ->
          {:halt, {:error, {:role_prompt_not_loaded, role_id, module}}}

        not function_exported?(module, :system_prompt, 1) ->
          {:halt, {:error, {:role_prompt_callback_missing, role_id, module}}}

        Pika.Agent.RolePrompt not in behaviours(module) ->
          {:halt, {:error, {:role_prompt_behaviour_missing, role_id, module}}}

        true ->
          {:cont, :ok}
      end
    end)
  end

  defp behaviours(module) do
    module.module_info(:attributes)
    |> Keyword.get_values(:behaviour)
    |> List.flatten()
  end
end
