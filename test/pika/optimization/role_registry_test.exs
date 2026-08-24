defmodule Pika.Optimization.RoleRegistryTest do
  use ExUnit.Case, async: true

  alias Pika.Optimization.RoleRegistry

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

  test "defines only the eight v2 Roles" do
    assert RoleRegistry.all() |> Map.keys() |> Enum.sort() == Enum.sort(@role_ids)

    for removed <- ~w(campaign alignment baseline plan setup_merge sync) do
      assert {:error, {:unknown_agent_role, ^removed}} = RoleRegistry.fetch(removed)
    end
  end

  test "binds prompts, tools, activation, and completion commands" do
    assert :ok = RoleRegistry.validate()

    assert {:ok, alignment} = RoleRegistry.fetch(:baseline_alignment)
    assert alignment.activation == :await_user_kickoff

    assert Enum.map(alignment.tools, & &1.name) ==
             ~w(get_context ask_questions submit_baseline_definition)

    assert alignment.terminal_commands == ["submit_baseline_definition"]

    assert {:ok, integration} = RoleRegistry.fetch(:integration)
    assert integration.terminal_commands == ["finish_integration"]

    assert Enum.map(integration.tools, & &1.name) ==
             ~w(get_context prepare_best_update finish_integration)

    assert {:ok, iteration} = RoleRegistry.fetch(:iteration)
    assert iteration.concurrency == :iteration_agents
    assert iteration.optional? == false

    assert {:ok, progress} = RoleRegistry.fetch(:progress_summary)
    assert progress.optional?
  end
end
