defmodule Pika.Optimization.Bootstrap do
  @moduledoc "Synchronously migrates and recovers the singleton v2 Optimization before schedulers start."

  use GenServer

  alias Pika.Baseline.Workspace, as: BaselineWorkspace
  alias Pika.Optimization.{Config, Persistence}
  alias Pika.Git

  def start_link(opts \\ []) do
    case Keyword.get(opts, :name, __MODULE__) do
      nil -> GenServer.start_link(__MODULE__, opts)
      name -> GenServer.start_link(__MODULE__, opts, name: name)
    end
  end

  def snapshot(server \\ __MODULE__), do: GenServer.call(server, :snapshot)

  @impl true
  def init(opts) do
    config_path = Keyword.fetch!(opts, :config_path)

    with {:ok, config} <- Config.load(config_path),
         :ok <- Persistence.migrate(),
         {:ok, initial_sha} <- initial_sha(config),
         {:ok, optimization, recovery} <- Persistence.initialize_or_recover(config, initial_sha),
         {:ok, baseline} <- maybe_prepare_baseline(config, optimization) do
      {:ok, %{config: config, optimization: optimization, recovery: recovery, baseline: baseline}}
    else
      {:error, reason} -> {:stop, {:v2_bootstrap_failed, reason}}
    end
  end

  @impl true
  def handle_call(:snapshot, _from, state), do: {:reply, state, state}

  defp initial_sha(config) do
    case Persistence.current() do
      %{initial_sha: sha} -> {:ok, sha}
      nil -> Git.run(config.repo, ["rev-parse", "HEAD"])
    end
  end

  defp maybe_prepare_baseline(config, %{status: "aligning_baseline"}),
    do: BaselineWorkspace.ensure_current(config)

  defp maybe_prepare_baseline(_config, _optimization), do: {:ok, nil}
end
