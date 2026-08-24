defmodule Pika.Optimization.ConfigFileTest do
  use ExUnit.Case, async: true

  alias Pika.Optimization.{Config, ConfigFile}
  alias Pika.Optimization.Config.{ProgressSummary, Role}

  test "canonical rendering preserves expanded Backend fields and Role controls" do
    root = Path.join(System.tmp_dir!(), "pika-config-file-#{System.unique_integer([:positive])}")
    path = Path.join(root, "pika.yaml")
    File.mkdir_p!(root)
    on_exit(fn -> File.rm_rf!(root) end)

    config = ConfigFile.new(path, Path.join(root, "repo"), root)

    rich_agent = %{
      config.baseline_alignment.agent
      | command: ["codex", "app-server"],
        model: "writer-model",
        reasoning_effort: "xhigh",
        env: %{"PIKA_TEST" => "configured"},
        protocol_config: %{"nested" => %{"enabled" => true}}
    }

    followup = %Role{agent: %{rich_agent | reasoning_effort: "medium"}, generator_max_attempts: 7}

    summary = %ProgressSummary{
      agent: %{rich_agent | reasoning_effort: "low", sandbox: "read_only"},
      interval_ms: 90_000,
      time_zone: "UTC",
      max_followups: 4
    }

    configured = %{
      config
      | baseline_alignment: %{config.baseline_alignment | agent: rich_agent},
        baseline_verify_followup: followup,
        iteration_followup: followup,
        integration_followup: followup,
        progress_summary: summary
    }

    File.write!(path, ConfigFile.render(configured))
    assert {:ok, loaded} = Config.load(path)

    assert Config.Agent.snapshot(loaded.baseline_alignment.agent) ==
             Config.Agent.snapshot(rich_agent)

    assert loaded.baseline_verify_followup.generator_max_attempts == 7
    assert loaded.iteration_followup.generator_max_attempts == 7
    assert loaded.integration_followup.generator_max_attempts == 7
    assert loaded.progress_summary.interval_ms == 90_000
    assert loaded.progress_summary.time_zone == "UTC"
    assert loaded.progress_summary.max_followups == 4
  end
end
