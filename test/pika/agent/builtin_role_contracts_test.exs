defmodule Pika.Agent.BuiltinRoleContractsTest do
  use ExUnit.Case, async: true

  alias Pika.Agent.{RoleRegistry, Roles}

  test "every built-in Role implements the shared contract" do
    assert Map.keys(RoleRegistry.all()) |> Enum.sort() ==
             ~w(alignment baseline integration iteration plan progress_summary setup_merge sync)

    Enum.each(RoleRegistry.all(), fn {role_id, role} ->
      definition = role.definition()
      assert definition.id == role_id
      assert :ok = Roles.validate_definition(role, definition)
    end)
  end

  test "Alignment lifecycle phases have separate activation and least-privilege catalogs" do
    alignment = definition("alignment")
    setup_merge = definition("setup_merge")
    baseline = definition("baseline")

    assert alignment.activation == :await_user_kickoff
    assert setup_merge.activation == :automatic
    assert baseline.activation == :automatic

    assert tool_names(alignment) ==
             MapSet.new(
               ~w(get_context ask_questions register_artifact submit_spec submit_harness submit_implementation_bundle submit_implementation_review)
             )

    assert tool_names(setup_merge) == MapSet.new(~w(get_context complete_setup_merge))

    assert tool_names(baseline) ==
             MapSet.new(
               ~w(get_context register_artifact reopen_baseline_definition submit_baseline submit_iteration_sample)
             )
  end

  defp definition(role_id) do
    assert {:ok, role} = RoleRegistry.fetch(role_id)
    role.definition()
  end

  defp tool_names(definition),
    do: definition.tools |> Enum.map(& &1.name) |> MapSet.new()
end
