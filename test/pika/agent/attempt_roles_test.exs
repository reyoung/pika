defmodule Pika.Agent.AttemptRolesTest do
  use ExUnit.Case, async: true

  alias Pika.Agent.{Completion, RoleRegistry, Roles}

  test "Plan and Iteration expose separate Role contracts for the same Attempt work kind" do
    assert {:ok, plan} = RoleRegistry.fetch("plan")
    assert {:ok, iteration} = RoleRegistry.fetch("iteration")

    assert plan.definition().work_kind == :attempt
    assert iteration.definition().work_kind == :attempt
    assert plan.definition().profile_key == "iteration_agents"
    assert iteration.definition().profile_key == "iteration_agents"
    assert :ok = Roles.validate_definition(plan, plan.definition())
    assert :ok = Roles.validate_definition(iteration, iteration.definition())

    plan_tools = Enum.map(plan.definition().tools, & &1.name)
    iteration_tools = Enum.map(iteration.definition().tools, & &1.name)

    assert "submit_plan" in plan_tools
    refute "record_metrics" in plan_tools
    assert "record_metrics" in iteration_tools
    assert "reject_attempt" in iteration_tools
    refute "submit_plan" in iteration_tools
  end

  test "Iteration completion supports normal completion and direct rejection" do
    {:ok, iteration} = RoleRegistry.fetch("iteration")
    graph = iteration.definition().completion

    progress =
      Completion.evaluate(graph, %{
        needs_metrics: true,
        needs_summary: true,
        needs_completion: true,
        _revision: 1
      })

    assert progress.required_operations == [
             "record_metrics",
             "submit_attempt_summary",
             "complete_attempt"
           ]

    assert Completion.evaluate(graph, %{needs_reject: true, _revision: 2}).required_operations ==
             ["reject_attempt"]

    assert Completion.evaluate(graph, %{attempt_status: "ready_for_integration", _revision: 3}).state ==
             {:terminal, :completed}

    assert Completion.evaluate(graph, %{attempt_status: "rejected", _revision: 4}).state ==
             {:terminal, :rejected}
  end
end
