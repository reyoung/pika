defmodule Pika.Attempt.PromptInput do
  @moduledoc "Builds the concrete v2 Iteration Prompt input from committed Attempt state."

  alias Pika.Agent.RolePrompt.Section
  alias Pika.Agent.RolePromptRegistry
  alias Pika.Agent.RolePrompts.Iteration
  alias Pika.Agent.RolePrompts.Iteration.{AttemptHistory, Input, StaleRefresh}
  alias Pika.Attempt.{Lifecycle, Workspace}
  alias Pika.Repo

  @spec build(pos_integer(), Path.t(), Path.t(), non_neg_integer(), [Section.t()]) ::
          {:ok, Input.t()} | {:error, term()}
  def build(attempt_id, workspace_root, best_repo, history_limit, sections \\ []) do
    with {:ok, attempt} <- Lifecycle.fetch_attempt(attempt_id),
         {:ok, schemas} <- RolePromptRegistry.schemas(),
         {:ok, sampling_case_ids} <- sampling_case_ids(attempt.sampling_revision_id) do
      {:ok,
       %Input{
         sampling_case_ids: sampling_case_ids,
         recent_attempts: recent_attempts(attempt_id, workspace_root, history_limit),
         all_attempts_dir: Path.join(workspace_root, "attempts"),
         result_schema: schemas.iteration_result,
         sections: sections,
         stale_refresh: stale_refresh(attempt, best_repo)
       }}
    end
  end

  @spec render(pos_integer(), Path.t(), Path.t(), non_neg_integer(), [Section.t()]) ::
          {:ok, String.t()} | {:error, term()}
  def render(attempt_id, workspace_root, best_repo, history_limit, sections \\ []) do
    with {:ok, input} <- build(attempt_id, workspace_root, best_repo, history_limit, sections),
         do: Iteration.system_prompt(input)
  end

  defp sampling_case_ids(sampling_revision_id) do
    ids =
      Repo.query!(
        """
        SELECT case_id FROM sampling_revision_cases
        WHERE sampling_revision_id = ? ORDER BY case_id
        """,
        [sampling_revision_id]
      ).rows
      |> List.flatten()

    if ids == [], do: {:error, :sampling_cases_missing}, else: {:ok, ids}
  end

  defp recent_attempts(_attempt_id, _workspace_root, 0), do: []

  defp recent_attempts(attempt_id, workspace_root, history_limit) do
    Repo.query!(
      """
      SELECT id, summary, status
      FROM attempts
      WHERE id != ? AND status IN ('accepted', 'rejected')
      ORDER BY id DESC LIMIT ?
      """,
      [attempt_id, history_limit]
    ).rows
    |> Enum.reverse()
    |> Enum.map(fn [id, summary, status] ->
      %AttemptHistory{
        id: id,
        summary: summary || "no summary",
        status: if(status == "accepted", do: :accepted, else: :rejected),
        path: Workspace.paths(workspace_root, id).root
      }
    end)
  end

  defp stale_refresh(%{status: "refreshing_iteration", base_sha: best_sha}, best_repo) do
    %StaleRefresh{best_commit: best_sha, best_repo_dir: best_repo}
  end

  defp stale_refresh(_attempt, _best_repo), do: nil
end
