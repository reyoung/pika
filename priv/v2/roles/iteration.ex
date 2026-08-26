defmodule Pika.Agent.RolePrompts.Iteration.AttemptHistory do
  @moduledoc false

  @enforce_keys [:id, :summary, :status, :path]
  defstruct @enforce_keys

  @type t :: %__MODULE__{
          id: pos_integer(),
          summary: String.t(),
          status: :accepted | :rejected,
          path: Path.t()
        }
end

defmodule Pika.Agent.RolePrompts.Iteration.StaleRefresh do
  @moduledoc false

  @enforce_keys [:best_commit, :best_repo_dir]
  defstruct @enforce_keys

  @type t :: %__MODULE__{best_commit: String.t(), best_repo_dir: Path.t()}
end

defmodule Pika.Agent.RolePrompts.Iteration.Input do
  @moduledoc false

  alias Pika.Agent.RolePrompt.Section
  alias Pika.Agent.RolePrompts.Iteration.{AttemptHistory, StaleRefresh}

  @enforce_keys [:sampling_case_ids, :recent_attempts, :all_attempts_dir, :result_schema]
  defstruct @enforce_keys ++ [sections: [], stale_refresh: nil]

  @type t :: %__MODULE__{
          sampling_case_ids: [non_neg_integer()],
          recent_attempts: [AttemptHistory.t()],
          all_attempts_dir: Path.t(),
          result_schema: Path.t(),
          sections: [Section.t()],
          stale_refresh: StaleRefresh.t() | nil
        }
end

defmodule Pika.Agent.RolePrompts.Iteration do
  @moduledoc "v2 Iteration Role-owned system prompt."

  @behaviour Pika.Agent.RolePrompt

  alias Pika.Agent.RolePrompt.Sections
  alias Pika.Agent.RolePrompts.Iteration.{AttemptHistory, Input, StaleRefresh}

  @sha_pattern ~r/^[0-9a-f]{40}([0-9a-f]{24})?$/

  @impl true
  def system_prompt(%Input{} = input) do
    with {:ok, rendered_sections} <- Sections.render(input.sections),
         :ok <- validate_case_ids(input.sampling_case_ids),
         :ok <- validate_attempts(input.recent_attempts, input.all_attempts_dir),
         :ok <- validate_stale_refresh(input.stale_refresh),
         :ok <- validate_result_schema(input.result_schema) do
      case_ids = Enum.join(input.sampling_case_ids, ",")

      {:ok,
       [
         intro(),
         rendered_sections,
         work_scope(),
         render_stale_refresh(input.stale_refresh),
         render_history(input.recent_attempts, input.all_attempts_dir),
         development_and_completion(case_ids, input.result_schema)
       ]
       |> IO.iodata_to_binary()
       |> String.trim()}
    end
  end

  def system_prompt(_input), do: {:error, :invalid_prompt_input}

  defp intro do
    """
    你是 Pika 的 Iteration Agent。你在一个整数 Attempt ID 对应的独立 Git worktree 中探索、实现和验证一次性能优化。你只能让当前 Attempt 进入 Integration 或被拒绝；只有 Integration 可以接受 Attempt 并推进 Best。

    所有用户可见分析、进度和总结必须使用中文；命令、代码、路径、标识符和协议字段保持原样。
    """
    |> String.trim()
  end

  defp work_scope do
    """


    ## 工作范围

    - 只修改当前 Attempt worktree 和分支。
    - 不修改只读 `target/`、Correctness Oracle、`verify_cases.sh`、`benchmark_cases.sh` 或其他 protected files。
    - Pika 已注入 `PIKA_CANDIDATE_MANIFEST=<当前 Attempt repo>/candidate/manifest.json` 与 `PIKA_ATTEMPT_ROOT`。将新 Candidate bundle 写到该 manifest 路径，让冻结的标准 Harness 自动读取；不得为了选择 `candidate/` 修改 `benchmarks/pika_adapter.py` 或任何 Harness 文件。若已审阅的 Harness 不读取该变量，以 `details.failure_code="baseline_harness_missing_candidate_env"` 拒绝本次 Attempt，并说明需要新的 Baseline Revision；Pika 会将它提升为 Baseline 级阻塞并停止创建 Attempt。不得以修改 Harness、替换 Development 或重用旧结果绕过。
    - 不修改 `pika/best`，不 Push 远端。
    - 可以在 Attempt branch 内创建、修改或合并提交。
    - 正常 Iteration 以创建时的 Base 工作。
    """
  end

  defp render_stale_refresh(nil), do: ""

  defp render_stale_refresh(%StaleRefresh{} = stale) do
    """


    ## stale refresh

    当前 Attempt 工作期间 Best Commit 已更新为 `#{stale.best_commit}`。从 [Best Repo](<#{stale.best_repo_dir}>) merge 这个 Best Commit 到当前 Attempt branch，然后重新运行本次 Iteration Cases 并重新估算当前 Iteration 的结果。
    """
  end

  defp render_history([], all_attempts_dir) do
    """


    ## 历史尝试

    最近没有历史尝试。

    所有 Attempt 过程在 [#{Path.basename(all_attempts_dir)}](<#{all_attempts_dir}>) 目录中。每个 Attempt 无论成功或失败，都包含 `message.jsonl` 和 `summary.jsonl`。

    Rejected Attempt 只表示那次实现失败，可能是实现、测量或环境问题；它不表示该方向永远不值得继续。可以在理解失败原因后重新尝试相同方向，但不要无解释地复制旧 Patch。
    """
  end

  defp render_history(attempts, all_attempts_dir) do
    rows =
      Enum.map_join(attempts, "\n", fn attempt ->
        "| Attempt #{attempt.id} | #{escape_cell(attempt.summary)} | #{status_label(attempt.status)} | [#{Path.basename(attempt.path)}](<#{attempt.path}>) |"
      end)

    """


    ## 历史尝试

    最近#{length(attempts)}次尝试包含：

    | 名称 | 摘要 | 状态 | 路径 |
    | --- | --- | --- | --- |
    #{rows}

    所有 Attempt 过程在 [#{Path.basename(all_attempts_dir)}](<#{all_attempts_dir}>) 目录中。每个 Attempt 无论成功或失败，都包含 `message.jsonl` 和 `summary.jsonl`。

    Rejected Attempt 只表示那次实现失败，可能是实现、测量或环境问题；它不表示该方向永远不值得继续。可以在理解失败原因后重新尝试相同方向，但不要无解释地复制旧 Patch。
    """
  end

  defp development_and_completion(case_ids, result_schema) do
    """


    ## 开发与验证

    可以先运行任意小型探索实验，但正式完成必须使用以下 Iteration Case IDs：

        ./verify_cases.sh --case-id #{case_ids}
        ./benchmark_cases.sh --case-id #{case_ids}

    保持脚本的标准 interface，不写另一个只有本 Attempt 理解的正式 Harness。避免每个 Case/Pair 启动新的重量级进程；保证 Pair 是独立交替执行。

    你需要检查正确性、性能、数值稳定性、工作目录状态和 protected files。Pika会从原始输出重算 Metrics，不要伪造平均提升或删掉不利样本。`guard` Metric 不要求改善，但普通 Case 超过 `max(max_regression_ratio, Noise Tolerance)` 的回退会在 Integration 被硬拒绝；critical Case 仍执行更严格的 Noise Tolerance 门禁。

    ## 结果选择

    以下情况可以直接 Reject，而不进入 Integration：

    - 正确性失败；
    - 明确的坏 Case；
    - 实现方向不可行；
    - 正式采样没有超过噪声的改善；
    - 继续占用 Integration 明显没有价值。

    Reject 可以没有代码修改或完整 Benchmark，但必须提供具体 failure reason 和已有证据。如果已经完成了覆盖全部 Iteration Case IDs 的有效 Benchmark，即使决定 Reject，也必须在 `files.benchmark` 中引用该 JSONL；Pika 会保留这些速度数据用于性能时间线和后续 Attempt。不要引用不完整或无效的 Benchmark。

    只有在 Sampling Cases 正确、正式测量有合理收益、Candidate 已提交且 worktree 干净时，才能选择 `ready_for_integration`。

    ## 完成

    写出 `iteration-result.json`，并确保它符合 #{schema_link(result_schema)}。Result 包含 Attempt ID、Iteration Round、Base/Candidate SHA、Sampling Revision、hypothesis、changes、risks、文件引用和 outcome：

    - `ready_for_integration`
    - `rejected`

    然后调用：

        finish_iteration(result_path, idempotency_key)
    """
  end

  defp validate_case_ids(case_ids) when is_list(case_ids) and case_ids != [] do
    if Enum.uniq(case_ids) == case_ids and Enum.all?(case_ids, &(is_integer(&1) and &1 >= 0)),
      do: :ok,
      else: {:error, :invalid_sampling_case_ids}
  end

  defp validate_case_ids(_case_ids), do: {:error, :invalid_sampling_case_ids}

  defp validate_attempts(attempts, all_attempts_dir)
       when is_list(attempts) and is_binary(all_attempts_dir) do
    cond do
      not File.dir?(all_attempts_dir) ->
        {:error, {:missing_attempts_directory, all_attempts_dir}}

      not Enum.all?(attempts, &valid_attempt?/1) ->
        {:error, :invalid_attempt_history}

      true ->
        :ok
    end
  end

  defp validate_attempts(_attempts, _all_attempts_dir), do: {:error, :invalid_attempt_history}

  defp valid_attempt?(%AttemptHistory{id: id, summary: summary, status: status, path: path}) do
    is_integer(id) and id > 0 and is_binary(summary) and String.trim(summary) != "" and
      status in [:accepted, :rejected] and File.dir?(path) and
      File.regular?(Path.join(path, "message.jsonl")) and
      File.regular?(Path.join(path, "summary.jsonl"))
  end

  defp valid_attempt?(_attempt), do: false

  defp validate_stale_refresh(nil), do: :ok

  defp validate_stale_refresh(%StaleRefresh{best_commit: commit, best_repo_dir: directory}) do
    if is_binary(commit) and Regex.match?(@sha_pattern, commit) and File.dir?(directory),
      do: :ok,
      else: {:error, :invalid_stale_refresh}
  end

  defp validate_stale_refresh(_stale), do: {:error, :invalid_stale_refresh}

  defp validate_result_schema(path) do
    if File.regular?(path), do: :ok, else: {:error, {:missing_schema_files, [path]}}
  end

  defp status_label(:accepted), do: "已合入"
  defp status_label(:rejected), do: "拒绝"

  defp escape_cell(value) do
    value
    |> String.replace(~r/\s+/, " ")
    |> String.replace("|", "\\|")
  end

  defp schema_link(path), do: "[#{Path.basename(path)}](<#{path}>)"
end
