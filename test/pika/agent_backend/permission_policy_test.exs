defmodule Pika.AgentBackend.PermissionPolicyTest do
  use ExUnit.Case, async: true

  alias Pika.AgentBackend.PermissionPolicy

  test "defines backend-specific defaults and rejects cross-provider policies" do
    assert PermissionPolicy.defaults(:codex_app_server) == %{
             approval_policy: "never",
             sandbox_policy: "danger_full_access"
           }

    assert PermissionPolicy.defaults(:cursor_acp) == %{
             approval_policy: "force",
             sandbox_policy: "disabled"
           }

    assert {:ok, "on_request"} =
             PermissionPolicy.parse(:codex_app_server, :approval_policy, "on-request")

    assert {:error, _message} =
             PermissionPolicy.parse(:cursor_acp, :approval_policy, "on_request")
  end

  test "maps Codex policies to thread and turn protocol values" do
    assert PermissionPolicy.codex_approval_policy("on_request") == "on-request"
    assert PermissionPolicy.codex_approvals_reviewer("on_request") == "auto_review"
    assert PermissionPolicy.codex_approval_decision("on_request") == "decline"

    assert PermissionPolicy.codex_legacy_approval_decision("on_request") == %{
             "denied" => %{
               "rejection" => "Pika delegates non-unattended approval to auto review"
             }
           }

    assert PermissionPolicy.codex_approvals_reviewer("never") == nil
    assert PermissionPolicy.codex_approval_decision("never") == "acceptForSession"
    assert PermissionPolicy.codex_legacy_approval_decision("never") == "approved_for_session"
    assert PermissionPolicy.codex_sandbox_mode("danger_full_access") == "danger-full-access"

    assert PermissionPolicy.codex_sandbox_policy("danger_full_access") == %{
             "type" => "dangerFullAccess"
           }

    assert PermissionPolicy.codex_sandbox_mode("workspace_write") == "workspace-write"

    assert PermissionPolicy.codex_sandbox_policy("workspace_write") == %{
             "type" => "workspaceWrite"
           }
  end

  test "maps Cursor policies to explicit CLI switches" do
    assert PermissionPolicy.cursor_cli_args("force", "disabled") == [
             "--force",
             "--sandbox",
             "disabled"
           ]

    assert PermissionPolicy.cursor_cli_args("auto_review", "enabled") == [
             "--auto-review",
             "--sandbox",
             "enabled"
           ]

    options = [
      %{"optionId" => "allow", "kind" => "allow_always"},
      %{"optionId" => "reject", "kind" => "reject_once"}
    ]

    assert PermissionPolicy.cursor_permission_option(options, "force")["optionId"] == "allow"

    assert PermissionPolicy.cursor_permission_option(options, "auto_review")["optionId"] ==
             "reject"
  end
end
