defmodule Pika.Agent.RolePromptRegistryTest do
  use ExUnit.Case, async: true

  alias Pika.Agent.RolePromptRegistry

  @role_ids ~w(
    baseline_alignment
    baseline_verify
    baseline_verify_followup
    integration
    integration_followup
    iteration
    iteration_followup
    progress_summary
  )

  test "registers every v2 Role Prompt implementation" do
    assert RolePromptRegistry.all() |> Map.keys() |> Enum.sort() == Enum.sort(@role_ids)

    Enum.each(@role_ids, fn role_id ->
      assert {:ok, module} = RolePromptRegistry.fetch(role_id)
      assert function_exported?(module, :system_prompt, 1)
    end)

    assert {:error, {:unknown_role_prompt, "missing"}} = RolePromptRegistry.fetch("missing")
  end

  test "validates compiled prompt modules and packaged Schemas" do
    assert :ok = RolePromptRegistry.validate()
    assert {:ok, schemas} = RolePromptRegistry.schemas()

    schemas
    |> Map.from_struct()
    |> Map.values()
    |> Enum.each(&assert(File.regular?(&1)))
  end
end
