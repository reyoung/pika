defmodule Pika.Stage0.PromptCatalogTest do
  use ExUnit.Case, async: false

  alias Pika.Stage0.PromptCatalog

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
               selected_references: "cutlass: NVIDIA CUTLASS"
             })

    assert alignment =~ "latency"
    assert alignment =~ "min/max ranges"

    assert {:ok, setup} = PromptCatalog.render(:setup_merge, %{source_sha: "abc"})
    assert setup =~ "complete_setup_merge"

    assert {:ok, baseline} =
             PromptCatalog.render(:baseline, %{
               best_sha: "def",
               repo: "/tmp/repo",
               artifacts: "/tmp/artifacts",
               max_initial_cases: 10
             })

    assert baseline =~ "submit_iteration_sample"
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
