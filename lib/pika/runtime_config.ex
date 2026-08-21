defmodule Pika.RuntimeConfig do
  @moduledoc false

  alias Pika.Config

  def mutable(workspace) do
    with {:ok, config} <- load(workspace), do: {:ok, config.campaign}
  end

  def alignment_profile(workspace) do
    with {:ok, config} <- load(workspace), do: {:ok, config.backend}
  end

  defp load(workspace) do
    path = Path.join(workspace.root, "pika.yaml")
    opts = [workspace: workspace.root] ++ repo_opt(workspace)

    case Config.load(path, opts) do
      {:ok, config} -> {:ok, config}
      {:error, reason} -> {:error, {:runtime_config_reload_failed, reason}}
    end
  end

  def profile(workspace, key) do
    with {:ok, mutable} <- mutable(workspace),
         profile when is_map(profile) <- mutable[key] do
      {:ok, profile}
    else
      nil -> {:error, {:runtime_profile_missing, key}}
      {:error, _} = error -> error
    end
  end

  defp repo_opt(%{mode: :managed_repo, managed_repo: repo}) when is_binary(repo), do: [repo: repo]
  defp repo_opt(_workspace), do: []
end
