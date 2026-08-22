defmodule Pika.Agent.RoleOwnershipTest do
  use ExUnit.Case, async: false

  alias Pika.Agent.RoleOwnership

  setup do
    previous = Application.get_env(:pika, :agent_role_owners)
    Application.delete_env(:pika, :agent_role_owners)

    on_exit(fn ->
      if is_nil(previous),
        do: Application.delete_env(:pika, :agent_role_owners),
        else: Application.put_env(:pika, :agent_role_owners, previous)
    end)

    :ok
  end

  test "all completed Role migrations default to Actor ownership" do
    assert RoleOwnership.all() == %{
             "alignment" => :actor,
             "baseline" => :actor,
             "integration" => :actor,
             "integration_followup" => :actor,
             "iteration" => :actor,
             "plan" => :actor,
             "progress_summary" => :actor,
             "setup_merge" => :actor,
             "sync" => :actor
           }
  end

  test "legacy ownership remains available only where the compatibility adapter exists" do
    Application.put_env(:pika, :agent_role_owners, %{
      alignment: :legacy,
      setup_merge: "legacy",
      sync: :legacy,
      iteration: :legacy
    })

    assert RoleOwnership.owner("alignment") == :legacy
    assert RoleOwnership.owner(:setup_merge) == :legacy
    assert RoleOwnership.owner("sync") == :actor
    assert RoleOwnership.owner(:iteration) == :actor
  end
end
