defmodule Pika.PromptCatalogTest do
  use ExUnit.Case, async: false

  alias Pika.PromptCatalog

  setup do
    original = Application.get_env(:pika, PromptCatalog)
    on_exit(fn -> Application.put_env(:pika, PromptCatalog, original) end)
    :ok
  end

  test "validates and renders all configured default resources" do
    assert :ok = PromptCatalog.validate()

    assert {:ok, alignment} =
             PromptCatalog.render(:alignment, %{
               setup_worktree: "/tmp/setup",
               source_sha: "abc",
               revision: 1,
               setup_branch: "pika/setup/1"
             })

    assert alignment =~ "latency"
    assert alignment =~ "min/max ranges"
    assert alignment =~ "user-initiated Turn"
    assert alignment =~ "ask_questions"
    assert alignment =~ "required user decisions"
    assert alignment =~ "formal_pair_protocol"

    assert alignment =~
             "It is not a Case count, dataset size, record count, batch size or shape count"

    assert alignment =~ "Case count × Metric count × pair_count"
    assert alignment =~ "never change pair_count when the user only corrects the number of Cases"
    assert alignment =~ "Never invent, infer or apply defaults for either"
    assert alignment =~ "Implementation Review Evidence"
    assert alignment =~ "Both must pass the Oracle on the same inputs"
    assert alignment =~ "register its canonical Workspace-relative path with kind"
    assert alignment =~ "`implementation_review_evidence`"
    assert alignment =~ "artifacts/implementation-review/"
    assert alignment =~ "submit_implementation_bundle"
    assert alignment =~ "submit_implementation_review"
    assert alignment =~ "not the full Baseline"
    refute alignment =~ "call ask_question with"

    assert {:ok, setup} =
             PromptCatalog.render(:setup_merge, %{
               source_sha: "abc",
               revision: 1,
               setup_branch: "pika/setup/1"
             })

    assert setup =~ "complete_setup_merge"

    assert {:ok, baseline} =
             PromptCatalog.render(:baseline, %{
               best_sha: "def",
               repo: "/tmp/repo",
               artifacts: "/tmp/artifacts",
               target_snapshot_id: "target-fixture",
               target_entrypoint: "kernel/reference.py",
               development_entrypoint: "kernel/development.py",
               pair_count: 8,
               min_valid_pairs: 6,
               max_initial_cases: 10
             })

    assert baseline =~ "submit_iteration_sample"
    assert baseline =~ "exactly 8 genuinely"
    assert baseline =~ "Target/Candidate pairs"
    assert baseline =~ "at least 6 valid pairs"
    assert baseline =~ "genuinely independent A/B execution"
    assert baseline =~ "Never duplicate, relabel, interpolate or"
    assert baseline =~ "do not manufacture a larger JSONL"
    assert baseline =~ "call reopen_baseline_definition exactly once"
    assert baseline =~ "do not ask for revision only in prose"

    benchmark = %{"pair_count" => 8, "min_valid_pairs" => 6}

    attempt = %{
      id: "attempt-id",
      ordinal: 1,
      slot_index: 0,
      base_sha: "abc",
      sampling_revision_id: "sampling-id",
      plan_artifact_id: nil
    }

    context = %{
      protected_paths: ["kernel/bench.py"],
      spec: %{"benchmark" => benchmark, "stopping" => %{}},
      target_snapshot: %{id: "target-fixture", entrypoint: "kernel/reference.py"},
      cases: [],
      sampled_case_ids: []
    }

    assert :ok = PromptCatalog.validate([:iteration, :integration, :sync])

    assert {:ok, iteration} =
             PromptCatalog.render(:iteration, %{
               attempt: attempt,
               context: context,
               history: [],
               guidance: [],
               important_events: []
             })

    assert iteration =~ "exactly 8 alternating"
    assert iteration =~ ~r/at least\s+6 valid Pairs/

    assert {:ok, integration} =
             PromptCatalog.render(:integration, %{
               attempt: attempt,
               context: context,
               best_worktree: "/tmp/repo",
               patch_path: "/tmp/candidate.patch"
             })

    assert integration =~ ~r/independent\s+8 Pair run/

    assert {:ok, sync} =
             PromptCatalog.render(:sync, %{
               sync_run: %{"id" => "sync-id"},
               context: %{"spec" => %{"benchmark" => benchmark}}
             })

    assert sync =~ "exactly 8 alternating"

    for validation_prompt <- [baseline, iteration, integration, sync] do
      assert validation_prompt =~ "one long-lived batch driver"
      assert validation_prompt =~ "all Cases and Pair indexes"
      assert validation_prompt =~ "for-loop"
      assert validation_prompt =~ "Never start a fresh Python process for each Case"
      assert validation_prompt =~ "at most one persistent worker process per implementation"
      assert validation_prompt =~ "not each individual sample"
    end

    for execution_prompt <- [alignment, baseline, iteration, integration, sync] do
      assert execution_prompt =~ "uv venv .venv"
      assert execution_prompt =~ "uv pip install --python .venv/bin/python"
      assert execution_prompt =~ "`uv pip install --system`"
      assert execution_prompt =~ "global Python environment"
      assert execution_prompt =~ "dependency manifests or"
    end
  end

  test "supports absolute Config overrides and reports invalid templates" do
    root = Path.join(System.tmp_dir!(), "pika-prompt-#{System.unique_integer([:positive])}")
    File.mkdir_p!(root)
    valid = Path.join(root, "valid.md.eex")
    invalid = Path.join(root, "invalid.md.eex")
    File.write!(valid, "hello <%= @name %>")
    File.write!(invalid, "<%= if true do %>broken")

    Application.put_env(:pika, PromptCatalog,
      alignment: valid,
      setup_merge: valid,
      baseline: valid
    )

    assert :ok = PromptCatalog.validate()
    assert {:ok, "hello Pika"} = PromptCatalog.render(:alignment, %{name: "Pika"})

    Application.put_env(:pika, PromptCatalog,
      alignment: invalid,
      setup_merge: valid,
      baseline: valid
    )

    assert {:error, {:prompt_resource_invalid, :alignment, {:compile_failed, _}}} =
             PromptCatalog.validate()
  end
end
