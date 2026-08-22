defmodule Pika.Test.CampaignPersistenceProbe do
  def set_owner(owner), do: :persistent_term.put({__MODULE__, :owner}, owner)
  def clear_owner, do: :persistent_term.erase({__MODULE__, :owner})

  def persist(state) do
    send(:persistent_term.get({__MODULE__, :owner}), {
      :campaign_persisted,
      state.agent_responding,
      length(state.messages)
    })

    :ok
  end
end

defmodule Pika.Alignment.CampaignTest do
  use ExUnit.Case, async: false

  alias Pika.Git
  alias Pika.Alignment.{ArtifactStore, Campaign, Workspace}
  alias Pika.Test.AlignmentFixtures

  @token "alignment-test-token"

  setup do
    if pid = Process.whereis(Campaign), do: GenServer.stop(pid)
    repo = AlignmentFixtures.git_repo()
    {:ok, workspace} = Workspace.prepare(repo)

    skill_dir = Path.join(workspace.root, ".pika/skills/ncu-report-skill")
    File.mkdir_p!(skill_dir)

    File.write!(
      Path.join(skill_dir, "SKILL.md"),
      "---\nname: ncu-report-skill\ndescription: test\n---\n"
    )

    skill = %{
      name: "ncu-report-skill",
      url: "https://example.invalid/ncu-report-skill.git",
      sha: String.duplicate("c", 40),
      path: skill_dir
    }

    {:ok, pid} =
      Campaign.start_link(
        workspace: workspace,
        backend: :codex_app_server,
        start_backend: false,
        resolve_references: false,
        mcp_url: "http://127.0.0.1:1/mcp",
        mcp_token: @token,
        skill: skill
      )

    on_exit(fn -> if Process.alive?(pid), do: GenServer.stop(pid) end)
    %{workspace: workspace, skill: skill}
  end

  test "adds and removes a user-owned Reference Project while the Spec is editable" do
    initial = Campaign.snapshot()

    assert {:ok, added} =
             Campaign.add_reference_project(%{
               "url" => "https://github.com/example/custom-kernels.git",
               "description" => "Campaign-specific kernels"
             })

    assert added.id == "custom-kernels"
    assert added.origin == :user
    assert added.selected

    snapshot = Campaign.snapshot()
    assert length(snapshot.references) == length(initial.references) + 1
    assert List.last(snapshot.references) == added
    assert "custom-kernels" in snapshot.spec["reference_ids"]

    assert snapshot.required_operations == [
             "submit_harness",
             "submit_implementation_bundle",
             "submit_implementation_review",
             "submit_spec"
           ]

    assert {:error, {:duplicate_reference_project, :url, _url}} =
             Campaign.add_reference_project(%{
               "id" => "another-id",
               "url" => "https://github.com/example/custom-kernels.git/"
             })

    builtin_id = initial.references |> List.first() |> Map.fetch!(:id)

    assert {:error, :builtin_reference_project_cannot_be_removed} =
             Campaign.remove_reference_project(builtin_id)

    assert :ok = Campaign.remove_reference_project("custom-kernels")
    refute Enum.any?(Campaign.snapshot().references, &(&1.id == "custom-kernels"))
    refute "custom-kernels" in Campaign.snapshot().spec["reference_ids"]
  end

  test "freezes Reference Project editing after confirmation starts" do
    :sys.replace_state(Campaign, &%{&1 | status: :resolving_references})

    assert {:error, :references_frozen} =
             Campaign.add_reference_project(%{url: "https://example.com/repo.git"})

    assert {:error, :references_frozen} = Campaign.remove_reference_project("cutlass")
  end

  test "resolves and materializes a user-added project even when startup References were pre-resolved",
       %{
         workspace: workspace,
         skill: skill
       } do
    GenServer.stop(Process.whereis(Campaign))
    source = Pika.Test.AlignmentFixtures.git_repo()

    user_reference = %{
      id: "campaign-reference",
      url: source,
      description: "Local Campaign Reference",
      origin: :user,
      selected: true,
      status: :unresolved,
      sha: nil,
      branch: nil
    }

    {:ok, pid} =
      Campaign.start_link(
        workspace: workspace,
        backend: :codex_app_server,
        start_backend: false,
        resolve_references: false,
        materialize_references: true,
        references: [user_reference],
        mcp_url: "http://127.0.0.1:1/mcp",
        mcp_token: @token,
        skill: skill
      )

    on_exit(fn -> if Process.alive?(pid), do: GenServer.stop(pid) end)
    harness_args = AlignmentFixtures.create_harness(workspace.setup_worktree)

    assert {:ok, %{ready: true}} =
             Campaign.mcp_call(@token, "submit_spec", %{
               "idempotency_key" => "user-reference-spec",
               "spec" => AlignmentFixtures.spec()
             })

    assert {:ok, _} =
             Campaign.mcp_call(
               @token,
               "submit_harness",
               Map.put(harness_args, "idempotency_key", "user-reference-harness")
             )

    assert {:ok, _} = submit_implementation_review()
    assert :ok = confirm_reviewed_spec()
    assert eventually(fn -> Campaign.snapshot().status == :building_baseline end)

    [resolved] = Campaign.snapshot().references
    assert resolved.status == :resolved
    assert resolved.sha == Git.run!(source, ["rev-parse", "HEAD"])

    checkout = Path.join(workspace.root, "refs/campaign-reference")
    link = Path.join(workspace.setup_worktree, "ref/campaign-reference")
    assert Git.run!(checkout, ["rev-parse", "HEAD"]) == resolved.sha
    assert File.lstat!(link).type == :symlink
    assert File.read_link!(link) == checkout

    assert Git.run!(link, ["rev-parse", "HEAD"]) == resolved.sha

    status =
      Git.run!(workspace.setup_worktree, ["status", "--porcelain=v1", "--untracked-files=all"])

    refute status =~ "ref/campaign-reference"
    refute File.exists?(Path.join(workspace.setup_worktree, ".gitmodules"))
  end

  test "runs Alignment, explicit confirmation, setup merge and Baseline to Optimizing", %{
    workspace: workspace,
    skill: skill
  } do
    harness_args = AlignmentFixtures.create_harness(workspace.setup_worktree)

    assert {:ok, %{ready: true}} =
             Campaign.mcp_call(@token, "submit_spec", %{
               "idempotency_key" => "spec-1",
               "spec" => AlignmentFixtures.spec()
             })

    assert {:ok, %{digest: digest}} =
             Campaign.mcp_call(
               @token,
               "submit_harness",
               Map.put(harness_args, "idempotency_key", "harness-1")
             )

    assert is_binary(digest)
    assert Campaign.snapshot().status == :drafting_spec

    assert Campaign.snapshot().required_operations == [
             "submit_implementation_bundle",
             "submit_implementation_review"
           ]

    assert :ok = confirm_reviewed_spec()
    assert eventually(fn -> Campaign.snapshot().status == :building_baseline end)

    confirmed = Campaign.snapshot()

    assert {:error, "invalid_state",
            "submit_spec is only allowed before the Campaign Spec is confirmed", %{}} =
             Campaign.mcp_call(@token, "submit_spec", %{
               "idempotency_key" => "late-spec",
               "spec" => put_in(AlignmentFixtures.spec(), ["title"], "late replacement")
             })

    assert {:error, "invalid_state",
            "submit_harness is only allowed before the Campaign Spec is confirmed", %{}} =
             Campaign.mcp_call(
               @token,
               "submit_harness",
               Map.put(harness_args, "idempotency_key", "late-harness")
             )

    after_late_submissions = Campaign.snapshot()
    assert after_late_submissions.status == :building_baseline
    assert after_late_submissions.required_operations == ["complete_setup_merge"]
    assert after_late_submissions.spec == confirmed.spec
    assert after_late_submissions.harness.digest == confirmed.harness.digest

    {setup_sha, best_sha} = merge_setup(workspace)

    assert {:ok, %{best_sha: ^best_sha}} =
             Campaign.mcp_call(@token, "complete_setup_merge", %{
               "idempotency_key" => "merge-1",
               "base_sha" => workspace.source_sha,
               "setup_sha" => setup_sha,
               "best_sha" => best_sha,
               "target_snapshot_id" => Campaign.snapshot().target_snapshot.id
             })

    AlignmentFixtures.write_baseline_artifacts(workspace, best_sha, skill.sha)

    manifest =
      AlignmentFixtures.write_baseline_manifest(workspace, best_sha, "fixture baseline",
        profiler: false
      )

    assert {:ok, %{status: "validating_baseline", total_records: 7}} =
             Campaign.mcp_call(@token, "submit_baseline", %{
               "idempotency_key" => "baseline-1",
               "manifest_artifact" => manifest
             })

    assert eventually(fn -> Campaign.snapshot().status == :selecting_iteration_sample end)
    [metric] = Campaign.snapshot().baseline.metrics
    assert metric.valid_pair_count == AlignmentFixtures.pair_count()
    snapshot = Campaign.snapshot()
    assert snapshot.status == :selecting_iteration_sample
    assert snapshot.required_operations == ["submit_iteration_sample"]
    assert Enum.any?(snapshot.artifacts, &(&1.relative_path == manifest))

    assert Enum.any?(
             snapshot.artifacts,
             &(&1.relative_path == "artifacts/baseline/samples.jsonl")
           )

    refute Enum.any?(
             snapshot.artifacts,
             &(&1.relative_path == "artifacts/profiles/profiler.json")
           )

    assert {:ok, %{status: "optimizing", sampling_revision: 1}} =
             Campaign.mcp_call(@token, "submit_iteration_sample", %{
               "idempotency_key" => "sample-1",
               "case_ids" => ["target_case"],
               "reasons" => %{"target_case" => "only target and representative fixture"},
               "estimated_cost" => %{
                 "iteration_seconds" => 1.0,
                 "full_seconds" => 1.0,
                 "savings_ratio" => 0.0
               },
               "summary" => "single-case fixture"
             })

    snapshot = Campaign.snapshot()
    assert snapshot.status == :optimizing
    assert snapshot.required_operations == []
    assert snapshot.baseline.summary == "fixture baseline"
    assert snapshot.iteration_sampling.case_ids == ["target_case"]
    assert length(snapshot.sampling_revisions) == 1
    assert :ok = Workspace.verify_source_unchanged(workspace)
  end

  test "sends text and Artifacts as one atomic user message", %{workspace: workspace} do
    upload_source = Path.join(workspace.root, "upload.jsonl")
    File.write!(upload_source, "{\"n\":128}\n")
    {:ok, artifact} = ArtifactStore.copy_upload(workspace.root, upload_source, "shapes.jsonl")

    assert :ok = Campaign.send_message("use this production dump", [artifact])
    snapshot = Campaign.snapshot()
    user_message = List.last(snapshot.messages)
    assert user_message.role == :user
    assert user_message.content == "use this production dump"
    assert user_message.attachments == [artifact]
    assert Enum.any?(snapshot.artifacts, &(&1.relative_path == artifact.relative_path))
  end

  test "cannot build Baseline before confirmation and returns to DraftingSpec on user changes", %{
    workspace: workspace
  } do
    assert {:error, :spec_not_confirmable} = Campaign.confirm_spec(nil, nil, nil)
    harness_args = AlignmentFixtures.create_harness(workspace.setup_worktree)

    assert {:ok, %{ready: true}} =
             Campaign.mcp_call(@token, "submit_spec", %{
               "idempotency_key" => "reject-spec",
               "spec" => AlignmentFixtures.spec()
             })

    assert {:ok, _} =
             Campaign.mcp_call(
               @token,
               "submit_harness",
               Map.put(harness_args, "idempotency_key", "reject-harness")
             )

    assert Campaign.snapshot().status == :drafting_spec
    assert {:ok, _} = submit_implementation_review()
    assert Campaign.snapshot().status == :awaiting_confirmation
    assert {:ok, implementation_review} = Campaign.implementation_review()
    assert implementation_review.target.path == "kernel/reference.py"
    assert implementation_review.target.content == "def reference(x): return x\n"
    assert {:error, :target_not_reviewed} = Campaign.confirm_spec(nil, nil, nil)
    assert :ok = Campaign.request_changes("目标 Case 需要改成 n=2048")
    snapshot = Campaign.snapshot()
    assert snapshot.status == :drafting_spec

    assert snapshot.required_operations == [
             "submit_harness",
             "submit_implementation_bundle",
             "submit_implementation_review",
             "submit_spec"
           ]

    refute snapshot.status == :building_baseline
  end

  test "requires successful current Reference performance evidence before confirmation", %{
    workspace: workspace
  } do
    harness_args = AlignmentFixtures.create_harness(workspace.setup_worktree)

    assert {:ok, %{ready: true}} =
             Campaign.mcp_call(@token, "submit_spec", %{
               "idempotency_key" => "evidence-spec",
               "spec" => AlignmentFixtures.spec()
             })

    assert {:ok, _} =
             Campaign.mcp_call(
               @token,
               "submit_harness",
               Map.put(harness_args, "idempotency_key", "evidence-harness")
             )

    assert Campaign.snapshot().required_operations == [
             "submit_implementation_bundle",
             "submit_implementation_review"
           ]

    assert {:error, "missing_required_data", _, %{reason: :implementation_review_run_failed}} =
             AlignmentFixtures.submit_implementation_review(
               @token,
               workspace,
               "failed-reference-review",
               %{"exit_code" => 1}
             )

    assert {:error, "missing_required_data", _,
            %{
              reason: {:unknown_benchmark_case, "invented_case"}
            }} =
             AlignmentFixtures.submit_implementation_review(
               @token,
               workspace,
               "unknown-case-reference-review",
               %{"case_id" => "invented_case"}
             )

    assert {:error, "missing_required_data", _,
            %{
              reason: {:metric_unit_mismatch, "latency_us", "ms", "us"}
            }} =
             AlignmentFixtures.submit_implementation_review(
               @token,
               workspace,
               "wrong-unit-reference-review",
               %{
                 "metrics" => [
                   %{
                     "metric_id" => "latency_us",
                     "target_value" => 1.0,
                     "development_value" => 1.0,
                     "unit" => "ms",
                     "sample_count" => 3
                   }
                 ]
               }
             )

    assert {:ok, %{case_id: "target_case", ready_for_user_review: true}} =
             AlignmentFixtures.submit_implementation_review(
               @token,
               workspace,
               "valid-reference-review"
             )

    snapshot = Campaign.snapshot()
    assert snapshot.status == :awaiting_confirmation

    assert [
             %{
               metric_id: "latency_us",
               target_value: 10.0,
               development_value: 12.5,
               unit: "us",
               sample_count: 3
             }
           ] = snapshot.implementation_review_evidence.metrics

    {:ok, review} = Campaign.implementation_review()

    assert {:error, :implementation_evidence_not_reviewed} =
             Campaign.confirm_spec(
               review.target_snapshot.digest,
               snapshot.prepared_setup_sha,
               nil
             )

    evidence = snapshot.implementation_review_evidence
    File.write!(Path.join(workspace.root, evidence.output_artifact), "changed after review\n")

    assert {:error,
            {:implementation_review_failed,
             {:artifact_verification_failed, artifact_path, :sha256_mismatch}}} =
             Campaign.confirm_spec(
               review.target_snapshot.digest,
               evidence.development_sha,
               evidence.digest
             )

    assert artifact_path == evidence.output_artifact
    assert Campaign.snapshot().status == :awaiting_confirmation
  end

  test "reopens BuildingBaseline as a new Spec revision after setup merge", %{
    workspace: workspace
  } do
    harness_args = AlignmentFixtures.create_harness(workspace.setup_worktree)

    assert {:ok, %{ready: true, revision: 1}} =
             Campaign.mcp_call(@token, "submit_spec", %{
               "idempotency_key" => "reopen-spec-1",
               "spec" => AlignmentFixtures.spec()
             })

    assert {:ok, _} =
             Campaign.mcp_call(
               @token,
               "submit_harness",
               Map.put(harness_args, "idempotency_key", "reopen-harness-1")
             )

    assert :ok = confirm_reviewed_spec()
    assert eventually(fn -> Campaign.snapshot().status == :building_baseline end)
    {setup_sha, first_best_sha} = merge_setup(workspace)

    assert {:ok, %{best_sha: ^first_best_sha}} =
             Campaign.mcp_call(@token, "complete_setup_merge", %{
               "idempotency_key" => "reopen-merge-1",
               "base_sha" => workspace.source_sha,
               "setup_sha" => setup_sha,
               "best_sha" => first_best_sha,
               "target_snapshot_id" => Campaign.snapshot().target_snapshot.id
             })

    first_target_id = Campaign.snapshot().target_snapshot.id

    assert Campaign.snapshot().required_operations == ["submit_baseline"]
    assert :ok = Campaign.request_changes("Reference 应改为 FlashAttention 4 / CuTeDSL")

    reopened = Campaign.snapshot()
    assert reopened.status == :drafting_spec
    assert reopened.spec["revision"] == 2
    assert reopened.best_sha == first_best_sha

    assert reopened.required_operations == [
             "submit_harness",
             "submit_implementation_bundle",
             "submit_implementation_review",
             "submit_spec"
           ]

    assert reopened.baseline == nil

    revision_workspace = :sys.get_state(Campaign).workspace
    assert revision_workspace.setup_worktree == Path.join(workspace.root, "setup/2")

    assert Git.run!(revision_workspace.setup_worktree, ["branch", "--show-current"]) ==
             "pika/setup/2"

    revised_spec =
      AlignmentFixtures.spec()
      |> Map.put("revision", 99)
      |> Map.put("title", "Fixture kernel with FA4")
      |> put_in(
        ["implementations", "optimization_target", "entrypoint"],
        "kernel/fa4_reference.py"
      )

    assert {:ok, %{ready: true, revision: 2}} =
             Campaign.mcp_call(@token, "submit_spec", %{
               "idempotency_key" => "reopen-spec-2",
               "spec" => revised_spec
             })

    revised_harness = AlignmentFixtures.create_harness(revision_workspace.setup_worktree)

    File.write!(
      Path.join(revision_workspace.setup_worktree, "kernel/fa4_reference.py"),
      "def fa4_reference(x): return x\n"
    )

    assert {:ok, _} =
             Campaign.mcp_call(
               @token,
               "submit_harness",
               Map.put(revised_harness, "idempotency_key", "reopen-harness-2")
             )

    assert Campaign.snapshot().status == :drafting_spec
    assert Campaign.snapshot().spec["revision"] == 2
    assert :ok = confirm_reviewed_spec()
    assert eventually(fn -> Campaign.snapshot().status == :building_baseline end)
    refute Campaign.snapshot().target_snapshot.id == first_target_id

    {second_setup_sha, second_best_sha} = merge_setup(revision_workspace)

    assert {:ok, %{best_sha: ^second_best_sha}} =
             Campaign.mcp_call(@token, "complete_setup_merge", %{
               "idempotency_key" => "reopen-merge-2",
               "base_sha" => first_best_sha,
               "setup_sha" => second_setup_sha,
               "best_sha" => second_best_sha,
               "target_snapshot_id" => Campaign.snapshot().target_snapshot.id
             })

    assert Campaign.snapshot().required_operations == ["submit_baseline"]
    assert Campaign.snapshot().best_sha == second_best_sha
  end

  test "keeps the frozen Target when Harness or Development is revised", %{
    workspace: workspace
  } do
    harness_args = AlignmentFixtures.create_harness(workspace.setup_worktree)

    assert {:ok, %{ready: true}} =
             Campaign.mcp_call(@token, "submit_spec", %{
               "idempotency_key" => "stable-target-spec",
               "spec" => AlignmentFixtures.spec()
             })

    assert {:ok, _} =
             Campaign.mcp_call(
               @token,
               "submit_harness",
               Map.put(harness_args, "idempotency_key", "stable-target-harness-1")
             )

    assert {:ok, _} =
             AlignmentFixtures.submit_implementation_review(
               @token,
               workspace,
               "stable-target-review"
             )

    first = Campaign.snapshot()
    first_target = first.target_snapshot
    first_development_sha = first.prepared_setup_sha

    File.write!(
      Path.join(workspace.setup_worktree, "kernel/development.py"),
      "def candidate(x): return x + 1\n"
    )

    assert {:ok, _} =
             Campaign.mcp_call(
               @token,
               "submit_harness",
               Map.put(harness_args, "idempotency_key", "stable-target-harness-2")
             )

    assert {:ok, second_development_sha} =
             AlignmentFixtures.prepare_implementation_bundle(
               @token,
               workspace,
               "stable-target-bundle-2"
             )

    second = Campaign.snapshot()
    assert second.target_snapshot.id == first_target.id
    assert second.target_snapshot.digest == first_target.digest
    assert second.target_snapshot.source_sha == first_target.source_sha
    refute second_development_sha == first_development_sha
    assert second.prepared_setup_sha == second_development_sha
  end

  test "Baseline Agent can autonomously reopen a frozen Baseline definition through MCP", %{
    workspace: workspace
  } do
    harness_args = AlignmentFixtures.create_harness(workspace.setup_worktree)

    assert {:ok, %{ready: true}} =
             Campaign.mcp_call(@token, "submit_spec", %{
               "idempotency_key" => "agent-reopen-spec",
               "spec" => AlignmentFixtures.spec()
             })

    assert {:ok, _} =
             Campaign.mcp_call(
               @token,
               "submit_harness",
               Map.put(harness_args, "idempotency_key", "agent-reopen-harness")
             )

    assert :ok = confirm_reviewed_spec()
    assert eventually(fn -> Campaign.snapshot().status == :building_baseline end)
    {setup_sha, best_sha} = merge_setup(workspace)

    assert {:ok, _} =
             Campaign.mcp_call(@token, "complete_setup_merge", %{
               "idempotency_key" => "agent-reopen-merge",
               "base_sha" => workspace.source_sha,
               "setup_sha" => setup_sha,
               "best_sha" => best_sha,
               "target_snapshot_id" => Campaign.snapshot().target_snapshot.id
             })

    assert {:error, "missing_required_data", _, %{}} =
             Campaign.mcp_call(@token, "reopen_baseline_definition", %{
               "idempotency_key" => "agent-reopen-empty",
               "reason" => "",
               "requested_changes" => "use FA4"
             })

    user_message_count =
      Campaign.snapshot().messages |> Enum.count(&(&1.role == :user))

    test_process = self()

    :sys.replace_state(Campaign, fn state ->
      validation_pid = spawn(fn -> Process.sleep(:infinity) end)
      monitor_ref = Process.monitor(validation_pid)
      send(test_process, {:fake_baseline_validation, validation_pid})

      %{
        state
        | baseline_submission: %{pid: validation_pid, monitor_ref: monitor_ref}
      }
    end)

    assert_receive {:fake_baseline_validation, validation_pid}
    assert Process.alive?(validation_pid)

    assert {:ok,
            %{
              status: "drafting_spec",
              revision: 2,
              setup_branch: "pika/setup/2",
              required_operations: [
                "submit_harness",
                "submit_implementation_bundle",
                "submit_implementation_review",
                "submit_spec"
              ]
            }} =
             Campaign.mcp_call(@token, "reopen_baseline_definition", %{
               "idempotency_key" => "agent-reopen-fa4",
               "reason" => "frozen Reference invokes the old implementation",
               "requested_changes" => "replace it with the pinned FA4 CuTeDSL adapter"
             })

    assert eventually(fn -> not Process.alive?(validation_pid) end)

    reopened = Campaign.snapshot()
    assert reopened.status == :drafting_spec
    assert reopened.spec["revision"] == 2
    assert reopened.best_sha == best_sha

    assert reopened.required_operations == [
             "submit_harness",
             "submit_implementation_bundle",
             "submit_implementation_review",
             "submit_spec"
           ]

    assert Enum.count(reopened.messages, &(&1.role == :user)) == user_message_count

    assert Enum.any?(reopened.messages, fn message ->
             message.role == :system and
               String.contains?(message.content, "Baseline Agent 请求修订") and
               String.contains?(message.content, "FA4 CuTeDSL")
           end)

    internal_state = :sys.get_state(Campaign)
    revision_workspace = internal_state.workspace
    assert revision_workspace.setup_worktree == Path.join(workspace.root, "setup/2")
    assert internal_state.backend_workflow == :alignment
    assert internal_state.workflow_kickoffs.alignment =~ "FA4 CuTeDSL"

    assert Git.run!(revision_workspace.setup_worktree, ["branch", "--show-current"]) ==
             "pika/setup/2"

    assert {:error, "forbidden_role", "reopen_baseline_definition requires a baseline session",
            %{}} =
             Campaign.mcp_call(@token, "reopen_baseline_definition", %{
               "idempotency_key" => "agent-reopen-again",
               "reason" => "another issue",
               "requested_changes" => "change it again"
             })
  end

  test "completes a required setup merge from a confirmed legacy DraftingSpec", %{
    workspace: workspace
  } do
    harness_args = AlignmentFixtures.create_harness(workspace.setup_worktree)

    assert {:ok, %{ready: true}} =
             Campaign.mcp_call(@token, "submit_spec", %{
               "idempotency_key" => "legacy-merge-spec",
               "spec" => AlignmentFixtures.spec()
             })

    assert {:ok, _} =
             Campaign.mcp_call(
               @token,
               "submit_harness",
               Map.put(harness_args, "idempotency_key", "legacy-merge-harness")
             )

    assert :ok = confirm_reviewed_spec()
    assert eventually(fn -> Campaign.snapshot().status == :building_baseline end)

    :sys.replace_state(Campaign, &%{&1 | status: :drafting_spec})
    drifted = Campaign.snapshot()
    assert drifted.status == :drafting_spec
    assert drifted.required_operations == ["complete_setup_merge"]

    {setup_sha, best_sha} = merge_setup(workspace)

    assert {:ok, %{best_sha: ^best_sha}} =
             Campaign.mcp_call(@token, "complete_setup_merge", %{
               "idempotency_key" => "legacy-merge-complete",
               "base_sha" => workspace.source_sha,
               "setup_sha" => setup_sha,
               "best_sha" => best_sha,
               "target_snapshot_id" => Campaign.snapshot().target_snapshot.id
             })

    reconciled = Campaign.snapshot()
    assert reconciled.status == :building_baseline
    assert reconciled.best_sha == best_sha
    assert reconciled.required_operations == ["submit_baseline"]
  end

  test "allows an attachment-only user message", %{workspace: workspace} do
    upload_source = Path.join(workspace.root, "shape.pkl")
    File.write!(upload_source, "fixture")
    {:ok, artifact} = ArtifactStore.copy_upload(workspace.root, upload_source, "shape.pkl")

    assert :ok = Campaign.send_message("", [artifact])

    assert %{role: :user, content: "", attachments: [^artifact]} =
             List.last(Campaign.snapshot().messages)
  end

  test "same idempotency key is replayed and conflicting request is rejected" do
    request = %{"idempotency_key" => "spec-same", "spec" => AlignmentFixtures.spec()}
    assert {:ok, first} = Campaign.mcp_call(@token, "submit_spec", request)
    assert {:ok, ^first} = Campaign.mcp_call(@token, "submit_spec", request)

    changed = put_in(request, ["spec", "title"], "different")

    assert {:error, "idempotency_conflict", _, _} =
             Campaign.mcp_call(@token, "submit_spec", changed)
  end

  test "ask_questions presents a batch sequentially and returns all answers together" do
    task =
      Task.async(fn ->
        Campaign.mcp_call(@token, "ask_questions", %{
          "questions" => [
            %{
              "id" => "fusion_scope",
              "question" => "优化边界应包含 epilogue 吗？",
              "options" => [
                %{"label" => "包含", "description" => "允许融合 epilogue"},
                %{"label" => "不包含", "description" => "只优化核心算子"}
              ]
            },
            %{
              "id" => "correctness",
              "question" => "正确性容差使用哪一档？",
              "options" => [
                %{"label" => "严格", "description" => "rtol 1e-5"},
                %{"label" => "宽松", "description" => "rtol 1e-3"}
              ]
            }
          ]
        })
      end)

    assert eventually(fn -> not is_nil(Campaign.snapshot().pending_question) end)
    snapshot = Campaign.snapshot()
    assert snapshot.pending_question.question == "优化边界应包含 epilogue 吗？"
    assert snapshot.pending_question.position == 1
    assert snapshot.pending_question.total == 2
    assert Enum.map(snapshot.pending_question.options, & &1.label) == ["包含", "不包含"]
    assert List.last(snapshot.messages).kind == :question
    first_question_id = snapshot.pending_question.id

    assert :ok =
             Campaign.answer_question(snapshot.pending_question.id, "包含", "option-1")

    refute Task.yield(task, 20)
    snapshot = Campaign.snapshot()
    assert snapshot.pending_question.question == "正确性容差使用哪一档？"
    assert snapshot.pending_question.position == 2
    assert snapshot.pending_question.total == 2
    assert List.last(snapshot.messages).kind == :question

    assert {:error, :question_expired} =
             Campaign.answer_question(first_question_id, "duplicate click", "option-1")

    assert :ok = Campaign.answer_question(snapshot.pending_question.id, "rtol 5e-4")

    assert {:ok,
            %{
              answers: [
                %{
                  question_id: "fusion_scope",
                  answer: "包含",
                  selected_option: "包含"
                },
                %{
                  question_id: "correctness",
                  answer: "rtol 5e-4",
                  selected_option: nil
                }
              ]
            }} = Task.await(task)

    assert Campaign.snapshot().pending_question == nil
    assert List.last(Campaign.snapshot().messages).content == "rtol 5e-4"
  end

  test "clears a pending question batch when its MCP caller disconnects" do
    task =
      Task.async(fn ->
        Campaign.mcp_call(@token, "ask_questions", %{
          "questions" => [
            %{
              "id" => "continue",
              "question" => "还需要继续吗？",
              "options" => [%{"label" => "继续"}, %{"label" => "停止"}]
            }
          ]
        })
      end)

    assert eventually(fn -> not is_nil(Campaign.snapshot().pending_question) end)
    Task.shutdown(task, :brutal_kill)
    assert eventually(fn -> is_nil(Campaign.snapshot().pending_question) end)
  end

  test "rejects duplicate batch ids and removes the singular ask_question tool" do
    assert {:error, "missing_required_data", "question ids must be unique", %{}} =
             Campaign.mcp_call(@token, "ask_questions", %{
               "questions" => [
                 question_args("duplicate", "First?"),
                 question_args("duplicate", "Second?")
               ]
             })

    assert {:error, "forbidden_role", "tool is unavailable: ask_question", %{}} =
             Campaign.mcp_call(@token, "ask_question", %{})
  end

  test "a late start_turn reply cannot resurrect an already completed Turn" do
    pid = Process.whereis(Campaign)
    session_id = "race-session"

    send(
      pid,
      {:pika_backend_event,
       Pika.AgentBackend.Event.new(:turn_started, :fake, session_id, %{turn_id: "old-turn"})}
    )

    send(
      pid,
      {:pika_backend_event,
       Pika.AgentBackend.Event.new(:turn_completed, :fake, session_id, %{turn_id: "old-turn"})}
    )

    send(pid, {:backend_turn_result, {:ok, "old-turn"}})

    Process.sleep(20)
    assert Campaign.snapshot().active_turn_id == nil
  end

  test "persists once at the Turn boundary instead of once per streamed token", %{
    workspace: workspace,
    skill: skill
  } do
    GenServer.stop(Process.whereis(Campaign))
    Pika.Test.CampaignPersistenceProbe.set_owner(self())
    on_exit(&Pika.Test.CampaignPersistenceProbe.clear_owner/0)

    {:ok, pid} =
      Campaign.start_link(
        workspace: workspace,
        persistence: Pika.Test.CampaignPersistenceProbe,
        backend: :codex_app_server,
        start_backend: false,
        resolve_references: false,
        mcp_url: "http://127.0.0.1:1/mcp",
        skill: skill
      )

    on_exit(fn -> if Process.alive?(pid), do: GenServer.stop(pid) end)
    assert_receive {:campaign_persisted, false, _initial_messages}

    send(
      pid,
      {:pika_backend_event,
       Pika.AgentBackend.Event.new(:turn_started, :fake, "stream-session", %{
         turn_id: "stream-turn"
       })}
    )

    Enum.each(1..100, fn _index ->
      send(
        pid,
        {:pika_backend_event,
         Pika.AgentBackend.Event.new(:message_delta, :fake, "stream-session", %{
           turn_id: "stream-turn",
           data: %{delta: "token "}
         })}
      )
    end)

    send(
      pid,
      {:pika_backend_event,
       Pika.AgentBackend.Event.new(:turn_completed, :fake, "stream-session", %{
         turn_id: "stream-turn"
       })}
    )

    assert_receive {:campaign_persisted, false, _messages}, 1_000
    refute_receive {:campaign_persisted, _, _}, 50
  end

  test "a cancelled predecessor cannot interrupt or clear a newer steered Turn" do
    pid = Process.whereis(Campaign)
    session_id = "steer-race-session"

    send(
      pid,
      {:pika_backend_event,
       Pika.AgentBackend.Event.new(:turn_started, :fake, session_id, %{turn_id: "old-turn"})}
    )

    send(
      pid,
      {:pika_backend_event,
       Pika.AgentBackend.Event.new(:turn_started, :fake, session_id, %{turn_id: "new-turn"})}
    )

    send(
      pid,
      {:pika_backend_event,
       Pika.AgentBackend.Event.new(:turn_completed, :fake, session_id, %{turn_id: "old-turn"})}
    )

    send(pid, {:completion_followup, "old-turn"})

    Process.sleep(20)
    assert Campaign.snapshot().active_turn_id == "new-turn"
  end

  test "a completion follow-up tolerates a missing Baseline submission" do
    pid = Process.whereis(Campaign)

    :sys.replace_state(Campaign, fn state ->
      %{state | backend: self(), baseline_submission: nil, target_submission: %{id: "in-flight"}}
    end)

    send(pid, {:completion_followup, "completed-turn"})

    Process.sleep(20)
    assert Process.alive?(pid)
    refute Campaign.snapshot().agent_responding
  end

  test "filters protocol internals and aggregates a Turn into one concise activity row" do
    pid = Process.whereis(Campaign)
    before_count = length(Campaign.snapshot().messages)

    send(
      pid,
      {:pika_backend_event,
       Pika.AgentBackend.Event.new(:tool_started, :fake, "s", %{
         data: %{
           item: %{
             "type" => "userMessage",
             "content" => [%{"text" => String.duplicate("raw", 200)}]
           }
         }
       })}
    )

    Process.sleep(10)
    assert length(Campaign.snapshot().messages) == before_count

    send(
      pid,
      {:pika_backend_event,
       Pika.AgentBackend.Event.new(:tool_started, :fake, "s", %{
         data: %{
           item: %{
             "id" => "command-1",
             "type" => "commandExecution",
             "command" => "python3 benchmark.py --very-long " <> String.duplicate("x", 300),
             "commandActions" => [%{"type" => "read", "path" => "benchmark.py"}]
           }
         }
       })}
    )

    Process.sleep(10)
    activity = List.last(Campaign.snapshot().messages)
    assert activity.role == :activity
    assert activity.content == "读取文件 · 运行了命令"
    assert activity.activity.running
    assert [%{key: "command-1", status: "running"}] = activity.activity.details

    send(
      pid,
      {:pika_backend_event,
       Pika.AgentBackend.Event.new(:tool_completed, :fake, "s", %{
         data: %{
           item: %{
             "id" => "command-1",
             "type" => "commandExecution",
             "command" => "python3 benchmark.py --very-long " <> String.duplicate("x", 300),
             "commandActions" => [%{"type" => "read", "path" => "benchmark.py"}],
             "status" => "completed"
           }
         }
       })}
    )

    send(
      pid,
      {:pika_backend_event,
       Pika.AgentBackend.Event.new(:tool_started, :fake, "s", %{
         data: %{
           item: %{
             "id" => "mcp-1",
             "type" => "mcpToolCall",
             "tool" => "get_context"
           }
         }
       })}
    )

    Process.sleep(10)
    messages = Campaign.snapshot().messages
    activity = List.last(messages)
    assert length(messages) == before_count + 1
    assert activity.content == "读取文件 · 运行了命令 · 调用 MCP"
    refute activity.content =~ "benchmark.py"
    assert Enum.at(activity.activity.details, 0).status == "completed"
    assert Enum.at(activity.activity.details, 1).status == "running"
  end

  test "allows one complete Baseline rerun and blocks the second insufficient result", %{
    workspace: workspace,
    skill: skill
  } do
    harness_args = AlignmentFixtures.create_harness(workspace.setup_worktree)

    {:ok, _} =
      Campaign.mcp_call(@token, "submit_spec", %{
        "idempotency_key" => "s",
        "spec" => AlignmentFixtures.spec()
      })

    {:ok, _} =
      Campaign.mcp_call(@token, "submit_harness", Map.put(harness_args, "idempotency_key", "h"))

    :ok = confirm_reviewed_spec()
    assert eventually(fn -> Campaign.snapshot().status == :building_baseline end)
    {setup_sha, best_sha} = merge_setup(workspace)

    {:ok, _} =
      Campaign.mcp_call(@token, "complete_setup_merge", %{
        "idempotency_key" => "m",
        "base_sha" => workspace.source_sha,
        "setup_sha" => setup_sha,
        "best_sha" => best_sha,
        "target_snapshot_id" => Campaign.snapshot().target_snapshot.id
      })

    AlignmentFixtures.write_baseline_artifacts(workspace, best_sha, skill.sha, 4)
    manifest = AlignmentFixtures.write_baseline_manifest(workspace, best_sha, "insufficient")

    base_request = %{"manifest_artifact" => manifest}

    first_request = Map.put(base_request, "idempotency_key", "b1")
    second_request = Map.put(base_request, "idempotency_key", "b2")

    assert {:ok, %{status: "validating_baseline"}} =
             Campaign.mcp_call(@token, "submit_baseline", first_request)

    assert eventually(fn ->
             snapshot = Campaign.snapshot()
             snapshot.baseline_retry_count == 1 and is_nil(snapshot.baseline_progress)
           end)

    assert {:error, "missing_required_data", _, %{retry: 1}} =
             Campaign.mcp_call(@token, "submit_baseline", first_request)

    assert {:ok, %{status: "validating_baseline"}} =
             Campaign.mcp_call(@token, "submit_baseline", second_request)

    assert eventually(fn -> is_nil(Campaign.snapshot().baseline_progress) end)

    assert {:error, "blocked", _, _} =
             Campaign.mcp_call(@token, "submit_baseline", second_request)

    assert Campaign.snapshot().status == :building_baseline
    assert Campaign.snapshot().baseline_retry_count == 1
  end

  defp merge_setup(workspace) do
    setup_sha = Campaign.snapshot().prepared_setup_sha
    assert Git.run!(workspace.setup_worktree, ["rev-parse", "HEAD"]) == setup_sha
    assert Git.clean?(workspace.setup_worktree)
    Git.run!(workspace.repo, ["merge", "--squash", setup_sha])
    Git.run!(workspace.repo, ["commit", "-m", "Alignment setup"])
    {setup_sha, Git.run!(workspace.repo, ["rev-parse", "HEAD"])}
  end

  defp confirm_reviewed_spec do
    if is_nil(Campaign.snapshot().implementation_review_evidence) do
      {:ok, _} = submit_implementation_review()
    end

    {:ok, implementation_review} = Campaign.implementation_review()
    evidence = Campaign.snapshot().implementation_review_evidence

    Campaign.confirm_spec(
      implementation_review.target_snapshot.digest,
      evidence.development_sha,
      evidence.digest
    )
  end

  defp submit_implementation_review do
    root = Campaign.snapshot().workspace.root
    key = "campaign-review-#{System.unique_integer([:positive])}"
    AlignmentFixtures.submit_implementation_review(@token, %{root: root}, key)
  end

  defp question_args(id, question) do
    %{
      "id" => id,
      "question" => question,
      "options" => [%{"label" => "Yes"}, %{"label" => "No"}]
    }
  end

  defp eventually(fun, attempts \\ 50)

  defp eventually(fun, attempts) when attempts > 0 do
    if fun.() do
      true
    else
      Process.sleep(20)
      eventually(fun, attempts - 1)
    end
  end

  defp eventually(_fun, 0), do: false
end
