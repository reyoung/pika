defmodule Pika.MixProject do
  use Mix.Project

  def project do
    [
      app: :pika,
      version: "0.0.1",
      elixir: "~> 1.18",
      start_permanent: Mix.env() == :prod,
      deps: deps(),
      aliases: aliases(),
      releases: [pika: [steps: [:assemble, &Pika.Release.add_cli/1]]],
      test_ignore_filters: [&String.starts_with?(&1, "test/support/")]
    ]
  end

  def application do
    [
      mod: {Pika.Application, []},
      extra_applications: [:crypto, :logger, :inets, :ssl]
    ]
  end

  def cli do
    [
      preferred_envs: [
        check: :test,
        "test.integration.lifecycle": :test,
        "test.integration.contract": :test,
        "test.integration.scenario": :test
      ]
    ]
  end

  defp deps do
    [
      {:bandit, "~> 1.8"},
      {:ecto_sqlite3, "~> 0.24.1"},
      {:esbuild, "~> 0.10", runtime: Mix.env() == :dev},
      {:jason, "~> 1.4"},
      {:lazy_html, ">= 0.1.0", only: :test},
      {:mdex, "~> 0.13"},
      {:phoenix, "~> 1.8"},
      {:phoenix_html, "~> 4.2"},
      {:phoenix_live_view, "~> 1.1"},
      {:yaml_elixir, "~> 2.12"}
    ]
  end

  defp aliases do
    [
      "assets.build": ["esbuild default"],
      "test.integration.lifecycle": ["test test/pika/integration/lifecycle_test.exs"],
      "test.integration.contract": [
        "test test/pika/integration/lifecycle_test.exs test/pika/agent/integration_role_test.exs"
      ],
      "test.integration.scenario": ["test test/pika/integration_full_regression_test.exs"],
      check: ["format --check-formatted", "compile --warnings-as-errors", "test"]
    ]
  end
end
