Code.require_file(Path.expand("../../support/v2_baseline_fixtures.ex", __DIR__))

defmodule Pika.Optimization.PerformanceTimelineTest do
  use ExUnit.Case, async: false

  alias Pika.Baseline.Lifecycle, as: BaselineLifecycle
  alias Pika.Optimization.{Config, PerformanceTimeline, Persistence}
  alias Pika.Test.V2BaselineFixtures
  alias Pika.{Auth, Repo}

  setup do
    root =
      Path.join(
        System.tmp_dir!(),
        "pika-performance-timeline-#{System.unique_integer([:positive])}"
      )

    workspace = Path.join(root, "workspace")
    File.mkdir_p!(workspace)
    baseline = V2BaselineFixtures.create_work_root(workspace, 0)
    config_path = Path.join(root, "pika.yaml")
    File.write!(config_path, yaml(baseline.repo, workspace))
    assert {:ok, config} = Config.load(config_path)

    Application.put_env(:pika, Repo,
      database: Path.join(workspace, "pika.sqlite3"),
      pool_size: 1,
      journal_mode: :wal,
      synchronous: :full,
      foreign_keys: :on,
      busy_timeout: 5_000
    )

    start_supervised!(Repo)
    assert :ok = Persistence.migrate()

    assert {:ok, _optimization, :initialized} =
             Persistence.initialize_or_recover(config, baseline.development_sha)

    assert {:ok, _draft} = BaselineLifecycle.ensure_draft(baseline.root)

    assert {:ok, _submitted} =
             BaselineLifecycle.submit_definition(0, baseline.root, "baseline-definition.json")

    assert {:ok, approved} =
             BaselineLifecycle.review(0, :approve, baseline.root, "baseline-definition.json")

    V2BaselineFixtures.write_verification_result(baseline, approved.id, :accepted)

    assert {:ok, _accepted} =
             BaselineLifecycle.finish_verification(
               0,
               baseline.root,
               "baseline-verification-result.json"
             )

    on_exit(fn -> File.rm_rf!(root) end)
    %{baseline: baseline}
  end

  test "projects baseline and sampled Attempt measurements with direction-aware improvement", %{
    baseline: baseline
  } do
    baseline_timeline = PerformanceTimeline.load()

    assert baseline_timeline.cases == [
             %{id: 0, label: "small"},
             %{id: 1, label: "large"}
           ]

    assert baseline_timeline.metrics == [
             %{id: "bandwidth_gbps", label: "GB/s"},
             %{id: "latency_us", label: "us"}
           ]

    assert length(baseline_timeline.points) == 4
    assert Enum.all?(baseline_timeline.points, &(&1.source == "baseline"))

    latency =
      Enum.find(
        baseline_timeline.points,
        &(&1.case_id == 0 and &1.metric_id == "latency_us")
      )

    bandwidth =
      Enum.find(
        baseline_timeline.points,
        &(&1.case_id == 0 and &1.metric_id == "bandwidth_gbps")
      )

    assert latency.improvement_ratio < 0
    assert bandwidth.improvement_ratio > 0

    insert_attempt_measurements(baseline.development_sha)
    timeline = PerformanceTimeline.load()

    assert length(timeline.points) == 6

    attempt_latency =
      Enum.find(
        timeline.points,
        &(&1.source == "iteration" and &1.metric_id == "latency_us")
      )

    attempt_bandwidth =
      Enum.find(
        timeline.points,
        &(&1.source == "iteration" and &1.metric_id == "bandwidth_gbps")
      )

    assert attempt_latency.attempt_id == 1
    assert_in_delta attempt_latency.improvement_ratio, 0.20, 1.0e-12
    assert_in_delta attempt_bandwidth.improvement_ratio, 0.25, 1.0e-12
    assert {1, _updated_at, 2, 1, _created_at} = PerformanceTimeline.version()

    %{marker: marker} = Auth.generate()
    on_exit(&Auth.clear/0)

    assert {:ok, socket} =
             PikaWeb.OptimizationLive.mount(
               %{},
               %{"pika_auth" => marker},
               %Phoenix.LiveView.Socket{}
             )

    html = render_socket(socket)
    assert html =~ ~s(phx-hook="PerformanceChart")
    assert html =~ "6 measurements"
    assert html =~ "0 · small"

    assert {:noreply, filtered_socket} =
             PikaWeb.OptimizationLive.handle_event(
               "filter_performance",
               %{"case_filter" => "0 · small", "metric_filter" => "latency_us"},
               socket
             )

    assert filtered_socket.assigns.performance_visible_points == 2
    assert length(Jason.decode!(filtered_socket.assigns.performance_points_json)) == 2
  end

  defp insert_attempt_measurements(base_sha) do
    now = System.system_time(:microsecond)
    [[sampling_revision_id]] = Repo.query!("SELECT id FROM sampling_revisions LIMIT 1").rows

    [[artifact_id]] =
      Repo.query!("SELECT source_artifact_id FROM best_metrics ORDER BY rowid LIMIT 1").rows

    Repo.query!(
      """
      INSERT INTO attempts(
        id, optimization_id, status, work_relative_path, branch, slot_index,
        base_best_revision, base_sha, sampling_revision_id, current_iteration_round,
        summary, outcome, inserted_at, updated_at
      ) VALUES (1, 'optimization', 'ready_for_integration', 'attempts/000001',
                'pika/attempt/000001', 0, 0, ?, ?, 1,
                'sample candidate', 'ready_for_integration', ?, ?)
      """,
      [base_sha, sampling_revision_id, now, now]
    )

    for {metric_id, target, candidate} <- [
          {"latency_us", 100.0, 80.0},
          {"bandwidth_gbps", 100.0, 125.0}
        ] do
      Repo.query!(
        """
        INSERT INTO attempt_metrics(
          attempt_id, case_id, metric_id, target_value, candidate_value,
          best_relative_improvement, noise_tolerance, valid_pair_count, source_artifact_id
        ) VALUES (1, 0, ?, ?, ?, NULL, 0.01, 3, ?)
        """,
        [metric_id, target, candidate, artifact_id]
      )
    end
  end

  defp render_socket(socket) do
    socket.assigns
    |> PikaWeb.OptimizationLive.render()
    |> Phoenix.HTML.Safe.to_iodata()
    |> IO.iodata_to_binary()
  end

  defp yaml(repo, workspace) do
    """
    version: 2
    repo: #{repo}
    workspace: #{workspace}
    agents:
      baseline_alignment:
        backend: codex
        approval_policy: never
        sandbox: danger_full_access
      baseline_verify:
        backend: codex
        approval_policy: never
        sandbox: danger_full_access
      iteration:
        agents:
          - backend: codex
            approval_policy: never
            sandbox: danger_full_access
      integration:
        backend: codex
        approval_policy: never
        sandbox: danger_full_access
    """
  end
end
