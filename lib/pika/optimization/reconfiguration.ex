defmodule Pika.Optimization.Reconfiguration do
  @moduledoc "Interactively updates mutable per-Agent v2 Workspace configuration."

  alias Pika.FileSystem
  alias Pika.Optimization.{Config, ConfigFile, InteractiveConfig}

  @spec run(Path.t(), Path.t(), keyword() | map()) :: {:ok, Config.t()} | {:error, term()}
  def run(workspace, config_path, opts \\ []) do
    workspace = Path.expand(workspace)
    config_path = Path.expand(config_path)
    opts = normalize_opts(opts)

    IO.puts("Pika v2 Workspace reconfiguration")

    with {:ok, config} <- Config.load(config_path),
         :ok <- validate_identity(config, workspace, config_path),
         {:ok, role} <- role(opts),
         {:ok, updated} <- InteractiveConfig.reconfigure(config, role, opts),
         contents <- ConfigFile.render(updated),
         :ok <- validate_candidate(contents),
         :ok <- FileSystem.atomic_write(config_path, contents),
         {:ok, persisted} <- Config.load(config_path) do
      IO.puts("Updated #{config_path}")
      IO.puts("New Agent Sessions will use this configuration; active Sessions are unchanged.")
      {:ok, persisted}
    end
  end

  defp role(opts) do
    cond do
      opts[:all] == true -> {:ok, :all}
      not is_nil(opts[:role]) -> InteractiveConfig.validate_role(opts[:role])
      opts[:yes] == true -> {:error, :role_required_in_non_interactive_mode}
      true -> {:ok, InteractiveConfig.choose_role()}
    end
  end

  defp validate_identity(config, workspace, config_path) do
    cond do
      Path.expand(config.workspace) != workspace ->
        {:error, {:workspace_mismatch, config.workspace, workspace}}

      Path.expand(config.source_path) != config_path ->
        {:error, {:config_path_mismatch, config.source_path, config_path}}

      Path.expand(Path.join(workspace, "pika.yaml")) != config_path ->
        {:error, {:config_must_be_workspace_pika_yaml, workspace}}

      true ->
        :ok
    end
  end

  defp validate_candidate(contents) do
    path =
      Path.join(
        System.tmp_dir!(),
        "pika-reconfiguration-#{Base.url_encode64(:crypto.strong_rand_bytes(12), padding: false)}.yaml"
      )

    try do
      with :ok <- File.write(path, contents),
           {:ok, _config} <- Config.load(path) do
        :ok
      end
    after
      File.rm(path)
    end
  end

  defp normalize_opts(opts) when is_map(opts), do: Map.to_list(opts)
  defp normalize_opts(opts) when is_list(opts), do: opts
end
