defmodule Pika.Agent.PromptBuilder do
  @moduledoc "Renders one frozen v2 System Prompt and initial User Prompt from committed Work."

  alias Pika.Agent.RolePrompt.{Context, FileRef, Section}
  alias Pika.Agent.RolePromptRegistry
  alias Pika.Agent.RolePrompts.{BaselineAlignment, BaselineVerify}
  alias Pika.Agent.Work
  alias Pika.Attempt.PromptInput, as: IterationPromptInput
  alias Pika.Followup.PromptInput, as: FollowupPromptInput
  alias Pika.Integration.PromptInput, as: IntegrationPromptInput
  alias Pika.Optimization.Config
  alias Pika.ProgressSummary.PromptInput, as: ProgressPromptInput
  alias Pika.Repo

  @type recovery :: %{
          required(:directory) => Path.t(),
          required(:messages_file) => Path.t(),
          required(:state_file) => Path.t()
        }

  @spec build(Config.t(), Work.t(), map(), [recovery()]) ::
          {:ok,
           %{system: String.t(), activation: :await_user_kickoff | {:start_turn, String.t()}}}
          | {:error, term()}
  def build(%Config{} = config, %Work{} = work, bundle, recoveries \\ []) do
    sections = context_sections(bundle.context_file, recoveries)

    with {:ok, system} <- system_prompt(config, work, bundle.context_file, sections),
         {:ok, activation} <- activation(config, work, bundle.context_file, recoveries) do
      {:ok, %{system: system, activation: activation}}
    end
  end

  defp system_prompt(_config, %Work{role_id: "baseline_alignment"}, _context, sections) do
    with {:ok, schemas} <- RolePromptRegistry.schemas() do
      BaselineAlignment.system_prompt(%Context{
        schemas: schemas,
        sections: sections ++ verification_failure_sections()
      })
    end
  end

  defp system_prompt(_config, %Work{role_id: "baseline_verify"}, _context, sections) do
    with {:ok, schemas} <- RolePromptRegistry.schemas() do
      BaselineVerify.system_prompt(%Context{schemas: schemas, sections: sections})
    end
  end

  defp system_prompt(config, %Work{role_id: "iteration", id: id}, _context, sections) do
    with {:ok, attempt_id} <- integer_id(id) do
      IterationPromptInput.render(
        attempt_id,
        config.workspace,
        config.repo,
        config.iteration.history_limit,
        sections
      )
    end
  end

  defp system_prompt(_config, %Work{role_id: "integration", id: id}, _context, sections) do
    with {:ok, attempt_id} <- integer_id(id),
         do: IntegrationPromptInput.render(attempt_id, sections)
  end

  defp system_prompt(
         _config,
         %Work{role_id: role, id: id},
         context_file,
         _sections
       )
       when role in ~w(baseline_verify_followup iteration_followup integration_followup),
       do: id |> integer_id() |> then_render(&FollowupPromptInput.render(&1, context_file))

  defp system_prompt(
         _config,
         %Work{role_id: "progress_summary", id: id},
         context_file,
         _sections
       ),
       do: id |> integer_id() |> then_render(&ProgressPromptInput.render(&1, context_file))

  defp system_prompt(_config, work, _context, _sections),
    do: {:error, {:unsupported_prompt_work, work.role_id, work.kind}}

  defp activation(_config, %Work{role_id: "baseline_alignment"}, context, []) do
    if verification_rejected?() do
      {:ok, {:start_turn, verification_rejection_prompt(context)}}
    else
      {:ok, :await_user_kickoff}
    end
  end

  defp activation(
         _config,
         %Work{role_id: "baseline_alignment", id: work_id},
         context,
         recoveries
       ) do
    cond do
      alignment_started?(work_id) ->
        {:ok, {:start_turn, recovery_prompt(context, recoveries)}}

      verification_rejected?() ->
        {:ok, {:start_turn, verification_rejection_prompt(context)}}

      true ->
        {:ok, :await_user_kickoff}
    end
  end

  defp activation(_config, %Work{role_id: "iteration", id: id}, context, []) do
    with {:ok, attempt_id} <- integer_id(id) do
      case attempt_status(attempt_id) do
        "refreshing_iteration" -> {:ok, {:start_turn, stale_refresh_prompt(attempt_id, context)}}
        _status -> {:ok, {:start_turn, initial_prompt("Iteration", context)}}
      end
    end
  end

  defp activation(_config, work, context, []),
    do: {:ok, {:start_turn, initial_prompt(role_label(work.role_id), context)}}

  defp activation(_config, _work, context, recoveries),
    do: {:ok, {:start_turn, recovery_prompt(context, recoveries)}}

  defp context_sections(context_file, recoveries) do
    recovery_sections =
      recoveries
      |> Enum.with_index()
      |> Enum.map(fn {recovery, index} ->
        %Section{
          title: "recover #{index}",
          files: [
            %FileRef{label: "恢复目录", path: recovery.directory, kind: :directory},
            %FileRef{label: "历史消息", path: recovery.messages_file},
            %FileRef{label: "状态文件", path: recovery.state_file}
          ]
        }
      end)

    [
      %Section{
        title: "session context",
        files: [%FileRef{label: "Context Bundle", path: context_file}]
      }
      | recovery_sections
    ]
  end

  defp verification_failure_sections do
    workspace = Pika.Optimization.Persistence.current().workspace_canonical_path

    Repo.query!("""
    SELECT a.relative_path
    FROM baseline_verifications bv
    JOIN artifacts a ON a.id = bv.result_artifact_id
    WHERE bv.optimization_id = 'optimization' AND bv.outcome = 'definition_rejected'
    ORDER BY bv.id
    """).rows
    |> Enum.with_index()
    |> Enum.map(fn {[relative_path], index} ->
      absolute_path = Path.join(workspace, relative_path)

      %Section{
        title: "previous verification #{index}",
        required?: true,
        files: [
          %FileRef{
            label: "失败的 Baseline Verification Result",
            path: absolute_path
          }
        ],
        content: verification_failure_content(absolute_path)
      }
    end)
  end

  defp verification_rejected? do
    Repo.query!("""
    SELECT 1
    FROM baseline_verifications
    WHERE optimization_id = 'optimization' AND outcome = 'definition_rejected'
    LIMIT 1
    """).rows != []
  end

  defp verification_failure_content(path) do
    with {:ok, contents} <- File.read(path),
         {:ok, result} <- Jason.decode(contents) do
      feedback = %{
        "summary" => result["summary"],
        "failure_kind" => get_in(result, ["details", "failure_kind"]),
        "reason" => get_in(result, ["details", "reason"]),
        "requested_changes" => get_in(result, ["details", "requested_changes"])
      }

      "上一轮 Baseline Verify 的拒绝反馈如下。新 Revision 必须逐项处理，不能原样重交：\n\n```json\n#{Jason.encode!(feedback, pretty: true)}\n```"
    else
      _error -> "必须读取并处理上一轮 Baseline Verify 的完整拒绝结果，不能原样重交。"
    end
  end

  defp initial_prompt(role, context) do
    "开始本次 #{role} Work。先检查当前 Git/工作目录，并按 System Prompt 读取 Context Bundle：#{context}。完成时必须调用该 Role 的终态 MCP。"
  end

  defp recovery_prompt(context, recoveries) do
    latest = List.last(recoveries)

    "这是新的 Backend Session，不使用 provider resume。先读取 Context Bundle #{context}，再读取最新恢复历史 #{latest.messages_file} 和状态 #{latest.state_file}；保留并检查当前 Git 现场，然后继续缺少的终态操作。"
  end

  defp verification_rejection_prompt(context) do
    "上一轮 Baseline Verify 已拒绝 Definition。拒绝原因和 requested_changes 已注入 System Prompt；先读取 Context Bundle #{context} 和失败结果，逐项修订，并按协议调用 ask_questions。"
  end

  defp stale_refresh_prompt(attempt_id, context) do
    [[previous_base, current_base]] =
      Repo.query!(
        """
        SELECT previous.base_sha, current.base_sha
        FROM iteration_rounds current
        JOIN iteration_rounds previous
          ON previous.attempt_id = current.attempt_id AND previous.round = current.round - 1
        WHERE current.attempt_id = ?
        ORDER BY current.round DESC LIMIT 1
        """,
        [attempt_id]
      ).rows

    "Attempt #{attempt_id} 的旧 Base #{previous_base} 已 stale，当前 Best 是 #{current_base}。先读取 #{context}，在本 Attempt branch 上执行 `git merge #{current_base}`，解决冲突并重新运行当前 Sampling Cases；重新估算结果后调用 `finish_iteration`。"
  end

  defp attempt_status(attempt_id) do
    case Repo.query!("SELECT status FROM attempts WHERE id = ?", [attempt_id]).rows do
      [[status]] -> status
      [] -> nil
    end
  end

  defp alignment_started?(work_id) do
    Repo.query!(
      """
      SELECT 1 FROM conversation_turns
      WHERE optimization_id = 'optimization' AND role = 'baseline_alignment'
        AND work_kind = 'baseline_revision' AND work_id = ?
      LIMIT 1
      """,
      [work_id]
    ).rows != []
  end

  defp role_label("baseline_verify"), do: "Baseline Verify"
  defp role_label("integration"), do: "Integration"
  defp role_label("baseline_verify_followup"), do: "Baseline Verify Follow-up"
  defp role_label("iteration_followup"), do: "Iteration Follow-up"
  defp role_label("integration_followup"), do: "Integration Follow-up"
  defp role_label("progress_summary"), do: "Progress Summary"
  defp role_label(role), do: role

  defp integer_id(value) do
    case Integer.parse(value) do
      {id, ""} when id > 0 -> {:ok, id}
      _other -> {:error, {:invalid_work_id, value}}
    end
  end

  defp then_render({:ok, id}, render), do: render.(id)
  defp then_render({:error, _reason} = error, _render), do: error
end
