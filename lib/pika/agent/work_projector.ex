defmodule Pika.Agent.WorkProjector do
  @moduledoc "Projects all runnable v2 domain Work without a global Actor capacity limit."

  alias Pika.Agent.Work
  alias Pika.Attempt.Scheduler
  alias Pika.Baseline.Lifecycle, as: BaselineLifecycle
  alias Pika.Baseline.Workspace, as: BaselineWorkspace
  alias Pika.Followup.Lifecycle, as: FollowupLifecycle
  alias Pika.Integration.Lifecycle, as: IntegrationLifecycle
  alias Pika.Optimization.{Config, Persistence, StopPolicy}
  alias Pika.ProgressSummary.Lifecycle, as: ProgressSummaryLifecycle
  alias Pika.Repo

  @terminal_optimization ~w(completed stopped failed)

  @spec reconcile(Config.t(), DateTime.t()) :: {:ok, [Work.t()]} | {:error, term()}
  def reconcile(%Config{} = config, now \\ DateTime.utc_now()) do
    optimization = Persistence.current()

    with :ok <- maybe_prepare_baseline(optimization, config),
         :ok <- maybe_tick_summary(config, now),
         :ok <- maybe_apply_stop_policy(Persistence.current(), now),
         :ok <- maybe_spawn_attempts(Persistence.current(), config) do
      works =
        cond do
          optimization.status in @terminal_optimization or optimization.status == "paused" ->
            summary_and_followup_work()

          optimization.status in [
            "aligning_baseline",
            "awaiting_baseline_review",
            "verifying_baseline"
          ] ->
            BaselineLifecycle.project_work() ++ summary_and_followup_work()

          true ->
            IntegrationLifecycle.project_work() ++
              Scheduler.project_work() ++
              FollowupLifecycle.project_work() ++
              ProgressSummaryLifecycle.project_work()
        end

      {:ok,
       works
       |> Enum.map(&normalize/1)
       |> Enum.uniq_by(&Work.key/1)
       |> Enum.sort_by(&Work.key/1)}
    end
  end

  @spec terminal?(Work.t()) :: boolean()
  def terminal?(%Work{role_id: role, id: id}) do
    case role do
      "baseline_alignment" ->
        baseline_status(id) != "drafting"

      "baseline_verify" ->
        baseline_status(id) != "verifying"

      "iteration" ->
        attempt_status(id) not in ["iterating", "refreshing_iteration"]

      "integration" ->
        attempt_status(id) not in ["ready_for_integration", "integrating"]

      role when role in ~w(baseline_verify_followup iteration_followup integration_followup) ->
        followup_status(id) not in ["generating", "generator_running"]

      "progress_summary" ->
        progress_status(id) not in ["requested", "running"]

      _other ->
        true
    end
  end

  defp maybe_tick_summary(%Config{progress_summary: nil}, _now), do: :ok

  defp maybe_tick_summary(config, now) do
    case ProgressSummaryLifecycle.tick(config, now) do
      {:ok, _status} -> :ok
      {:error, reason} -> {:error, reason}
    end
  end

  defp maybe_spawn_attempts(%{status: "optimizing"}, config) do
    case Scheduler.spawn_available(config) do
      {:ok, _attempts} -> :ok
      {:error, reason} -> {:error, reason}
    end
  end

  defp maybe_spawn_attempts(_optimization, _config), do: :ok

  defp maybe_apply_stop_policy(%{status: "optimizing"}, now), do: StopPolicy.apply(now)
  defp maybe_apply_stop_policy(_optimization, _now), do: :ok

  defp maybe_prepare_baseline(%{status: "aligning_baseline"}, config) do
    case BaselineWorkspace.ensure_current(config) do
      {:ok, _baseline} -> :ok
      {:error, reason} -> {:error, reason}
    end
  end

  defp maybe_prepare_baseline(_optimization, _config), do: :ok

  defp summary_and_followup_work do
    FollowupLifecycle.project_work() ++ ProgressSummaryLifecycle.project_work()
  end

  defp normalize(%Work{} = work), do: work

  defp normalize(%{role_id: role, work_kind: kind, work_id: id} = projection) do
    payload = Map.drop(projection, [:role_id, :work_kind, :work_id])
    %Work{role_id: role, kind: kind, id: id, payload: payload}
  end

  defp baseline_status(id) do
    with {:ok, id} <- integer_id(id),
         [[status]] <-
           Repo.query!("SELECT status FROM baseline_revisions WHERE id = ?", [id]).rows do
      status
    else
      _other -> nil
    end
  end

  defp attempt_status(id) do
    with {:ok, id} <- integer_id(id),
         [[status]] <- Repo.query!("SELECT status FROM attempts WHERE id = ?", [id]).rows do
      status
    else
      _other -> nil
    end
  end

  defp followup_status(id) do
    with {:ok, id} <- integer_id(id),
         [[status]] <- Repo.query!("SELECT status FROM followup_requests WHERE id = ?", [id]).rows do
      status
    else
      _other -> nil
    end
  end

  defp progress_status(id) do
    with {:ok, id} <- integer_id(id),
         [[status]] <-
           Repo.query!("SELECT status FROM progress_summary_requests WHERE id = ?", [id]).rows do
      status
    else
      _other -> nil
    end
  end

  defp integer_id(value) do
    case Integer.parse(value) do
      {id, ""} when id > 0 -> {:ok, id}
      _other -> :error
    end
  end
end
