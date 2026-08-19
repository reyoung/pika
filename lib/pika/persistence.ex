defmodule Pika.Persistence do
  @moduledoc false

  import Ecto.Query

  alias Ecto.Multi
  alias Pika.Persistence.{AgentSession, Campaign, DomainEvent}
  alias Pika.{Repo, Workspace}

  @topic_prefix "pika:campaign:"

  def migrate do
    migrations = Application.app_dir(:pika, "priv/repo/migrations")
    compiler_options = Code.compiler_options()

    try do
      upgrade_campaign_status_check()
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

  defp upgrade_campaign_status_check do
    case Repo.query!("SELECT sql FROM sqlite_master WHERE type = 'table' AND name = 'campaigns'").rows do
      [] ->
        :ok

      [[sql]] ->
        if String.contains?(sql, "selecting_iteration_sample") do
          :ok
        else
          replacement =
            sql
            |> String.replace(
              ~r/CREATE TABLE\s+["`]?campaigns["`]?/i,
              "CREATE TABLE campaigns_status_upgrade"
            )
            |> String.replace(
              "'building_baseline','optimizing'",
              "'building_baseline','selecting_iteration_sample','optimizing'"
            )

          columns =
            Repo.query!("PRAGMA table_info(campaigns)").rows
            |> Enum.map(fn [_cid, name | _] -> ~s("#{name}") end)
            |> Enum.join(", ")

          Repo.query!("PRAGMA foreign_keys = OFF")

          result =
            Repo.transaction(fn ->
              Repo.query!(replacement)

              Repo.query!(
                "INSERT INTO campaigns_status_upgrade (#{columns}) SELECT #{columns} FROM campaigns"
              )

              Repo.query!("DROP TABLE campaigns")
              Repo.query!("ALTER TABLE campaigns_status_upgrade RENAME TO campaigns")

              Repo.query!(
                "CREATE UNIQUE INDEX campaigns_singleton_key_index ON campaigns(singleton_key)"
              )
            end)

          Repo.query!("PRAGMA foreign_keys = ON")

          case result do
            {:ok, _} -> :ok
            {:error, reason} -> raise "campaigns status upgrade failed: #{inspect(reason)}"
          end
        end
    end
  end

  def configure_connection do
    Repo.checkout(fn ->
      Repo.query!("PRAGMA foreign_keys = ON")
      Repo.query!("PRAGMA journal_mode = WAL")
      Repo.query!("PRAGMA synchronous = FULL")

      expected = %{
        "foreign_keys" => 1,
        "journal_mode" => "wal",
        "synchronous" => 2,
        "busy_timeout" => 5000
      }

      actual = pragma_values()
      if actual == expected, do: :ok, else: raise("unexpected SQLite pragmas: #{inspect(actual)}")
    end)
  end

  def pragma_values do
    %{
      "foreign_keys" => pragma("foreign_keys"),
      "journal_mode" => pragma("journal_mode"),
      "synchronous" => pragma("synchronous"),
      # Exqlite installs its own busy handler from this connection option. Querying
      # PRAGMA busy_timeout would replace that handler, so verify the configured value.
      "busy_timeout" => Repo.config()[:busy_timeout]
    }
  end

  def current_campaign, do: Repo.one(from(campaign in Campaign, limit: 1))

  def initialize_or_recover(%Workspace{} = workspace) do
    case current_campaign() do
      nil -> initialize_campaign(workspace)
      campaign -> recover_campaign(campaign, workspace)
    end
  end

  def transition_campaign(%Campaign{} = campaign, changes, event_type, payload) do
    now = now_us()
    event = event_changeset(campaign.id, event_type, payload, now)

    multi =
      Multi.new()
      |> Multi.update(:campaign, Campaign.changeset(campaign, Map.put(changes, :updated_at, now)))
      |> Multi.insert(:event, event)

    case Repo.transaction(multi) do
      {:ok, %{campaign: updated, event: persisted_event}} ->
        broadcast(updated.id, persisted_event)
        {:ok, updated, persisted_event}

      {:error, operation, reason, _changes} ->
        {:error, operation, reason}
    end
  end

  def topic(campaign_id), do: @topic_prefix <> campaign_id

  def recover_agent_sessions(campaign_id) do
    sessions =
      Repo.all(
        from(session in AgentSession,
          where:
            session.campaign_id == ^campaign_id and
              session.status in ["starting", "running", "awaiting_report"]
        )
      )

    Enum.reduce_while(sessions, {:ok, []}, fn session, {:ok, recovered} ->
      case interrupt_lost_session(session) do
        {:ok, :already_recovered} -> {:cont, {:ok, recovered}}
        {:ok, event} -> {:cont, {:ok, [event | recovered]}}
        {:error, reason} -> {:halt, {:error, reason}}
      end
    end)
  end

  defp initialize_campaign(workspace) do
    now = now_us()
    mutable = workspace.snapshot["mutable"]
    stop_mode = get_in(mutable, ["stop_conditions", "mode"]) || "all_goals"

    attrs = %{
      status: "drafting_spec",
      workspace_mode: Atom.to_string(workspace.mode),
      managed_repo_canonical_path: workspace.managed_repo,
      git_common_dir: workspace.git_common_dir,
      base_sha: workspace.base_sha,
      best_sha: best_sha(workspace),
      max_attempts: mutable["max_attempts"],
      plan_enabled: mutable["plan"],
      history_limit: mutable["history_n"],
      stop_mode: stop_mode,
      config_hash: workspace.config_hash,
      inserted_at: now,
      updated_at: now
    }

    campaign = Campaign.changeset(%Campaign{}, attrs)

    multi =
      Multi.new()
      |> Multi.insert(:campaign, campaign)
      |> Multi.run(:event, fn repo, %{campaign: persisted} ->
        repo.insert(
          event_changeset(
            persisted.id,
            "campaign_initialized",
            %{
              workspace_mode: persisted.workspace_mode,
              base_sha: persisted.base_sha,
              best_sha: persisted.best_sha
            },
            now
          )
        )
      end)

    case Repo.transaction(multi) do
      {:ok, %{campaign: persisted, event: event}} ->
        broadcast(persisted.id, event)
        {:ok, persisted, :initialized}

      {:error, operation, reason, _changes} ->
        {:error, {:campaign_initialization_failed, operation, reason}}
    end
  end

  defp recover_campaign(campaign, workspace) do
    with :ok <- verify_campaign_identity(campaign, workspace),
         {:ok, campaign} <- apply_mutable_config(campaign, workspace),
         {:ok, _events} <- recover_agent_sessions(campaign.id) do
      {:ok, campaign, :recovered}
    else
      {:error, {:identity_mismatch, _} = reason} -> block_campaign(campaign, reason)
      {:error, reason} -> {:error, reason}
    end
  end

  def block_campaign(%Campaign{} = campaign, reason) do
    reason_text = inspect(reason)
    idempotency_key = "startup-block:#{campaign.id}:#{sha256(reason_text)}"
    now = now_us()

    result =
      Repo.transaction(fn ->
        {inserted, _} =
          Repo.insert_all(
            "operation_intents",
            [
              %{
                id: Ecto.UUID.generate(),
                campaign_id: campaign.id,
                kind: "recovery",
                owner_type: "campaign",
                owner_id: campaign.id,
                state: "verified",
                idempotency_key: idempotency_key,
                payload_json: Jason.encode!(%{reason: reason_text}),
                created_at: now,
                updated_at: now
              }
            ],
            on_conflict: :nothing
          )

        if inserted == 0 or campaign.status == "blocked" do
          {:already_blocked, Repo.get!(Campaign, campaign.id)}
        else
          {:ok, blocked} =
            campaign
            |> Campaign.changeset(%{
              status: "blocked",
              resume_state: campaign.status,
              updated_at: now
            })
            |> Repo.update()

          {:ok, event} =
            Repo.insert(
              event_changeset(
                campaign.id,
                "campaign_blocked",
                %{reason: reason_text, recovery_idempotency_key: idempotency_key},
                now
              )
            )

          {:blocked, blocked, event}
        end
      end)

    case result do
      {:ok, {:blocked, blocked, event}} ->
        broadcast(blocked.id, event)
        {:ok, blocked, {:blocked, reason}}

      {:ok, {:already_blocked, blocked}} ->
        {:ok, blocked, {:blocked, reason}}

      {:error, error} ->
        {:error, {:block_campaign_failed, error}}
    end
  end

  defp verify_campaign_identity(campaign, workspace) do
    expected = %{
      workspace_mode: Atom.to_string(workspace.mode),
      managed_repo: workspace.managed_repo,
      git_common_dir: workspace.git_common_dir,
      base_sha: campaign.base_sha,
      best_sha: campaign.best_sha
    }

    actual = %{
      workspace_mode: campaign.workspace_mode,
      managed_repo: campaign.managed_repo_canonical_path,
      git_common_dir: campaign.git_common_dir,
      base_sha: workspace.base_sha,
      best_sha: best_sha(workspace)
    }

    if expected == actual,
      do: :ok,
      else: {:error, {:identity_mismatch, %{expected: expected, actual: actual}}}
  end

  defp apply_mutable_config(campaign, workspace) do
    mutable = workspace.snapshot["mutable"]
    stop_mode = get_in(mutable, ["stop_conditions", "mode"]) || "all_goals"

    attrs = %{
      config_hash: workspace.config_hash,
      plan_enabled: mutable["plan"],
      max_attempts: mutable["max_attempts"],
      history_limit: mutable["history_n"],
      stop_mode: stop_mode
    }

    changed? = Enum.any?(attrs, fn {key, value} -> Map.get(campaign, key) != value end)

    if changed? do
      case transition_campaign(campaign, attrs, "campaign_config_updated", attrs) do
        {:ok, updated, _event} -> {:ok, updated}
        {:error, operation, reason} -> {:error, {:config_update_failed, operation, reason}}
      end
    else
      {:ok, campaign}
    end
  end

  defp interrupt_lost_session(session) do
    key = "startup-recovery:#{session.id}:#{session.process_started_at || 0}"
    request_hash = sha256(key)
    now = now_us()

    Repo.transaction(fn ->
      {inserted, _} =
        Repo.insert_all(
          "idempotency_records",
          [
            %{
              backend_session_id: session.id,
              tool_name: "recover_lost_session",
              idempotency_key: key,
              request_sha256: request_hash,
              response_json: Jason.encode!(%{status: "interrupted"}),
              created_at: now
            }
          ],
          on_conflict: :nothing
        )

      if inserted == 0 do
        Repo.rollback(:already_recovered)
      else
        Repo.update_all(from(value in AgentSession, where: value.id == ^session.id),
          set: [status: "interrupted", ended_at: now, process_pid: nil]
        )

        {:ok, event} =
          Repo.insert(
            event_changeset(
              session.campaign_id,
              "agent_session_interrupted",
              %{
                session_id: session.id,
                recovery_idempotency_key: key
              },
              now
            )
          )

        event
      end
    end)
    |> case do
      {:ok, event} ->
        broadcast(session.campaign_id, event)
        {:ok, event}

      {:error, :already_recovered} ->
        {:ok, :already_recovered}

      {:error, reason} ->
        {:error, {:session_recovery_failed, session.id, reason}}
    end
  end

  defp best_sha(workspace) do
    case Pika.Git.run(workspace.repo, ["rev-parse", "refs/heads/pika/best"]) do
      {:ok, sha} -> sha
      {:error, _} -> workspace.base_sha
    end
  end

  defp event_changeset(campaign_id, type, payload, now) do
    DomainEvent.changeset(%DomainEvent{}, %{
      event_id: Ecto.UUID.generate(),
      aggregate_type: "campaign",
      aggregate_id: campaign_id,
      event_type: type,
      payload_json: Jason.encode!(payload),
      created_at: now
    })
  end

  defp broadcast(campaign_id, event) do
    Phoenix.PubSub.broadcast(Pika.PubSub, topic(campaign_id), {:domain_event, event})
  end

  defp pragma(name) do
    %{rows: [[value]]} = Repo.query!("PRAGMA #{name}")
    value
  end

  defp sha256(value), do: :crypto.hash(:sha256, value) |> Base.encode16(case: :lower)
  defp now_us, do: System.system_time(:microsecond)
end
