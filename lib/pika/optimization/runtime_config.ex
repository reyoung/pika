defmodule Pika.Optimization.RuntimeConfig do
  @moduledoc "Reloads v2 YAML for each new Backend Session without Profile indirection."

  alias Pika.Optimization.{Config, RoleRegistry}

  @spec load(Path.t() | %{required(:root) => Path.t()}) ::
          {:ok, Config.t()} | {:error, {:runtime_config_reload_failed, term()}}
  def load(%{root: root}), do: load(root)

  def load(workspace_root) when is_binary(workspace_root) do
    path = Path.join(workspace_root, "pika.yaml")

    case Config.load(path) do
      {:ok, config} -> {:ok, config}
      {:error, reason} -> {:error, {:runtime_config_reload_failed, reason}}
    end
  end

  @spec backend_configs(Path.t() | map(), atom() | String.t()) ::
          {:ok, [Config.Agent.t()]} | {:error, term()}
  def backend_configs(workspace, role_id) do
    with {:ok, config} <- load(workspace),
         {:ok, definition} <- RoleRegistry.fetch(role_id) do
      {:ok, RoleRegistry.backend_configs(definition, config)}
    end
  end

  @spec concurrency(Path.t() | map(), atom() | String.t()) ::
          {:ok, non_neg_integer()} | {:error, term()}
  def concurrency(workspace, role_id) do
    with {:ok, configs} <- backend_configs(workspace, role_id), do: {:ok, length(configs)}
  end
end
