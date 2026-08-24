defmodule Pika.Agent.Workspace do
  @moduledoc "Resolves the v2 Agent-visible work root separately from its execution cwd."

  alias Pika.Agent.Work
  alias Pika.Attempt.Workspace, as: AttemptWorkspace
  alias Pika.Followup.Lifecycle, as: FollowupLifecycle
  alias Pika.Optimization.Config
  alias Pika.ProgressSummary.Lifecycle, as: ProgressSummaryLifecycle
  alias Pika.Repo

  @spec resolve(Config.t(), Work.t()) ::
          {:ok, %{work_root: Path.t(), cwd: Path.t()}} | {:error, term()}
  def resolve(%Config{} = config, %Work{role_id: role, id: id})
      when role in ["baseline_alignment", "baseline_verify"] do
    with {:ok, baseline_id} <- integer_id(id),
         [[relative_path]] <-
           Repo.query!("SELECT work_relative_path FROM baseline_revisions WHERE id = ?", [
             baseline_id
           ]).rows do
      root = Path.join(config.workspace, relative_path)
      nested = Path.join(root, "repo")
      {:ok, %{work_root: root, cwd: if(File.dir?(nested), do: nested, else: root)}}
    else
      [] -> {:error, {:baseline_revision_not_found, id}}
      {:error, _reason} = error -> error
    end
  end

  def resolve(%Config{} = config, %Work{role_id: "iteration", id: id}) do
    with {:ok, attempt_id} <- integer_id(id) do
      paths = AttemptWorkspace.paths(config.workspace, attempt_id)
      {:ok, %{work_root: paths.root, cwd: paths.repo}}
    end
  end

  def resolve(%Config{} = config, %Work{role_id: "integration", id: id}) do
    with {:ok, attempt_id} <- integer_id(id) do
      paths = AttemptWorkspace.paths(config.workspace, attempt_id)
      {:ok, %{work_root: paths.root, cwd: config.repo}}
    end
  end

  def resolve(_config, %Work{role_id: role, id: id})
      when role in ~w(baseline_verify_followup iteration_followup integration_followup) do
    with {:ok, request_id} <- integer_id(id),
         {:ok, request} <- FollowupLifecycle.fetch(request_id) do
      root = FollowupLifecycle.workdir(request)
      {:ok, %{work_root: root, cwd: root}}
    end
  end

  def resolve(_config, %Work{role_id: "progress_summary", id: id}) do
    with {:ok, request_id} <- integer_id(id),
         {:ok, request} <- ProgressSummaryLifecycle.fetch(request_id) do
      root = ProgressSummaryLifecycle.workdir(request)
      {:ok, %{work_root: root, cwd: root}}
    end
  end

  def resolve(_config, work), do: {:error, {:unsupported_agent_workspace, work.role_id}}

  @spec recovery_state(Work.t(), map()) :: map()
  def recovery_state(work, paths) do
    %{
      domain_status: status(work),
      work_root: paths.work_root,
      git_directory: paths.cwd,
      required_operation: required_operation(work.role_id)
    }
  end

  defp status(%Work{role_id: role, id: id})
       when role in ["baseline_alignment", "baseline_verify"] do
    query_status("baseline_revisions", id)
  end

  defp status(%Work{role_id: role, id: id}) when role in ["iteration", "integration"] do
    query_status("attempts", id)
  end

  defp status(%Work{role_id: role, id: id})
       when role in ~w(baseline_verify_followup iteration_followup integration_followup),
       do: query_status("followup_requests", id)

  defp status(%Work{role_id: "progress_summary", id: id}),
    do: query_status("progress_summary_requests", id)

  defp required_operation("baseline_alignment"), do: "submit_baseline_definition"
  defp required_operation("baseline_verify"), do: "finish_baseline_verification"
  defp required_operation("iteration"), do: "finish_iteration"
  defp required_operation("integration"), do: "finish_integration"

  defp required_operation(role)
       when role in ~w(baseline_verify_followup iteration_followup integration_followup),
       do: "submit_followup_message"

  defp required_operation("progress_summary"), do: "submit_progress_summary"

  defp query_status(table, id) do
    with {:ok, integer} <- integer_id(id),
         [[status]] <- Repo.query!("SELECT status FROM #{table} WHERE id = ?", [integer]).rows do
      status
    else
      _other -> nil
    end
  end

  defp integer_id(value) do
    case Integer.parse(value) do
      {id, ""} when id > 0 -> {:ok, id}
      _other -> {:error, {:invalid_work_id, value}}
    end
  end
end
