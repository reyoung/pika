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
               source_sha: "abc"
             })

    assert alignment =~ "latency"
    assert alignment =~ "min/max ranges"
    assert alignment =~ "user-initiated Turn"
    assert alignment =~ "ask_questions"
    refute alignment =~ "call ask_question with"

    assert {:ok, setup} = PromptCatalog.render(:setup_merge, %{source_sha: "abc"})
    assert setup =~ "complete_setup_merge"

    assert {:ok, baseline} =
             PromptCatalog.render(:baseline, %{
               best_sha: "def",
               repo: "/tmp/repo",
               artifacts: "/tmp/artifacts",
               pair_count: 8,
               min_valid_pairs: 6,
               max_initial_cases: 10
             })

    assert baseline =~ "submit_iteration_sample"
    assert baseline =~ "exactly 8 alternating self-pairs"
    assert baseline =~ "at least 6 valid pairs"

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
    assert iteration =~ "at least 6 valid Pairs"

    assert {:ok, integration} =
             PromptCatalog.render(:integration, %{
               attempt: attempt,
               context: context,
               best_worktree: "/tmp/repo",
               patch_path: "/tmp/candidate.patch"
             })

    assert integration =~ "independent 8 Pair run"

    assert {:ok, sync} =
             PromptCatalog.render(:sync, %{
               sync_run: %{"id" => "sync-id"},
               context: %{"spec" => %{"benchmark" => benchmark}}
             })

    assert sync =~ "exactly 8 alternating Best metric pairs"
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
