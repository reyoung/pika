defmodule Pika.CLITest do
  use ExUnit.Case, async: false

  test "parses the v2 init and serve contracts without legacy commands" do
    root = Path.join(System.tmp_dir!(), "pika-cli-#{System.unique_integer([:positive])}")
    workspace = Path.join(root, "workspace")
    repo = Path.join(root, "repo")

    assert {:ok, init} =
             Pika.CLI.parse_init([
               workspace,
               "--repo",
               repo,
               "--token",
               "fixed-token",
               "--iteration-agents",
               "3",
               "--progress-summary"
             ])

    assert init.workspace == Path.expand(workspace)
    assert init.repo == Path.expand(repo)
    assert init.token == "fixed-token"
    assert init.iteration_agents == 3
    assert init.progress_summary

    File.mkdir_p!(workspace)
    File.write!(Path.join(workspace, "pika.yaml"), "version: 2\n")
    previous = System.get_env("PIKA_CLI_CWD")
    System.put_env("PIKA_CLI_CWD", workspace)

    on_exit(fn ->
      File.rm_rf!(root)

      if previous,
        do: System.put_env("PIKA_CLI_CWD", previous),
        else: System.delete_env("PIKA_CLI_CWD")
    end)

    assert {:ok, serve} = Pika.CLI.parse_serve([])
    assert serve.workspace == Path.expand(workspace)
    assert serve.config == Path.join(Path.expand(workspace), "pika.yaml")
    assert serve.port == 8080
    refute serve.reload

    assert {:ok, %{reload: true}} = Pika.CLI.parse_serve(["--reload"])
    assert {:ok, %{reload: true}} = Pika.CLI.parse_serve(["--autoreload"])
  end

  test "help parsing does not require paths" do
    assert {:ok, %{help: true}} = Pika.CLI.parse_init(["--help"])
    assert {:ok, %{help: true}} = Pika.CLI.parse_serve(["--help"])
    assert {:ok, %{help: true}} = Pika.CLI.parse_reconfiguration(["--help"])
  end

  test "interactive init accepts omitted paths while --yes requires them" do
    assert {:ok, %{workspace: nil, repo: nil, yes: false}} = Pika.CLI.parse_init([])
    assert {:error, message} = Pika.CLI.parse_init(["--yes"])
    assert message =~ "WORKSPACE is required with --yes"
    assert message =~ "--repo PATH is required with --yes"
  end

  test "discovers reconfiguration config and parses per-Agent choices" do
    root =
      Path.join(
        System.tmp_dir!(),
        "pika-reconfiguration-cli-#{System.unique_integer([:positive])}"
      )

    File.mkdir_p!(root)
    File.write!(Path.join(root, "pika.yaml"), "version: 2\n")
    previous = System.get_env("PIKA_CLI_CWD")
    System.put_env("PIKA_CLI_CWD", root)

    on_exit(fn ->
      File.rm_rf!(root)

      if previous,
        do: System.put_env("PIKA_CLI_CWD", previous),
        else: System.delete_env("PIKA_CLI_CWD")
    end)

    assert {:ok, opts} =
             Pika.CLI.parse_reconfiguration([
               "--role",
               "integration",
               "--backend",
               "cursor",
               "--model",
               "cursor-model",
               "--reasoning-effort",
               "ultra",
               "--yes"
             ])

    assert opts.workspace == Path.expand(root)
    assert opts.config == Path.join(Path.expand(root), "pika.yaml")
    assert opts.role == "integration"
    assert opts.backend == "cursor"
    assert opts.reasoning_effort == "ultra"
  end
end
