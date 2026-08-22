defmodule Pika.Agent.RoleOwnership do
  @moduledoc """
  Compatibility selector for moving one built-in Role at a time between legacy and Actor execution.

  The selector controls only process ownership. Domain eligibility and committed state remain in the
  existing lifecycle modules on both paths.

  The completed migrations always use Actors. Only the three Alignment lifecycle roles retain an
  in-process legacy adapter for rolling recovery of pre-Role workspaces; asking for `:legacy` on a
  fully migrated role is therefore safely normalized back to `:actor` instead of stranding Work.
  """

  @defaults %{
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

  @legacy_capable ~w(alignment setup_merge baseline)

  def all do
    configured = Application.get_env(:pika, :agent_role_owners, %{})

    Enum.reduce(configured, @defaults, fn {role_id, owner}, owners ->
      role_id = to_string(role_id)
      Map.put(owners, role_id, normalize(role_id, owner))
    end)
  end

  def owner(role_id), do: Map.get(all(), to_string(role_id), :legacy)
  def actor?(role_id), do: owner(role_id) == :actor

  defp normalize(role_id, owner)
       when owner in [:legacy, "legacy"] and role_id in @legacy_capable,
       do: :legacy

  defp normalize(_role_id, _owner), do: :actor
end
