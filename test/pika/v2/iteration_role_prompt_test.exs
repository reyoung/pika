defmodule Pika.Agent.IterationRolePromptTest do
  use ExUnit.Case, async: true

  alias Pika.Agent.RolePrompt.{FileRef, Schemas, Section}
  alias Pika.Agent.RolePrompts.Iteration
  alias Pika.Agent.RolePrompts.Iteration.{AttemptHistory, Input, ReferenceProject, StaleRefresh}

  @project_root Path.expand("../../..", __DIR__)
  @schema_dir Path.join(@project_root, "priv/v2/roles/schemas")
  @fixture_dir Path.join(@project_root, "test/fixtures/v2/roles/iteration")
  @attempts_dir Path.join(@fixture_dir, "attempts")

  setup_all do
    assert {:ok, schemas} = Schemas.from_dir(@schema_dir)
    %{schemas: schemas}
  end

  test "renders concrete Cases, recent Attempt table, and result Schema", %{schemas: schemas} do
    input = input(schemas)
    result = Iteration.system_prompt(input)

    assert {:ok, prompt} = result

    if System.get_env("SHOW_V2_ITERATION_PROMPT") == "1" do
      IO.puts("\nCALL_RESULT=#{inspect(elem(result, 0))}\n")
      IO.puts(prompt)
    end

    assert String.starts_with?(prompt, "你是 Pika 的 Iteration Agent。")
    refute String.starts_with?(prompt, "#")
    refute prompt =~ "# Iteration System Prompt"
    refute prompt =~ "<sampling-case-ids>"
    refute prompt =~ "## stale refresh"
    refute prompt =~ "## Reference Projects"

    assert prompt =~ "./verify_cases.sh --case-id 0,3,7"
    assert prompt =~ "./benchmark_cases.sh --case-id 0,3,7"
    assert prompt =~ "files.benchmark"
    assert prompt =~ "性能时间线"
    assert prompt =~ "最近2次尝试包含"
    assert prompt =~ "| Attempt 1 | 向量化加载，平均提升2% | 已合入 |"
    assert prompt =~ "| Attempt 2 | 增加stage\\|导致寄存器压力 | 拒绝 |"
    assert prompt =~ "每个 Attempt 无论成功或失败，都包含 `message.jsonl` 和 `summary.jsonl`"
    assert prompt =~ "Rejected Attempt 只表示那次实现失败"
    assert prompt =~ "PIKA_CANDIDATE_MANIFEST"
    assert prompt =~ "不得为了选择 `candidate/` 修改 `benchmarks/pika_adapter.py`"
    assert prompt =~ "baseline_harness_missing_candidate_env"
    assert prompt =~ "details.failure_code"

    schema = schemas.iteration_result
    assert prompt =~ "[#{Path.basename(schema)}](<#{schema}>)"
    assert prompt =~ "finish_iteration(result_path, idempotency_key)"
    assert prompt =~ "PIKA_ATTEMPT_ROOT"
    assert prompt =~ "独占的 workspace"
    assert prompt =~ "必须严格为 `iteration-result.json`"

    refute prompt =~ "Pika自行核验文件 digest、Git、Patch、protected paths 和 Metrics"
    refute prompt =~ "Turn 结束但没有终态 MCP"
  end

  test "renders multiple recovery directories and files", %{schemas: schemas} do
    sections = [recovery_section(0), recovery_section(1)]

    assert {:ok, prompt} = Iteration.system_prompt(input(schemas, sections: sections))

    assert prompt =~ "## recover 0"
    assert prompt =~ "## recover 1"

    for index <- 1..2 do
      directory = Path.join(@fixture_dir, "recovery-0#{index}")
      assert prompt =~ "[recovery-0#{index}](<#{directory}>)"
      assert prompt =~ "[messages.jsonl](<#{Path.join(directory, "messages.jsonl")}>)"
      assert prompt =~ "[state.json](<#{Path.join(directory, "state.json")}>)"
    end
  end

  test "renders stale refresh only when supplied", %{schemas: schemas} do
    best_commit = String.duplicate("a", 40)
    best_repo_dir = Path.join(@fixture_dir, "best")

    stale = %StaleRefresh{best_commit: best_commit, best_repo_dir: best_repo_dir}

    assert {:ok, prompt} = Iteration.system_prompt(input(schemas, stale_refresh: stale))

    assert prompt =~ "## stale refresh"
    assert prompt =~ "Best Commit 已更新为 `#{best_commit}`"
    assert prompt =~ "[Best Repo](<#{best_repo_dir}>)"
    assert prompt =~ "merge 这个 Best Commit"
    assert prompt =~ "重新估算当前 Iteration 的结果"
    refute prompt =~ "检查 `git status`、HEAD、index"
  end

  test "renders frozen Reference Projects as read-only ref paths", %{schemas: schemas} do
    sha = String.duplicate("b", 40)
    path = Path.join(@fixture_dir, "best")

    project = %ReferenceProject{
      id: "kernel-examples",
      description: "Kernel implementation examples",
      path: path,
      sha: sha
    }

    assert {:ok, prompt} =
             Iteration.system_prompt(input(schemas, reference_projects: [project]))

    assert prompt =~ "## Reference Projects"
    assert prompt =~ "[ref/kernel-examples](<#{path}>)"
    assert prompt =~ "Kernel implementation examples"
    assert prompt =~ "固定 commit `#{sha}`"
    assert prompt =~ "不属于 Candidate Patch"
    assert prompt =~ "不是 Correctness Oracle"
  end

  test "result schema declares ready and rejected outcomes", %{schemas: schemas} do
    schema = schemas.iteration_result |> File.read!() |> Jason.decode!()

    assert get_in(schema, ["properties", "outcome", "enum"]) ==
             ~w(ready_for_integration rejected)
  end

  test "rejects Attempt history directories without required journals", %{schemas: schemas} do
    bad = %AttemptHistory{
      id: 3,
      summary: "missing journals",
      status: :rejected,
      path: Path.join(@fixture_dir, "best")
    }

    assert {:error, :invalid_attempt_history} =
             Iteration.system_prompt(input(schemas, recent_attempts: [bad]))
  end

  defp input(schemas, overrides \\ []) do
    defaults = [
      sampling_case_ids: [0, 3, 7],
      recent_attempts: [
        %AttemptHistory{
          id: 1,
          summary: "向量化加载，平均提升2%",
          status: :accepted,
          path: Path.join(@attempts_dir, "000001")
        },
        %AttemptHistory{
          id: 2,
          summary: "增加stage|导致寄存器压力",
          status: :rejected,
          path: Path.join(@attempts_dir, "000002")
        }
      ],
      all_attempts_dir: @attempts_dir,
      result_schema: schemas.iteration_result
    ]

    struct!(Input, Keyword.merge(defaults, overrides))
  end

  defp recovery_section(index) do
    directory = Path.join(@fixture_dir, "recovery-0#{index + 1}")

    %Section{
      title: "recover #{index}",
      files: [
        %FileRef{label: "恢复目录", path: directory, kind: :directory},
        %FileRef{label: "历史消息", path: Path.join(directory, "messages.jsonl")},
        %FileRef{label: "状态文件", path: Path.join(directory, "state.json")}
      ]
    }
  end
end
