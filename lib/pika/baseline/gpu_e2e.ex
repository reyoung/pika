defmodule Pika.Baseline.GPUE2E do
  @moduledoc false

  alias Pika.Alignment.{Campaign, Workspace}

  def run(source_repo, opts \\ []) do
    pair_count = Keyword.fetch!(opts, :pair_count)
    min_valid_pairs = Keyword.fetch!(opts, :min_valid_pairs)
    if pid = Process.whereis(Campaign), do: GenServer.stop(pid)
    {:ok, workspace} = Workspace.prepare(source_repo, workspace: Keyword.get(opts, :workspace))

    skill =
      Pika.SkillRegistry.ensure_latest(Path.join(workspace.root, ".pika/skills/ncu-report-skill"))

    port = Pika.MCP.ProbeServer.available_port()

    {:ok, server} =
      Bandit.start_link(plug: Pika.Alignment.MCP.Router, ip: {127, 0, 0, 1}, port: port)

    try do
      {:ok, campaign} =
        Campaign.start(
          workspace: workspace,
          backend: :codex_app_server,
          start_backend: true,
          resolve_references: Keyword.get(opts, :resolve_references, true),
          mcp_url: "http://127.0.0.1:#{port}",
          skill: skill,
          reasoning_effort: :high
        )

      wait!(fn -> Campaign.snapshot().backend_session_id != nil end, 120_000, :backend_start)

      :ok =
        Campaign.send_message(
          alignment_requirements(workspace.source_sha, pair_count, min_valid_pairs)
        )

      wait!(fn -> Campaign.snapshot().status == :awaiting_confirmation end, 900_000, :alignment)
      {:ok, reference_review} = Campaign.reference_review()
      evidence_digest = Campaign.snapshot().reference_review_evidence.digest
      :ok = Campaign.confirm_spec(reference_review.sha256, evidence_digest)
      wait!(fn -> Campaign.snapshot().status == :optimizing end, 10_800_000, :gpu_baseline)
      snapshot = Campaign.snapshot()
      best_worktree_clean = Pika.Git.clean?(workspace.repo)

      if not best_worktree_clean,
        do: raise("temporary pika/best worktree is dirty after Baseline")

      {:ok, source_head_at_end} = Pika.Git.head(workspace.source_repo)
      source_unchanged = Workspace.verify_source_unchanged(workspace) == :ok
      clone_origin_removed = Pika.Git.run!(workspace.repo, ["remote"]) == ""

      result = %{
        status: "passed",
        source_repo: Path.expand(source_repo),
        source_sha: workspace.source_sha,
        source_dirty_entries_ignored:
          length(String.split(workspace.source_status, "\n", trim: true)),
        source_repo_unchanged: source_unchanged,
        source_head_at_end: source_head_at_end,
        source_changed_during_run: not source_unchanged,
        source_isolation_verified: clone_origin_removed,
        best_worktree_clean: best_worktree_clean,
        workspace: workspace.root,
        best_sha: snapshot.best_sha,
        campaign_status: snapshot.status,
        spec: snapshot.spec,
        harness: snapshot.harness,
        baseline: snapshot.baseline,
        artifacts: snapshot.artifacts,
        backend_session_id: snapshot.backend_session_id,
        required_operations: snapshot.required_operations
      }

      artifact_dir = Path.expand(Keyword.get(opts, :artifact_dir, "artifacts/alignment-preview"))
      Pika.JSONSafe.write_json(Path.join(artifact_dir, "welm-h20-gpu-e2e.json"), result)
      GenServer.stop(campaign)
      Process.sleep(300)
      {:ok, result}
    after
      if pid = Process.whereis(Campaign), do: GenServer.stop(pid)
      GenServer.stop(server)
    end
  rescue
    error -> {:error, Exception.format(:error, error, __STACKTRACE__)}
  end

  defp alignment_requirements(source_sha, pair_count, min_valid_pairs) do
    """
    Use committed source SHA #{source_sha}; uncommitted source-repo files are intentionally absent.
    This is the real Alignment H20 acceptance for the existing WeLM v4.5 80A3 verify-attention mega-kernel.
    Do not ask more questions. Preserve the fixed Q=6, KV=1, D=256, page=16, BF16 semantics and existing
    fused verify-attention boundary. Create a self-contained PyTorch Reference and correctness wrapper under
    pika_alignment/. Build a normalized Harness around the committed verify fixture/benchmark for exactly three
    representative target trace cases: indices 5493, 1104 and 179. Target hardware is H20/sm_90a.
    The only performance Metric is latency_us (us, minimize, target, 1% threshold). Correctness uses the repo's
    established BF16 tolerance rtol=atol=3e-2. The user-selected measurement protocol is warmup=10,
    pair_count=#{pair_count}, min_valid_pairs=#{min_valid_pairs}, retry_limit=1.
    Stop after max_attempts=10 with mode all_goals. Do not use or copy files absent from this committed clone.
    Submit Campaign Spec v1 and Harness through MCP; do not confirm on the user's behalf and do not push.
    """
  end

  defp wait!(fun, timeout, stage) do
    deadline = System.monotonic_time(:millisecond) + timeout
    do_wait!(fun, deadline, stage)
  end

  defp do_wait!(fun, deadline, stage) do
    cond do
      fun.() ->
        :ok

      System.monotonic_time(:millisecond) >= deadline ->
        raise "Alignment #{stage} timed out: #{inspect(Campaign.snapshot())}"

      true ->
        Process.sleep(250)
        do_wait!(fun, deadline, stage)
    end
  end
end
