defmodule Pika.Optimization.Persistence do
  @moduledoc "Fresh v2 SQLite schema and singleton Optimization initialization/recovery."

  alias Pika.Optimization.Config
  alias Pika.Repo

  @optimization_id "optimization"
  @sha_pattern ~r/^[0-9a-f]{40}([0-9a-f]{24})?$/

  @spec migrate() :: :ok | {:error, {:migration_failed, term()}}
  def migrate do
    migrations = Application.app_dir(:pika, "priv/v2/migrations")
    compiler_options = Code.compiler_options()

    try do
      Code.compiler_options(ignore_module_conflict: true)
      Ecto.Migrator.run(Repo, migrations, :up, all: true)
      configure_connection()
      :ok
    rescue
      exception -> {:error, {:migration_failed, Exception.message(exception)}}
    catch
      kind, reason -> {:error, {:migration_failed, {kind, reason}}}
    after
      Code.compiler_options(compiler_options)
    end
  end

  @spec configure_connection() :: :ok
  def configure_connection do
    Repo.checkout(fn ->
      Repo.query!("PRAGMA foreign_keys = ON")
      Repo.query!("PRAGMA journal_mode = WAL")
      Repo.query!("PRAGMA synchronous = FULL")

      expected = %{
        foreign_keys: 1,
        journal_mode: "wal",
        synchronous: 2,
        busy_timeout: 5_000
      }

      actual = pragma_values()

      if actual == expected,
        do: :ok,
        else: raise("unexpected v2 SQLite pragmas: #{inspect(actual)}")
    end)
  end

  @spec pragma_values() :: map()
  def pragma_values do
    %{
      foreign_keys: pragma("foreign_keys"),
      journal_mode: pragma("journal_mode"),
      synchronous: pragma("synchronous"),
      busy_timeout: Repo.config()[:busy_timeout]
    }
  end

  @spec initialize_or_recover(Config.t(), String.t()) ::
          {:ok, map(), :initialized | :recovered} | {:error, term()}
  def initialize_or_recover(%Config{} = config, initial_sha) when is_binary(initial_sha) do
    with :ok <- validate_identity(config, initial_sha) do
      case current() do
        nil -> initialize(config, initial_sha)
        optimization -> recover(optimization, config, initial_sha)
      end
    end
  end

  @spec current() :: map() | nil
  def current do
    case Repo.query!("""
         SELECT id, status, resume_status, repo_canonical_path, workspace_canonical_path,
                initial_sha, best_branch, best_sha, stop_reason, config_sha256,
                inserted_at, updated_at
         FROM optimizations WHERE singleton_key = 1 LIMIT 1
         """).rows do
      [row] -> optimization(row)
      [] -> nil
    end
  end

  @spec allocate_attempt_id() :: {:ok, pos_integer()} | {:error, term()}
  def allocate_attempt_id do
    case Repo.transaction(fn ->
           case Repo.query!(
                  "SELECT next_attempt_id FROM optimizations WHERE id = ?",
                  [@optimization_id]
                ).rows do
             [[id]] when is_integer(id) and id > 0 ->
               Repo.query!(
                 "UPDATE optimizations SET next_attempt_id = ?, updated_at = ? WHERE id = ?",
                 [id + 1, now_us(), @optimization_id]
               )

               id

             [] ->
               Repo.rollback(:optimization_not_initialized)
           end
         end) do
      {:ok, id} -> {:ok, id}
      {:error, reason} -> {:error, reason}
    end
  end

  defp initialize(config, initial_sha) do
    now = now_us()
    config_sha256 = config_sha256(config)

    result =
      Repo.transaction(fn ->
        Repo.query!(
          """
          INSERT INTO optimizations(
            id, singleton_key, status, repo_canonical_path, workspace_canonical_path,
            initial_sha, best_branch, config_sha256, inserted_at, updated_at
          ) VALUES (?, 1, 'aligning_baseline', ?, ?, ?, 'pika/best', ?, ?, ?)
          """,
          [
            @optimization_id,
            config.repo,
            config.workspace,
            initial_sha,
            config_sha256,
            now,
            now
          ]
        )

        append_event(
          "optimization",
          @optimization_id,
          "optimization_initialized",
          %{repo: config.repo, workspace: config.workspace, initial_sha: initial_sha},
          now
        )

        current()
      end)

    case result do
      {:ok, optimization} -> {:ok, optimization, :initialized}
      {:error, reason} -> {:error, {:optimization_initialization_failed, reason}}
    end
  end

  defp recover(optimization, config, initial_sha) do
    expected = %{
      repo: optimization.repo_canonical_path,
      workspace: optimization.workspace_canonical_path,
      initial_sha: optimization.initial_sha
    }

    actual = %{repo: config.repo, workspace: config.workspace, initial_sha: initial_sha}

    if expected == actual do
      now = now_us()

      Repo.query!(
        "UPDATE optimizations SET config_sha256 = ?, updated_at = ? WHERE id = ?",
        [config_sha256(config), now, @optimization_id]
      )

      {:ok, current(), :recovered}
    else
      {:error, {:optimization_identity_mismatch, %{expected: expected, actual: actual}}}
    end
  end

  defp validate_identity(config, initial_sha) do
    cond do
      not File.dir?(config.repo) ->
        {:error, {:repo_not_directory, config.repo}}

      not File.dir?(config.workspace) ->
        {:error, {:workspace_not_directory, config.workspace}}

      not Regex.match?(@sha_pattern, initial_sha) ->
        {:error, {:invalid_initial_sha, initial_sha}}

      true ->
        :ok
    end
  end

  defp append_event(aggregate_type, aggregate_id, event_type, payload, now) do
    Repo.query!(
      """
      INSERT INTO domain_events(
        event_id, optimization_id, aggregate_type, aggregate_id, event_type, payload_json, created_at
      ) VALUES (?, ?, ?, ?, ?, ?, ?)
      """,
      [
        Ecto.UUID.generate(),
        @optimization_id,
        aggregate_type,
        aggregate_id,
        event_type,
        Jason.encode!(payload),
        now
      ]
    )
  end

  defp optimization([
         id,
         status,
         resume_status,
         repo,
         workspace,
         initial_sha,
         best_branch,
         best_sha,
         stop_reason,
         config_sha256,
         inserted_at,
         updated_at
       ]) do
    %{
      id: id,
      status: status,
      resume_status: resume_status,
      repo_canonical_path: repo,
      workspace_canonical_path: workspace,
      initial_sha: initial_sha,
      best_branch: best_branch,
      best_sha: best_sha,
      stop_reason: stop_reason,
      config_sha256: config_sha256,
      inserted_at: inserted_at,
      updated_at: updated_at
    }
  end

  defp config_sha256(config) do
    config.source_path
    |> File.read!()
    |> then(&:crypto.hash(:sha256, &1))
    |> Base.encode16(case: :lower)
  end

  defp pragma(name) do
    [[value]] = Repo.query!("PRAGMA #{name}").rows
    value
  end

  defp now_us, do: System.system_time(:microsecond)
end
