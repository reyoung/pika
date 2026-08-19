defmodule Pika.Test.AlignmentFixtures do
  alias Pika.Git

  @fixture_pair_count 7
  @fixture_min_valid_pairs 5

  def pair_count, do: @fixture_pair_count
  def min_valid_pairs, do: @fixture_min_valid_pairs

  def temp_dir(prefix) do
    suffix = :crypto.strong_rand_bytes(8) |> Base.url_encode64(padding: false)
    path = Path.join(System.tmp_dir!(), "#{prefix}-#{suffix}")
    File.mkdir_p!(path)
    path
  end

  def git_repo(opts \\ []) do
    repo = temp_dir("pika-alignment-source")
    Git.run!(repo, ["init"])
    Git.run!(repo, ["config", "user.name", "Pika Test"])
    Git.run!(repo, ["config", "user.email", "pika@example.invalid"])
    File.write!(Path.join(repo, "README.md"), "fixture\n")
    File.mkdir_p!(Path.join(repo, "kernel"))
    File.write!(Path.join(repo, "kernel/original.py"), "def reference(x): return x\n")
    Git.run!(repo, ["add", "."])
    Git.run!(repo, ["commit", "-m", "fixture"])

    if Keyword.get(opts, :dirty, false), do: File.write!(Path.join(repo, "dirty.txt"), "dirty\n")
    repo
  end

  def spec do
    %{
      "schema_version" => 1,
      "revision" => 1,
      "title" => "Fixture kernel",
      "target_hardware" => "H20 / sm_90a",
      "computation" => %{
        "semantics" => "identity fixture",
        "reference_path" => "kernel/reference.py",
        "inputs" => [%{"name" => "x", "dtype" => "float16", "layout" => "contiguous"}],
        "outputs" => [%{"name" => "y", "dtype" => "float16", "layout" => "contiguous"}],
        "fusion_scope" => "single kernel",
        "correctness" => %{"rtol" => 0.001, "atol" => 0.001}
      },
      "benchmark_cases" => [
        %{
          "id" => "target_case",
          "name" => "Target case",
          "kind" => "target",
          "shape" => %{"n" => 1024},
          "dtype" => "float16",
          "layout" => "contiguous",
          "frequency_weight" => 1.0
        }
      ],
      "metrics" => [
        %{
          "id" => "latency_us",
          "name" => "Latency",
          "unit" => "us",
          "direction" => "minimize",
          "role" => "target",
          "min_improvement_ratio" => 0.01
        }
      ],
      "benchmark" => %{
        "harness_path" => "kernel/bench.py",
        "warmup" => 10,
        "pair_count" => @fixture_pair_count,
        "min_valid_pairs" => @fixture_min_valid_pairs,
        "retry_limit" => 1
      },
      "stopping" => %{"max_attempts" => 10, "metric_goals" => [], "mode" => "all_goals"},
      "reference_ids" => ["cutlass"]
    }
  end

  def create_harness(setup_root) do
    File.mkdir_p!(Path.join(setup_root, "kernel"))
    File.write!(Path.join(setup_root, "kernel/reference.py"), "def reference(x): return x\n")
    File.write!(Path.join(setup_root, "kernel/test_correctness.py"), "assert True\n")
    File.write!(Path.join(setup_root, "kernel/bench.py"), "print('bench')\n")

    %{
      "reference_path" => "kernel/reference.py",
      "correctness_paths" => ["kernel/test_correctness.py"],
      "benchmark_path" => "kernel/bench.py",
      "protected_paths" => [
        "kernel/reference.py",
        "kernel/test_correctness.py",
        "kernel/bench.py"
      ]
    }
  end

  def write_baseline_artifacts(
        workspace,
        sha,
        skill_sha,
        valid_count \\ @fixture_pair_count
      ) do
    samples_relative = "artifacts/baseline/samples.jsonl"
    correctness_relative = "artifacts/baseline/correctness.json"
    profiler_relative = "artifacts/profiles/profiler.json"
    report_relative = "artifacts/profiles/report.ncu-rep"
    parsed_relative = "artifacts/profiles/analysis/metrics.json"
    evidence_relative = "artifacts/profiles/afs-trail.log"
    samples = Path.join(workspace.root, samples_relative)
    correctness = Path.join(workspace.root, correctness_relative)
    profiler = Path.join(workspace.root, profiler_relative)
    report = Path.join(workspace.root, report_relative)
    parsed = Path.join(workspace.root, parsed_relative)
    evidence = Path.join(workspace.root, evidence_relative)
    File.mkdir_p!(Path.dirname(samples))
    File.mkdir_p!(Path.dirname(profiler))
    File.mkdir_p!(Path.dirname(parsed))

    records =
      for index <- 0..(@fixture_pair_count - 1) do
        %{
          "schema_version" => 1,
          "measured_sha" => sha,
          "case_id" => "target_case",
          "metric_id" => "latency_us",
          "pair_index" => index,
          "order" => if(rem(index, 2) == 0, do: "ab", else: "ba"),
          "a" => 10.0 + index / 1000,
          "b" => 10.01 + index / 1000,
          "valid" => index < valid_count,
          "error" => if(index < valid_count, do: nil, else: "invalid")
        }
      end

    File.write!(samples, Enum.map_join(records, "\n", &Jason.encode!/1) <> "\n")

    File.write!(
      correctness,
      Jason.encode!(%{
        "measured_sha" => sha,
        "cases" => [%{"case_id" => "target_case", "passed" => true}]
      })
    )

    File.write!(
      profiler,
      Jason.encode!(%{
        "measured_sha" => sha,
        "case_id" => "target_case",
        "skill_sha" => skill_sha,
        "schema_version" => 1,
        "tool" => "NVIDIA Nsight Compute",
        "command" => "ncu --set full --export artifacts/profiles/report",
        "profile_directory" => "artifacts/profiles",
        "summary" => "fixture profiler",
        "report_paths" => [report_relative, parsed_relative],
        "parser" => %{
          "skill" => "ncu-report-skill",
          "command" => "python3 helpers/analyze_reports.py --run-dir artifacts/profiles",
          "output_paths" => [parsed_relative]
        },
        "remote_evidence_paths" => [evidence_relative]
      })
    )

    File.write!(report, "fixture ncu report\n")
    File.write!(parsed, Jason.encode!(%{"kernel" => "fixture", "metrics" => %{}}))

    File.write!(
      evidence,
      "proxy_before=ok\nworkload=visible\nexit=0\nproxy_after=ok\ncleanup=ok\n"
    )

    [samples_relative, correctness_relative, profiler_relative]
  end

  def baseline_dependency_paths,
    do: [
      "artifacts/profiles/report.ncu-rep",
      "artifacts/profiles/analysis/metrics.json",
      "artifacts/profiles/afs-trail.log"
    ]

  def write_baseline_manifest(workspace, sha, summary \\ "fixture baseline") do
    relative_path = "artifacts/baseline/manifest.json"
    absolute_path = Path.join(workspace.root, relative_path)
    File.mkdir_p!(Path.dirname(absolute_path))

    File.write!(
      absolute_path,
      Jason.encode!(%{
        "schema_version" => 1,
        "measured_sha" => sha,
        "summary" => summary,
        "samples_artifact" => "artifacts/baseline/samples.jsonl",
        "correctness_artifact" => "artifacts/baseline/correctness.json",
        "profiler_artifact" => "artifacts/profiles/profiler.json",
        "profiler_dependencies" => baseline_dependency_paths()
      })
    )

    relative_path
  end
end
