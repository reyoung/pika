defmodule Pika.Agent.IntegrationRoleTest do
  use ExUnit.Case, async: true

  alias Pika.Agent.{Completion, RoleRegistry, Roles}

  test "registers Integration as an automatic Attempt-scoped Role" do
    assert {:ok, role} = RoleRegistry.fetch("integration")
    definition = role.definition()

    assert definition.id == "integration"
    assert definition.work_kind == :attempt
    assert definition.activation == :automatic
    assert definition.profile_key == "integration_agent"
    assert definition.max_followups == 50
    assert :ok = Roles.validate_definition(role, definition)

    assert Enum.map(definition.tools, &{&1.name, &1.kind}) == [
             {"get_integration_context", :query},
             {"register_artifact", :command},
             {"acquire_integration_lease", :command},
             {"complete_refresh", :command},
             {"submit_fast_rejection", :command},
             {"submit_full_regression", :command},
             {"reject_attempt", :command},
             {"create_merge_intent", :command},
             {"complete_merge", :command}
           ]
  end

  test "projects committed Integration facts into one next operation or a terminal outcome" do
    {:ok, role} = RoleRegistry.fetch("integration")
    graph = role.definition().completion

    assert Completion.evaluate(graph, %{needs_acquire: true, _revision: 1}).required_operations ==
             ["acquire_integration_lease"]

    assert Completion.evaluate(graph, %{needs_refresh: true, _revision: 2}).required_operations ==
             ["complete_refresh"]

    assert Completion.evaluate(graph, %{needs_regression: true, _revision: 3}).required_operations ==
             ["submit_full_regression"]

    assert Completion.evaluate(graph, %{needs_reject: true, _revision: 4}).required_operations ==
             ["reject_attempt"]

    assert Completion.evaluate(graph, %{needs_intent: true, _revision: 5}).required_operations ==
             ["create_merge_intent"]

    assert Completion.evaluate(graph, %{needs_merge: true, _revision: 6}).required_operations ==
             ["complete_merge"]

    assert Completion.evaluate(graph, %{attempt_status: "accepted", _revision: 7}).state ==
             {:terminal, :accepted}

    assert Completion.evaluate(graph, %{attempt_status: "rejected", _revision: 8}).state ==
             {:terminal, :rejected}

    assert Completion.evaluate(graph, %{campaign_status: "blocked", _revision: 9}).state ==
             {:terminal, :blocked}
  end
end
