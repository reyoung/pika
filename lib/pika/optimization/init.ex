defmodule Pika.Optimization.Init do
  @moduledoc "Creates a new singleton v2 Workspace from fully expanded Role configurations."

  alias Pika.FileSystem
  alias Pika.Optimization.{Config, ConfigFile}
  alias Pika.Git

  @spec run(Path.t(), Path.t(), keyword()) :: {:ok, Config.t()} | {:error, term()}
  def run(workspace, repo, opts \\ []) do
    workspace = Path.expand(workspace)
    repo = Path.expand(repo)
    config_path = Path.join(workspace, "pika.yaml")

    with :ok <- validate_new_backends(opts),
         :ok <- validate_repo(repo),
         :ok <- prepare_workspace(workspace, config_path),
         contents <- ConfigFile.new(config_path, repo, workspace, opts) |> ConfigFile.render(),
         :ok <- FileSystem.atomic_write(config_path, contents),
         :ok <- File.chmod(config_path, 0o600),
         {:ok, config} <- Config.load(config_path) do
      {:ok, config}
    end
  end

  defp validate_new_backends(opts) do
    deprecated_option? =
      Keyword.get(opts, :backend) in [:cursor, :cursor_acp, "cursor", "cursor_acp"]

    configured_agents =
      opts
      |> Keyword.get(:agents, %{})
      |> Map.values()
      |> List.flatten()
      |> Enum.reject(&is_nil/1)

    if deprecated_option? or Enum.any?(configured_agents, &legacy_endpoint?/1),
      do: {:error, :cursor_acp_deprecated},
      else: :ok
  end

  defp legacy_endpoint?(%Config.Agent{backend: :cursor_acp}), do: true

  defp legacy_endpoint?(%Config.Agent{fallbacks: fallbacks}),
    do: Enum.any?(fallbacks, &legacy_endpoint?/1)

  defp legacy_endpoint?(_value), do: false

  defp validate_repo(repo) do
    cond do
      not File.dir?(repo) ->
        {:error, {:repo_not_directory, repo}}

      not (File.dir?(Path.join(repo, ".git")) or File.regular?(Path.join(repo, ".git"))) ->
        {:error, {:repo_not_git, repo}}

      not Git.clean?(repo) ->
        {:error, {:repo_not_clean, repo}}

      true ->
        :ok
    end
  end

  defp prepare_workspace(workspace, config_path) do
    with :ok <- File.mkdir_p(workspace) do
      if File.exists?(config_path),
        do: {:error, {:v2_config_already_exists, config_path}},
        else: :ok
    end
  end
end
