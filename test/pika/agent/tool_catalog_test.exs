defmodule Pika.Agent.ToolCatalogTest do
  use ExUnit.Case, async: true

  alias Pika.Agent.ToolCatalog
  alias Pika.Optimization.RoleRegistry

  test "matches every Role registry operation and requires idempotency for commands" do
    assert :ok = ToolCatalog.validate()

    for {role_id, role} <- RoleRegistry.all() do
      assert {:ok, tools} = ToolCatalog.for_role(role_id)
      assert Enum.map(tools, & &1["name"]) == Enum.map(role.tools, & &1.name)

      by_name = Map.new(tools, &{&1["name"], &1})

      for tool <- role.tools do
        schema = by_name[tool.name]["inputSchema"]
        assert schema["additionalProperties"] == false

        if tool.kind == :command do
          assert "idempotency_key" in schema["required"]
          assert schema["properties"]["idempotency_key"]["minLength"] == 1
        else
          refute "idempotency_key" in (schema["required"] || [])
        end
      end
    end
  end

  test "ask_questions is a bounded batch contract" do
    assert {:ok, tools} = ToolCatalog.for_role("baseline_alignment")
    questions = Enum.find(tools, &(&1["name"] == "ask_questions"))
    items = questions["inputSchema"]["properties"]["questions"]
    assert items["minItems"] == 1
    assert items["items"]["properties"]["options"]["minItems"] == 2
    assert items["items"]["properties"]["options"]["maxItems"] == 4
  end
end
