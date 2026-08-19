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
    assert Campaign.snapshot().status == :awaiting_confirmation
    assert :ok = Campaign.confirm_spec()
    assert eventually(fn -> Campaign.snapshot().status == :building_baseline end)

    {setup_sha, best_sha} = merge_setup(workspace)

    assert {:ok, %{best_sha: ^best_sha}} =
             Campaign.mcp_call(@token, "complete_setup_merge", %{
               "idempotency_key" => "merge-1",
               "base_sha" => workspace.source_sha,
               "setup_sha" => setup_sha,
               "best_sha" => best_sha
             })

    [samples, correctness, profiler] =
      AlignmentFixtures.write_baseline_artifacts(workspace, best_sha, skill.sha)

    Enum.each(
      [samples, correctness, profiler] ++ AlignmentFixtures.baseline_dependency_paths(),
      &register_artifact(&1, workspace)
    )

    assert {:ok, %{status: "selecting_iteration_sample", metrics: [metric]}} =
             Campaign.mcp_call(@token, "submit_baseline", %{
               "idempotency_key" => "baseline-1",
               "measured_sha" => best_sha,
               "samples_artifact" => samples,
               "correctness_artifact" => correctness,
               "profiler_artifact" => profiler,
               "summary" => "fixture baseline"
             })

    assert metric.valid_pair_count == 30
    snapshot = Campaign.snapshot()
    assert snapshot.status == :selecting_iteration_sample
    assert snapshot.required_operations == ["submit_iteration_sample"]

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
    assert {:error, :spec_not_confirmable} = Campaign.confirm_spec()
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

    assert Campaign.snapshot().status == :awaiting_confirmation
    assert :ok = Campaign.request_changes("目标 Case 需要改成 n=2048")
    snapshot = Campaign.snapshot()
    assert snapshot.status == :drafting_spec
    assert snapshot.required_operations == ["submit_harness", "submit_spec"]
    refute snapshot.status == :building_baseline
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

    :ok = Campaign.confirm_spec()
    assert eventually(fn -> Campaign.snapshot().status == :building_baseline end)
    {setup_sha, best_sha} = merge_setup(workspace)

    {:ok, _} =
      Campaign.mcp_call(@token, "complete_setup_merge", %{
        "idempotency_key" => "m",
        "base_sha" => workspace.source_sha,
        "setup_sha" => setup_sha,
        "best_sha" => best_sha
      })

    [samples, correctness, profiler] =
      AlignmentFixtures.write_baseline_artifacts(workspace, best_sha, skill.sha, 23)

    Enum.each(
      [samples, correctness, profiler] ++ AlignmentFixtures.baseline_dependency_paths(),
      &register_artifact(&1, workspace)
    )

    base_request = %{
      "measured_sha" => best_sha,
      "samples_artifact" => samples,
      "correctness_artifact" => correctness,
      "profiler_artifact" => profiler,
      "summary" => "insufficient"
    }

    assert {:error, "missing_required_data", _, %{retry: 1}} =
             Campaign.mcp_call(
               @token,
               "submit_baseline",
               Map.put(base_request, "idempotency_key", "b1")
             )

    assert {:error, "blocked", _, _} =
             Campaign.mcp_call(
               @token,
               "submit_baseline",
               Map.put(base_request, "idempotency_key", "b2")
             )

    assert Campaign.snapshot().status == :building_baseline
    assert Campaign.snapshot().baseline_retry_count == 1
  end

  defp merge_setup(workspace) do
    Git.run!(workspace.setup_worktree, ["add", "."])
    Git.run!(workspace.setup_worktree, ["commit", "-m", "alignment setup"])
    setup_sha = Git.run!(workspace.setup_worktree, ["rev-parse", "HEAD"])
    Git.run!(workspace.repo, ["merge", "--squash", setup_sha])
    Git.run!(workspace.repo, ["commit", "-m", "Alignment setup"])
    {setup_sha, Git.run!(workspace.repo, ["rev-parse", "HEAD"])}
  end

  defp register_artifact(relative, workspace) do
    {:ok, artifact} = ArtifactStore.register(workspace.root, relative)

    assert {:ok, _} =
             Campaign.mcp_call(@token, "register_artifact", %{
               "idempotency_key" => "artifact-#{relative}",
               "kind" => "baseline",
               "relative_path" => relative,
               "sha256" => artifact.sha256,
               "size" => artifact.size,
               "mime" => artifact.mime,
               "metadata" => %{}
             })
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
