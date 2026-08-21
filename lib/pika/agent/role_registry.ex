defmodule Pika.Agent.RoleRegistry do
  @moduledoc "Compile-time registry of Pika-owned Agent Role contracts."

  @roles %{
    "alignment" => Pika.Agent.Roles.Alignment,
    "baseline" => Pika.Agent.Roles.Baseline,
    "integration" => Pika.Agent.Roles.Integration,
    "iteration" => Pika.Agent.Roles.Iteration,
    "plan" => Pika.Agent.Roles.Plan,
    "progress_summary" => Pika.Agent.Roles.ProgressSummary,
    "setup_merge" => Pika.Agent.Roles.SetupMerge,
    "sync" => Pika.Agent.Roles.Sync
  }

  def all, do: @roles

  def fetch(role_id) when is_atom(role_id), do: fetch(Atom.to_string(role_id))

  def fetch(role_id) when is_binary(role_id) do
    case Map.fetch(@roles, role_id) do
      {:ok, role} -> {:ok, role}
      :error -> {:error, {:unknown_agent_role, role_id}}
    end
  end

  def validate do
    Enum.reduce_while(@roles, :ok, fn {role_id, role}, :ok ->
      case Pika.Agent.Roles.validate_definition(role, role.definition()) do
        :ok -> {:cont, :ok}
        {:error, reason} -> {:halt, {:error, {:invalid_agent_role, role_id, reason}}}
      end
    end)
  end

  def validate! do
    case validate() do
      :ok -> :ok
      {:error, reason} -> raise "invalid built-in Agent Role: #{inspect(reason)}"
    end
  end
end
