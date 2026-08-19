defmodule Pika.Alignment.BoundarySmoke do
  @moduledoc false

  alias Pika.Git
  alias Pika.Alignment.{Campaign, Workspace}

  def run(backend, opts \\ []) when backend in [:codex_app_server, :cursor_acp] do
    pair_count = Keyword.fetch!(opts, :pair_count)
    min_valid_pairs = Keyword.fetch!(opts, :min_valid_pairs)
    if pid = Process.whereis(Campaign), do: GenServer.stop(pid)
    source = create_source_repo()
    {:ok, workspace} = Workspace.prepare(source)

    skill =
      Pika.SkillRegistry.ensure_latest(Path.join(workspace.root, ".pika/skills/ncu-report-skill"))

    port = Pika.MCP.ProbeServer.available_port()

    {:ok, server} =
      Bandit.start_link(plug: Pika.Alignment.MCP.Router, ip: {127, 0, 0, 1}, port: port)

    try do
      {:ok, campaign} =
        Campaign.start(
          workspace: workspace,
          backend: backend,
          start_backend: true,
          resolve_references: false,
          mcp_url: "http://127.0.0.1:#{port}",
          skill: skill,
          reasoning_effort: :low
        )

      assert_eventually!(fn -> Campaign.snapshot().backend_session_id != nil end, 90_000)
      :ok = Campaign.send_message(smoke_requirements(pair_count, min_valid_pairs))
      assert_eventually!(fn -> Campaign.snapshot().status == :awaiting_confirmation end, 240_000)
      snapshot = Campaign.snapshot()

      result = %{
        status: "passed",
        backend: backend,
        backend_session_id: snapshot.backend_session_id,
        campaign_status: snapshot.status,
        spec_ready: snapshot.spec_ready,
        spec_title: snapshot.spec["title"],
        cases: Enum.map(snapshot.spec["benchmark_cases"], & &1["id"]),
        metrics: Enum.map(snapshot.spec["metrics"], & &1["id"]),
        protected_digest: snapshot.harness.digest,
        source_repo_unchanged: Workspace.verify_source_unchanged(workspace) == :ok,
        workspace: workspace.root,
        required_operations: snapshot.required_operations
      }

      artifact_dir = Path.expand(Keyword.get(opts, :artifact_dir, "artifacts/alignment-preview"))

      Pika.JSONSafe.write_json(
        Path.join(artifact_dir, "#{backend}-boundary-smoke.json"),
        result
      )

      GenServer.stop(campaign)
      Process.sleep(200)
      {:ok, result}
    after
      if pid = Process.whereis(Campaign), do: GenServer.stop(pid)
      GenServer.stop(server)
    end
  rescue
    error -> {:error, Exception.format(:error, error, __STACKTRACE__)}
  end

  defp create_source_repo do
    repo =
      Path.join(System.tmp_dir!(), "pika-alignment-smoke-#{System.unique_integer([:positive])}")

    File.mkdir_p!(Path.join(repo, "kernel"))
    Git.run!(repo, ["init"])
    Git.run!(repo, ["config", "user.name", "Pika Smoke"])
    Git.run!(repo, ["config", "user.email", "pika-smoke@example.invalid"])
    File.write!(Path.join(repo, "README.md"), "# Identity kernel smoke\n")
    File.write!(Path.join(repo, "kernel/original.py"), "def identity(x):\n    return x\n")
    Git.run!(repo, ["add", "."])
    Git.run!(repo, ["commit", "-m", "identity fixture"])
    repo
  end

  defp smoke_requirements(pair_count, min_valid_pairs) do
    """
    This is a deterministic protocol smoke. Do not ask more questions. Target H20/sm_90a.
    Define a single identity PyTorch kernel: float16 contiguous input/output [1024], no extra fusion,
    rtol=atol=0.001. Create kernel/reference.py, kernel/test_correctness.py and kernel/bench.py.
    The only Benchmark Case is target_case (n=1024, target, frequency 1.0).
    The only Metric is latency_us (us, minimize, target, 1% threshold).
    The user-selected Harness contract is warmup=10, pair_count=#{pair_count},
    min_valid_pairs=#{min_valid_pairs}, retry_limit=1.
    Stop after max_attempts=10 with mode all_goals. Submit the full Campaign Spec v1 and Harness via MCP.
    Do not run GPU, do not confirm for the user, do not commit, and do not push.
    """
  end

  defp assert_eventually!(fun, timeout) do
    deadline = System.monotonic_time(:millisecond) + timeout
    do_assert_eventually!(fun, deadline)
  end

  defp do_assert_eventually!(fun, deadline) do
    cond do
      fun.() ->
        :ok

      System.monotonic_time(:millisecond) >= deadline ->
        raise "Alignment Boundary smoke timed out: #{inspect(Campaign.snapshot())}"

      true ->
        Process.sleep(100)
        do_assert_eventually!(fun, deadline)
    end
  end
end
