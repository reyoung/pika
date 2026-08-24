defmodule Pika.Optimization.Init do
  @moduledoc "Creates a new singleton v2 Workspace from fully expanded Role configurations."

  alias Pika.FileSystem
  alias Pika.Optimization.Config
  alias Pika.Git

  @spec run(Path.t(), Path.t(), keyword()) :: {:ok, Config.t()} | {:error, term()}
  def run(workspace, repo, opts \\ []) do
    workspace = Path.expand(workspace)
    repo = Path.expand(repo)
    config_path = Path.join(workspace, "pika.yaml")

    with :ok <- validate_repo(repo),
         :ok <- prepare_workspace(workspace, config_path),
         contents <- render(repo, workspace, opts),
         :ok <- FileSystem.atomic_write(config_path, contents),
         {:ok, config} <- Config.load(config_path) do
      {:ok, config}
    end
  end

  defp validate_repo(repo) do
    cond do
      not File.dir?(repo) ->
        {:error, {:repo_not_directory, repo}}

      not (File.dir?(Path.join(repo, ".git")) or File.regular?(Path.join(repo, ".git"))) ->
        {:error, {:repo_not_git, repo}}

      not Git.clean?(repo) ->
        {:error, {:repo_not_clean, repo}}

      true ->
        :ok
    end
  end

  defp prepare_workspace(workspace, config_path) do
    with :ok <- File.mkdir_p(workspace) do
      if File.exists?(config_path),
        do: {:error, {:v2_config_already_exists, config_path}},
        else: :ok
    end
  end

  defp render(repo, workspace, opts) do
    backend = Keyword.get(opts, :backend, "codex")
    model = Keyword.get(opts, :model)
    effort = Keyword.get(opts, :reasoning_effort, "high")
    concurrency = Keyword.get(opts, :iteration_agents, 1)
    progress? = Keyword.get(opts, :progress_summary, false)
    permissions = Pika.AgentBackend.PermissionPolicy.defaults(backend)
    writer_sandbox = if backend == "cursor", do: "disabled", else: "workspace_write"

    iteration_agents =
      Enum.map_join(1..concurrency, "", fn _ ->
        [
          "      - backend: #{backend}\n",
          if(model, do: "        model: #{yaml_scalar(model)}\n", else: ""),
          "        reasoning_effort: #{effort}\n",
          "        approval_policy: #{permissions.approval_policy}\n",
          "        sandbox: #{writer_sandbox}\n"
        ]
        |> IO.iodata_to_binary()
      end)

    progress =
      if progress? do
        [
          "\n  progress_summary:\n",
          "    interval: 5m\n",
          "    timezone: Asia/Shanghai\n",
          agent_block(
            "    ",
            backend,
            model,
            "low",
            permissions.approval_policy,
            if(backend == "cursor", do: "enabled", else: "read_only")
          ),
          "    max_followups: 3\n"
        ]
      else
        ""
      end

    [
      "version: 2\n",
      "repo: #{yaml_scalar(repo)}\n",
      "workspace: #{yaml_scalar(workspace)}\n\n",
      "agents:\n",
      "  baseline_alignment:\n",
      agent_block(
        "    ",
        backend,
        model,
        effort,
        permissions.approval_policy,
        writer_sandbox
      ),
      "\n  baseline_verify:\n",
      agent_block(
        "    ",
        backend,
        model,
        effort,
        permissions.approval_policy,
        writer_sandbox
      ),
      "    max_followups: 8\n",
      "\n  iteration:\n",
      "    history_limit: 20\n",
      "    max_followups: 5\n",
      "    max_pending_attempts: 0\n",
      "    agents:\n",
      iteration_agents,
      "\n  integration:\n",
      agent_block(
        "    ",
        backend,
        model,
        effort,
        permissions.approval_policy,
        writer_sandbox
      ),
      "    max_followups: 8\n",
      "    regression_feedback_cases: 3\n",
      progress
    ]
    |> IO.iodata_to_binary()
  end

  defp agent_block(indent, backend, model, effort, approval_policy, sandbox) do
    [
      "#{indent}backend: #{backend}\n",
      if(model, do: "#{indent}model: #{yaml_scalar(model)}\n", else: ""),
      "#{indent}reasoning_effort: #{effort}\n",
      "#{indent}approval_policy: #{approval_policy}\n",
      "#{indent}sandbox: #{sandbox}\n"
    ]
  end

  defp yaml_scalar(value), do: Jason.encode!(value)
end
