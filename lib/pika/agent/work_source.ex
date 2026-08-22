defmodule Pika.Agent.WorkSource do
  @moduledoc "Domain lifecycle seam that projects eligible rows into Agent Work."

  alias Pika.Agent.Role.Work

  @callback runnable_work(String.t(), Pika.Workspace.t()) :: [Work.t()]
end

defmodule Pika.Agent.WorkSources.ProgressSummary do
  @moduledoc false

  @behaviour Pika.Agent.WorkSource

  @impl true
  def runnable_work(campaign_id, _workspace),
    do: Pika.ProgressSummaryStore.runnable_work(campaign_id)
end

defmodule Pika.Agent.WorkSources.AgentFollowup do
  @moduledoc false
  @behaviour Pika.Agent.WorkSource

  @impl true
  def runnable_work(campaign_id, _workspace),
    do: Pika.AgentFollowupStore.runnable_work(campaign_id)
end

defmodule Pika.Agent.WorkSources.Sync do
  @moduledoc false

  @behaviour Pika.Agent.WorkSource

  alias Pika.Agent.Role.Work

  @impl true
  def runnable_work(campaign_id, _workspace) do
    case {campaign_status(campaign_id), Pika.SyncStore.active_run(campaign_id)} do
      {status, %{status: run_status} = run}
      when status not in ~w(paused stopped blocked completed) and
             run_status in ~w(merging validating pushing) ->
        [%Work{role_id: "sync", kind: :sync, id: run.id, campaign_id: campaign_id}]

      _ ->
        []
    end
  end

  defp campaign_status(campaign_id) do
    case Pika.Repo.query!("SELECT status FROM campaigns WHERE id = ?", [campaign_id]).rows do
      [[status]] -> status
      [] -> nil
    end
  end
end

defmodule Pika.Agent.WorkSources.Integration do
  @moduledoc false

  @behaviour Pika.Agent.WorkSource

  alias Pika.Agent.Role.Work
  alias Pika.Agent.Roles.Integration.Domain
  alias Pika.{IntegrationStore, Repo}

  @impl true
  def runnable_work(campaign_id, workspace) do
    _ = IntegrationStore.repair_legacy_target_gate_rejections(campaign_id)

    with status when status not in ~w(paused stopped blocked completed) <-
           campaign_status(campaign_id),
         {:ok, attempt} <- IntegrationStore.queue_head(campaign_id),
         :ok <- Domain.ensure_recoverable_best(workspace, attempt) do
      [
        %Work{
          role_id: "integration",
          kind: :attempt,
          id: attempt.id,
          campaign_id: campaign_id
        }
      ]
    else
      _ -> []
    end
  end

  defp campaign_status(campaign_id) do
    case Repo.query!("SELECT status FROM campaigns WHERE id = ?", [campaign_id]).rows do
      [[status]] -> status
      [] -> nil
    end
  end
end

defmodule Pika.Agent.WorkSources.Plan do
  @moduledoc false

  @behaviour Pika.Agent.WorkSource
  alias Pika.Agent.Role.Work
  alias Pika.AttemptStore

  @impl true
  def runnable_work(campaign_id, _workspace) do
    with {:ok, %{status: "optimizing"} = context} <- AttemptStore.campaign_context(campaign_id),
         true <- context.plan_enabled do
      AttemptStore.active_attempts(campaign_id)
      |> Enum.filter(fn attempt ->
        attempt.status in ~w(running awaiting_report interrupted) and
          is_nil(attempt.plan_artifact_id)
      end)
      |> Enum.map(&work(&1, campaign_id))
    else
      _reason -> []
    end
  end

  defp work(attempt, campaign_id),
    do: %Work{role_id: "plan", kind: :attempt, id: attempt.id, campaign_id: campaign_id}
end

defmodule Pika.Agent.WorkSources.Iteration do
  @moduledoc false

  @behaviour Pika.Agent.WorkSource

  alias Pika.Agent.Role.Work
  alias Pika.AttemptStore

  @impl true
  def runnable_work(campaign_id, _workspace) do
    with {:ok, %{status: "optimizing"} = context} <- AttemptStore.campaign_context(campaign_id) do
      AttemptStore.active_attempts(campaign_id)
      |> Enum.filter(fn attempt ->
        attempt.status in ~w(running awaiting_report interrupted) and
          (not context.plan_enabled or not is_nil(attempt.plan_artifact_id))
      end)
      |> Enum.map(&work(&1, campaign_id))
    else
      _ -> []
    end
  end

  defp work(attempt, campaign_id),
    do: %Work{role_id: "iteration", kind: :attempt, id: attempt.id, campaign_id: campaign_id}
end

defmodule Pika.Agent.WorkSources.Alignment do
  @moduledoc false

  @behaviour Pika.Agent.WorkSource

  @impl true
  def runnable_work(campaign_id, _workspace),
    do: Pika.Alignment.Campaign.runnable_agent_work(campaign_id)
end
