defmodule Pika.ReferenceRegistry do
  @moduledoc false

  alias Pika.SkillRegistry, as: Skill
  alias Pika.{Git, ReferenceCatalog}

  def references(configured, durable_state) do
    case durable_state do
      %{references: [_ | _] = references} -> {:ok, references}
      _ -> configured_entries(configured) |> ReferenceCatalog.resolve_selected()
    end
  end

  def skill(workspace_root, durable_state) do
    path = Path.join(workspace_root, ".pika/skills/ncu-report-skill")

    case durable_state do
      %{skill: %{sha: sha} = snapshot} when is_binary(sha) ->
        restore_skill(path, snapshot)

      _ ->
        {:ok, Skill.ensure_latest(path)}
    end
  rescue
    error -> {:error, {:skill_registry_failed, Exception.message(error)}}
  end

  defp configured_entries([]), do: ReferenceCatalog.entries()

  defp configured_entries(entries) do
    Enum.map(entries, fn entry ->
      entry = stringify_keys(entry)

      %{
        id: entry["id"],
        url: entry["url"],
        description: entry["description"] || entry["id"],
        selected: Map.get(entry, "selected", true),
        status: :unresolved,
        sha: nil,
        branch: nil
      }
    end)
  end

  defp restore_skill(path, snapshot) do
    with true <- File.dir?(Path.join(path, ".git")),
         {:ok, actual} <- Git.run(path, ["rev-parse", "HEAD"]),
         true <- actual == snapshot.sha do
      {:ok, Map.put(snapshot, :path, path)}
    else
      false -> {:error, {:frozen_skill_missing_or_changed, snapshot.sha}}
      {:error, reason} -> {:error, {:frozen_skill_unreadable, reason}}
    end
  end

  defp stringify_keys(map) when is_map(map),
    do: Map.new(map, fn {key, value} -> {to_string(key), stringify_keys(value)} end)

  defp stringify_keys(list) when is_list(list), do: Enum.map(list, &stringify_keys/1)
  defp stringify_keys(value), do: value
end
