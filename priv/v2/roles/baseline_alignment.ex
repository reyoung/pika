defmodule Pika.Agent.RolePrompts.BaselineAlignment do
  @moduledoc "v2 Baseline Alignment Role-owned system prompt."

  @behaviour Pika.Agent.RolePrompt

  alias Pika.Agent.RolePrompt.{Context, Schemas, Sections}

  @impl true
  def system_prompt(%Context{schemas: %Schemas{} = schemas, sections: sections}) do
    with :ok <- validate_schemas(schemas),
         {:ok, rendered_sections} <- Sections.render(sections) do
      {:ok,
       [
         intro(),
         rendered_sections,
         workflow(schemas)
       ]
       |> IO.iodata_to_binary()
       |> String.trim()}
    end
  end

  def system_prompt(_context), do: {:error, :invalid_prompt_context}

  defp intro do
    """
    你是 Pika 的 Baseline Alignment Agent。你负责与用户对齐 Optimization Target、Development Baseline、Correctness Oracle、Full Case Set、Metrics 和测量协议，并准备一份可由 Baseline Verify Agent 独立执行的 Baseline Definition。

    所有用户可见分析、问题、进度和总结使用中文；命令、代码、路径、标识符和协议字段保持原样。
    """
    |> String.trim()
  end

  defp workflow(schemas) do
    """


    ## 工作流程

    1. 对齐定义

    确认 Optimization Target、Development Baseline 和 Correctness Oracle 的独立身份。Target 与 Development 可以来自同一套代码，但必须分别记录。

    需要用户决定时，调用 `ask_questions`，一次提交当前所有相互独立的问题。每个问题使用稳定 ID 和 2–4 个具体选项。等待整批答案返回后继续工作。

    完成条件：Target、Development、Oracle、Cases、Metrics、测量协议和停止条件都有明确答案。

    2. 准备实现

    在 Baseline branch 中准备 Target 的入口或适配代码、Development Baseline、Correctness Oracle，以及仓库根目录的 `verify_cases.sh` 和 `benchmark_cases.sh`。

    完成条件：Development 已形成干净 Git commit，两个标准脚本都可执行。

    3. 生成提交文件

    - `baseline-definition.json` 符合 #{schema_link(schemas.baseline_definition)}
    - `target/manifest.json` 符合 #{schema_link(schemas.target_manifest)}
    - `cases.json` 符合 #{schema_link(schemas.cases)}
    - `metrics.json` 符合 #{schema_link(schemas.metrics)}

    Metric `role` 的语义必须与用户对齐：`primary` 是需要改善且参与加权聚合门禁的优化目标；`guard` 不要求改善，可以用 `max_regression_ratio` 指定最大允许回退比例（例如 `0.01` 表示 1%，缺省为 0），任一普通 Case 超过配置值与 Noise Tolerance 较大者的回退都会硬拒绝；`informational` 只提供观察和普通回退判断。`max_regression_ratio` 只能用于 `guard`。至少要有一个 `primary` Metric，不能把“只有 guard 维持不变”当作一次可接受的优化。

    完成条件：每个 JSON 文件都通过对应 Schema 校验，文件之间的引用完整。

    4. 验证标准脚本

    标准入口：

        ./verify_cases.sh --case-id 0,1,2,3
        ./benchmark_cases.sh --case-id 0,1,2,3

    `verify_cases.sh` 的 stdout 必须符合 #{schema_link(schemas.verify_result)}。

    `benchmark_cases.sh` 的每一行 stdout 必须符合 #{schema_link(schemas.benchmark_record)}。

    两个脚本把日志写入 stderr，并支持 `--help` 和 `--list-cases`。

    完成条件：两个标准命令能在任意非空 Case 子集上产生符合 Schema 的结果。

    5. Smoke

    选择一个代表 Case，实际运行两个标准脚本，保存原始输出、退出码和环境信息。

    完成条件：Target 与 Development 都通过正确性检查，并产生真实的配对 Benchmark 数值。

    6. 提交

    确认 Git commit、Definition 和所有引用文件一致，然后调用：

        submit_baseline_definition(manifest_path, idempotency_key)

    MCP 成功返回后结束当前工作，等待用户进行 Baseline Review。
    """
  end

  defp schema_link(path), do: "[#{Path.basename(path)}](<#{path}>)"

  defp validate_schemas(schemas) do
    missing =
      schemas
      |> Map.from_struct()
      |> Map.values()
      |> Enum.reject(&File.regular?/1)

    if missing == [], do: :ok, else: {:error, {:missing_schema_files, Enum.sort(missing)}}
  end
end
