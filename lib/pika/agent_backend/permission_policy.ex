defmodule Pika.AgentBackend.PermissionPolicy do
  @moduledoc false

  @codex_approval_options [
    %{
      value: "never",
      label: "Never",
      description: "unattended execution; Codex does not request approval"
    },
    %{
      value: "on_request",
      label: "On request",
      description: "Codex may request approval when the Agent asks to escalate"
    },
    %{
      value: "untrusted",
      label: "Untrusted",
      description: "Codex requests approval outside its trusted command set"
    }
  ]

  @codex_sandbox_options [
    %{
      value: "danger_full_access",
      label: "Danger full access",
      description: "no filesystem or network sandbox; Codex hard safety rules still apply"
    },
    %{
      value: "workspace_write",
      label: "Workspace write",
      description: "read broadly and write only within the active workspace"
    },
    %{
      value: "read_only",
      label: "Read only",
      description: "allow reads but prevent workspace writes"
    }
  ]

  @cursor_approval_options [
    %{
      value: "force",
      label: "Force",
      description: "allow commands unless Cursor explicitly denies them"
    },
    %{
      value: "auto_review",
      label: "Auto review",
      description: "let Cursor auto-run safe calls and deny unresolved permission prompts"
    }
  ]

  @cursor_sandbox_options [
    %{
      value: "disabled",
      label: "Disabled",
      description: "run without Cursor's filesystem sandbox"
    },
    %{
      value: "enabled",
      label: "Enabled",
      description: "run inside Cursor's filesystem sandbox"
    }
  ]

  @spec defaults(atom() | String.t()) :: %{
          approval_policy: String.t(),
          sandbox_policy: String.t()
        }
  def defaults(backend) do
    case normalize_backend(backend) do
      :cursor_acp -> %{approval_policy: "force", sandbox_policy: "disabled"}
      :codex_app_server -> %{approval_policy: "never", sandbox_policy: "danger_full_access"}
    end
  end

  @spec options(atom() | String.t(), :approval_policy | :sandbox_policy) :: [map()]
  def options(backend, :approval_policy) do
    case normalize_backend(backend) do
      :cursor_acp -> @cursor_approval_options
      :codex_app_server -> @codex_approval_options
    end
  end

  def options(backend, :sandbox_policy) do
    case normalize_backend(backend) do
      :cursor_acp -> @cursor_sandbox_options
      :codex_app_server -> @codex_sandbox_options
    end
  end

  @spec parse(atom() | String.t(), :approval_policy | :sandbox_policy, term()) ::
          {:ok, String.t()} | {:error, String.t()}
  def parse(backend, kind, value) when kind in [:approval_policy, :sandbox_policy] do
    normalized = normalize_value(value)
    values = Enum.map(options(backend, kind), & &1.value)

    if normalized in values do
      {:ok, normalized}
    else
      {:error, "expected one of: #{Enum.join(values, ", ")}"}
    end
  end

  @spec codex_approval_policy(term()) :: String.t()
  def codex_approval_policy(value) do
    case normalize_value(value) do
      "on_request" -> "on-request"
      "untrusted" -> "untrusted"
      _ -> "never"
    end
  end

  @spec codex_approvals_reviewer(term()) :: String.t() | nil
  def codex_approvals_reviewer(value) do
    case normalize_value(value) do
      policy when policy in ["on_request", "untrusted"] -> "auto_review"
      _ -> nil
    end
  end

  @spec codex_approval_decision(term()) :: String.t()
  def codex_approval_decision(value) do
    case normalize_value(value) do
      policy when policy in ["on_request", "untrusted"] -> "decline"
      _ -> "acceptForSession"
    end
  end

  @spec codex_legacy_approval_decision(term()) :: String.t() | map()
  def codex_legacy_approval_decision(value) do
    case normalize_value(value) do
      policy when policy in ["on_request", "untrusted"] ->
        %{"denied" => %{"rejection" => "Pika delegates non-unattended approval to auto review"}}

      _ ->
        "approved_for_session"
    end
  end

  @spec codex_sandbox_mode(term()) :: String.t()
  def codex_sandbox_mode(value) do
    case normalize_value(value) do
      "read_only" -> "read-only"
      "workspace_write" -> "workspace-write"
      _ -> "danger-full-access"
    end
  end

  @spec codex_sandbox_policy(term()) :: map()
  def codex_sandbox_policy(value) do
    case normalize_value(value) do
      "read_only" -> %{"type" => "readOnly"}
      "workspace_write" -> %{"type" => "workspaceWrite"}
      _ -> %{"type" => "dangerFullAccess"}
    end
  end

  @spec cursor_cli_args(term(), term()) :: [String.t()]
  def cursor_cli_args(approval_policy, sandbox_policy) do
    approval_args =
      case normalize_value(approval_policy) do
        "auto_review" -> ["--auto-review"]
        _ -> ["--force"]
      end

    sandbox =
      case normalize_value(sandbox_policy) do
        "enabled" -> "enabled"
        _ -> "disabled"
      end

    approval_args ++ ["--sandbox", sandbox]
  end

  @spec cursor_permission_option([map()], term()) :: map() | nil
  def cursor_permission_option(options, approval_policy) when is_list(options) do
    case normalize_value(approval_policy) do
      "auto_review" ->
        Enum.find(options, &(Map.get(&1, "kind") in ["reject_once", "reject_always"]))

      _ ->
        Enum.find(options, &(Map.get(&1, "kind") in ["allow_always", "allow_once"])) ||
          List.first(options)
    end
  end

  defp normalize_backend(value) when value in [:cursor_acp, "cursor_acp", "cursor"],
    do: :cursor_acp

  defp normalize_backend(_value), do: :codex_app_server

  defp normalize_value(value) when is_atom(value),
    do: value |> Atom.to_string() |> normalize_value()

  defp normalize_value(value) when is_binary(value) do
    value
    |> String.trim()
    |> String.downcase()
    |> String.replace("-", "_")
  end

  defp normalize_value(_value), do: ""
end
