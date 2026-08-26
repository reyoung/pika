defmodule Pika.Integration.PromptInput do
  @moduledoc "Builds the concrete v2 Integration Prompt from the FIFO Attempt and Full Case Set."

  alias Pika.Agent.RolePrompt.Section
  alias Pika.Agent.RolePromptRegistry
  alias Pika.Agent.RolePrompts.Integration
  alias Pika.Agent.RolePrompts.Integration.Input
  alias Pika.Attempt.Lifecycle
  alias Pika.Integration.Lifecycle, as: IntegrationLifecycle
  alias Pika.Integration.RunPaths
  alias Pika.Repo

  @active_statuses ~w(ready_for_integration integrating)

  @spec build(pos_integer(), non_neg_integer(), [Section.t()]) ::
          {:ok, Input.t()} | {:error, term()}
  def build(attempt_id, regression_feedback_cases, sections \\ [])
      when is_integer(regression_feedback_cases) and regression_feedback_cases >= 0 do
    with {:ok, attempt} <- Lifecycle.fetch_attempt(attempt_id),
         :ok <- require_active(attempt),
         {:ok, run} <- IntegrationLifecycle.ensure_active_run(attempt),
         {:ok, schemas} <- RolePromptRegistry.schemas(),
         {:ok, case_ids} <- full_case_ids(attempt.sampling_revision_id) do
      {:ok,
       %Input{
         full_case_ids: case_ids,
         validation_schema: schemas.integration_validation,
         result_schema: schemas.integration_result,
         run_id: run.id,
         run_sequence: run.run_sequence,
         paths: RunPaths.for_run(run.run_sequence),
         regression_feedback_cases: regression_feedback_cases,
         sections: sections
       }}
    end
  end

  @spec render(pos_integer(), non_neg_integer(), [Section.t()]) ::
          {:ok, String.t()} | {:error, term()}
  def render(attempt_id, regression_feedback_cases, sections \\ []) do
    with {:ok, input} <- build(attempt_id, regression_feedback_cases, sections),
         do: Integration.system_prompt(input)
  end

  defp require_active(%{status: status}) when status in @active_statuses, do: :ok
  defp require_active(attempt), do: {:error, {:attempt_not_integrating, attempt.status}}

  defp full_case_ids(sampling_revision_id) do
    ids =
      Repo.query!(
        """
        SELECT bc.case_id
        FROM benchmark_cases bc
        JOIN sampling_revisions sr ON sr.baseline_revision_id = bc.baseline_revision_id
        WHERE sr.id = ?
        ORDER BY bc.case_id
        """,
        [sampling_revision_id]
      ).rows
      |> List.flatten()

    if ids == [], do: {:error, :full_case_set_missing}, else: {:ok, ids}
  end
end
