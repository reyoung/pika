defmodule Pika.ProgressSummaryCoordinator do
  @moduledoc "Periodic scheduler that creates durable Progress Summary Work for Agent Symphony."

  use GenServer

  alias Pika.{ProgressSummaryStore, Repo, WorkspaceLock}

  def start_link(opts \\ []) do
    case Keyword.get(opts, :name, __MODULE__) do
      nil -> GenServer.start_link(__MODULE__, opts)
      name -> GenServer.start_link(__MODULE__, opts, name: name)
    end
  end

  def trigger(server \\ __MODULE__), do: GenServer.call(server, :summarize, :infinity)

  @impl true
  def init(opts) do
    workspace = Keyword.get_lazy(opts, :workspace, &WorkspaceLock.workspace/0)

    campaign_id =
      Keyword.get_lazy(opts, :campaign_id, fn -> Pika.Persistence.current_campaign().id end)

    config = workspace.snapshot["mutable"]["progress_summary"] || %{"enabled" => false}
    configured_interval = config["interval_minutes"] * 60_000

    state = %{
      campaign_id: campaign_id,
      workspace: workspace,
      config: config,
      interval: Keyword.get(opts, :interval_ms, configured_interval),
      timer: nil,
      symphony: Keyword.get(opts, :symphony, Pika.Agent.Symphony),
      last_error: nil
    }

    {:ok, schedule(state)}
  end

  @impl true
  def handle_call(:summarize, _from, state) do
    state = state |> cancel_timer() |> reload_config() |> create_request()
    {:reply, :ok, schedule(state)}
  end

  @impl true
  def handle_info(:summarize, state) do
    state = state |> Map.put(:timer, nil) |> reload_config() |> create_request()
    {:noreply, schedule(state)}
  end

  def handle_info(_message, state), do: {:noreply, state}

  defp create_request(%{config: %{"enabled" => true}} = state) do
    case active_context(state.campaign_id) do
      nil ->
        state

      context ->
        case ProgressSummaryStore.request(state.campaign_id, context, state.config) do
          {:ok, _request} ->
            reconcile(state.symphony)
            %{state | last_error: nil}

          {:error, reason} ->
            %{state | last_error: reason}
        end
    end
  end

  defp create_request(state), do: state

  defp reload_config(state) do
    case Pika.RuntimeConfig.profile(state.workspace, "progress_summary") do
      {:ok, config} ->
        interval =
          if state.interval == :infinity,
            do: :infinity,
            else: config["interval_minutes"] * 60_000

        %{state | config: config, interval: interval, last_error: nil}

      {:error, reason} ->
        %{state | last_error: reason}
    end
  end

  defp active_context(campaign_id) do
    campaign =
      case Repo.query!("SELECT status, best_sha, attempts_created FROM campaigns WHERE id = ?", [
             campaign_id
           ]).rows do
        [[status, best_sha, count]] ->
          %{status: status, best_sha: best_sha, attempts_created: count}

        [] ->
          nil
      end

    attempts =
      Repo.query!(
        "SELECT id, ordinal, status, base_sha, candidate_sha, description, summary, outcome_reason FROM attempts WHERE campaign_id = ? ORDER BY ordinal DESC LIMIT 20",
        [campaign_id]
      ).rows
      |> Enum.map(fn [id, ordinal, status, base, candidate, description, summary, outcome] ->
        %{
          id: id,
          ordinal: ordinal,
          status: status,
          base_sha: base,
          candidate_sha: candidate,
          description: description,
          summary: summary,
          outcome_reason: outcome
        }
      end)

    active =
      Enum.filter(
        attempts,
        &(&1.status in ~w(queued running awaiting_report refreshing ready_for_integration integrating interrupted))
      )

    phase =
      cond do
        campaign && campaign.status == "building_baseline" -> "baseline"
        active != [] -> "attempts"
        true -> nil
      end

    if phase do
      events =
        Pika.AttemptStore.events(campaign_id, 0, 100)
        |> Enum.take(-30)
        |> Enum.map(&Map.take(&1, [:sequence, :event_type, :payload, :created_at]))

      %{
        phase: phase,
        campaign: campaign,
        active_attempts: active,
        recent_attempts: attempts,
        recent_events: events,
        observed_at: DateTime.utc_now() |> DateTime.to_iso8601()
      }
    end
  end

  defp reconcile(server) do
    Pika.Agent.Symphony.reconcile(server)
  catch
    :exit, _reason -> :ok
  end

  defp schedule(%{interval: :infinity} = state), do: state
  defp schedule(%{config: %{"enabled" => false}} = state), do: state

  defp schedule(state) do
    if state.timer,
      do: state,
      else: %{state | timer: Process.send_after(self(), :summarize, state.interval)}
  end

  defp cancel_timer(%{timer: nil} = state), do: state

  defp cancel_timer(state) do
    Process.cancel_timer(state.timer)
    %{state | timer: nil}
  end
end
