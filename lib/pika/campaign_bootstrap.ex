defmodule Pika.CampaignBootstrap do
  @moduledoc false

  use GenServer

  alias Pika.CampaignStore, as: Store
  alias Pika.CampaignWorkspace, as: Workspace
  alias Pika.ReferenceRegistry, as: Registry
  alias Pika.Alignment.Campaign

  def start_link(_opts), do: GenServer.start_link(__MODULE__, :ok, name: __MODULE__)

  def status, do: GenServer.call(__MODULE__, :status)

  @impl true
  def init(:ok) do
    {:ok, %{status: :starting}, {:continue, :start_campaign}}
  end

  @impl true
  def handle_continue(:start_campaign, state) do
    parent = self()
    Task.start(fn -> send(parent, {:campaign_ready, prepare_and_start()}) end)
    {:noreply, state}
  end

  @impl true
  def handle_call(:status, _from, state), do: {:reply, state.status, state}

  @impl true
  def handle_info({:campaign_ready, {:ok, pid}}, state) do
    Process.monitor(pid)
    {:noreply, %{state | status: :running}}
  end

  def handle_info({:campaign_ready, {:error, reason}}, state),
    do: {:noreply, %{state | status: {:error, reason}}}

  def handle_info({:DOWN, _ref, :process, _pid, reason}, state),
    do: {:noreply, %{state | status: {:error, {:campaign_exited, reason}}}}

  defp prepare_and_start do
    workspace = Pika.WorkspaceLock.workspace()
    campaign = Pika.Persistence.current_campaign()

    with {:ok, durable} <- load_durable(campaign.id),
         {:ok, stage_workspace} <- Workspace.prepare(workspace, campaign, revision(durable)),
         {:ok, references} <- Registry.references(reference_config(workspace), durable),
         {:ok, skill} <- Registry.skill(workspace.root, durable),
         {:ok, pid} <-
           start_campaign(workspace, campaign, stage_workspace, durable, references, skill) do
      {:ok, pid}
    end
  end

  defp start_campaign(workspace, campaign, stage_workspace, durable, references, skill) do
    immutable = workspace.snapshot["immutable"]
    backend = immutable["backend"]
    listen = immutable["listen"]
    host = if listen["host"] in ["0.0.0.0", "::"], do: "127.0.0.1", else: listen["host"]

    backend_name =
      case backend["type"] do
        "cursor_acp" -> :cursor_acp
        _ -> :codex_app_server
      end

    {command, args} = backend_command(backend["type"], backend["command"])

    opts = [
      workspace: stage_workspace,
      campaign_id: campaign.id,
      persistence: Store,
      durable_state: durable,
      backend: backend_name,
      backend_profile: %{
        command: command,
        args: args,
        protocol_config: backend["protocol_config"]
      },
      start_backend: true,
      resolve_references: false,
      materialize_references: true,
      references: references,
      mcp_url: "http://#{host}:#{listen["port"]}/mcp",
      skill: skill
    ]

    DynamicSupervisor.start_child(Pika.CampaignSupervisor, {Campaign, opts})
  end

  defp load_durable(campaign_id) do
    case Store.load(campaign_id) do
      {:ok, durable} -> {:ok, durable}
      :none -> {:ok, nil}
      {:error, _} = error -> error
    end
  end

  defp reference_config(workspace), do: workspace.snapshot["mutable"]["reference_catalog"] || []

  defp revision(%{spec_result: %{spec: %{"revision" => value}}}) when is_integer(value), do: value
  defp revision(_durable), do: 1

  defp backend_command("codex_app_server", [command, "app-server", "--listen", "stdio://"]),
    do: {command, []}

  defp backend_command("cursor_acp", [command, "acp"]), do: {command, []}
  defp backend_command(_type, [command | args]), do: {command, args}
end
