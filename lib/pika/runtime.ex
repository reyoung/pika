defmodule Pika.Runtime do
  @moduledoc false

  use GenServer

  alias Pika.Persistence.Campaign
  alias Pika.{ArtifactStore, Persistence, Workspace, WorkspaceLock}

  def start_link(_opts), do: GenServer.start_link(__MODULE__, :ok, name: __MODULE__)

  def snapshot, do: GenServer.call(__MODULE__, :snapshot)

  @impl true
  def init(:ok) do
    workspace = WorkspaceLock.workspace()
    preflight = Application.fetch_env!(:pika, :preflight)

    with :ok <- Persistence.migrate(),
         current <- Persistence.current_campaign(),
         :ok <- verify_git(workspace, current),
         :ok <- verify_artifacts(workspace, current),
         {:ok, campaign, recovery} <- Persistence.initialize_or_recover(workspace) do
      Phoenix.PubSub.subscribe(Pika.PubSub, Persistence.topic(campaign.id))

      {:ok,
       %{
         workspace: workspace,
         campaign: campaign,
         recovery: recovery,
         preflight: preflight,
         sqlite: Persistence.pragma_values()
       }}
    else
      {:blocked, %Campaign{} = campaign, reason} ->
        Phoenix.PubSub.subscribe(Pika.PubSub, Persistence.topic(campaign.id))

        {:ok,
         %{
           workspace: workspace,
           campaign: campaign,
           recovery: {:blocked, reason},
           preflight: preflight,
           sqlite: Persistence.pragma_values()
         }}

      {:error, reason} ->
        {:stop, reason}
    end
  end

  @impl true
  def handle_call(:snapshot, _from, state), do: {:reply, public_snapshot(state), state}

  @impl true
  def handle_info({:domain_event, _event}, state) do
    campaign = Persistence.current_campaign()
    {:noreply, %{state | campaign: campaign}}
  end

  defp verify_git(workspace, current) do
    expected_head = if current, do: current.best_sha, else: workspace.base_sha

    case Workspace.verify_identity(workspace, expected_head) do
      :ok ->
        :ok

      {:error, reason} when is_nil(current) ->
        {:error, reason}

      {:error, reason} ->
        case Persistence.block_campaign(current, reason) do
          {:ok, blocked, _recovery} -> {:blocked, blocked, reason}
          {:error, block_reason} -> {:error, block_reason}
        end
    end
  end

  defp verify_artifacts(workspace, current) do
    case ArtifactStore.verify_all(workspace) do
      :ok ->
        :ok

      {:error, reason} when is_nil(current) ->
        {:error, reason}

      {:error, reason} ->
        case Persistence.block_campaign(current, reason) do
          {:ok, blocked, _recovery} -> {:blocked, blocked, reason}
          {:error, block_reason} -> {:error, block_reason}
        end
    end
  end

  defp public_snapshot(state) do
    campaign = state.campaign

    %{
      workspace: %{
        root: state.workspace.root,
        repo: state.workspace.repo,
        mode: state.workspace.mode,
        config_hash: state.workspace.config_hash
      },
      campaign: %{
        id: campaign.id,
        status: campaign.status,
        base_sha: campaign.base_sha,
        best_sha: campaign.best_sha,
        best_branch: campaign.best_branch,
        attempts_created: campaign.attempts_created
      },
      recovery: public_recovery(state.recovery),
      preflight: state.preflight,
      sqlite: state.sqlite
    }
  end

  defp public_recovery(:initialized), do: "initialized"
  defp public_recovery(:recovered), do: "recovered"

  defp public_recovery({:blocked, reason}),
    do: %{"status" => "blocked", "reason" => inspect(reason)}

  defp public_recovery(value), do: inspect(value)
end
