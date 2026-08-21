defmodule Pika.ProgressSummaryCoordinator do
  @moduledoc false

  use GenServer

  alias Pika.{AgentBackend, ProgressSummaryStore, Repo, WorkspaceLock}

  def start_link(opts \\ []), do: GenServer.start_link(__MODULE__, opts, name: __MODULE__)

  @impl true
  def init(opts) do
    workspace = WorkspaceLock.workspace()
    campaign = Pika.Persistence.current_campaign()
    config = workspace.snapshot["mutable"]["progress_summary"] || %{"enabled" => false}
    interval = Keyword.get(opts, :interval_ms, config["interval_minutes"] * 60_000)

    state = %{
      campaign_id: campaign.id,
      workspace: workspace,
      config: config,
      interval: interval,
      timer: nil,
      run: nil,
      backend_modules: Keyword.get(opts, :backend_modules, %{})
    }

    {:ok, schedule(state)}
  end

  @impl true
  def handle_info(:summarize, %{run: nil} = state) do
    state = state |> Map.put(:timer, nil) |> reload_config()

    case {state.config["enabled"], active_context(state.campaign_id)} do
      {true, context} when not is_nil(context) -> {:noreply, start_summary(state, context)}
      _ -> {:noreply, schedule(state)}
    end
  end

  def handle_info(:summarize, state), do: {:noreply, state}

  def handle_info({:pika_backend_event, event}, %{run: run} = state) when not is_nil(run) do
    case event.type do
      :message_delta ->
        delta = event.data[:delta] || event.data["delta"] || ""
        {:noreply, put_in(state.run.content, run.content <> delta)}

      :turn_completed ->
        content = String.trim(run.content)
        if content != "", do: persist(state, run, content)
        Process.cancel_timer(run.timeout)
        close(run.handle)
        {:noreply, schedule(%{state | run: nil})}

      type when type in [:backend_error, :process_exited] ->
        Process.cancel_timer(run.timeout)
        close(run.handle)
        {:noreply, schedule(%{state | run: nil})}

      _ ->
        {:noreply, state}
    end
  end

  def handle_info(:summary_timeout, %{run: run} = state) when not is_nil(run) do
    close(run.handle)
    {:noreply, schedule(%{state | run: nil})}
  end

  def handle_info(_message, state), do: {:noreply, state}

  @impl true
  def terminate(_reason, %{run: %{handle: handle}}), do: close(handle)
  def terminate(_reason, _state), do: :ok

  defp schedule(%{config: %{"enabled" => true}} = state) do
    if state.timer, do: Process.cancel_timer(state.timer)
    %{state | timer: Process.send_after(self(), :summarize, state.interval)}
  end

  defp schedule(state), do: state

  defp reload_config(state) do
    case Pika.RuntimeConfig.profile(state.workspace, "progress_summary") do
      {:ok, config} ->
        %{state | config: config, interval: config["interval_minutes"] * 60_000}

      {:error, _reason} ->
        state
    end
  end

  defp start_summary(state, context) do
    profile = state.config
    backend = if profile["backend"] == "cursor_acp", do: :cursor_acp, else: :codex_app_server
    module = Map.get(state.backend_modules, backend, backend_module(backend))
    {command, args} = backend_command(backend, profile["command"])

    backend_profile = %{
      backend: backend,
      command: command,
      args: args,
      approval_policy: profile["approval_policy"],
      sandbox_policy: profile["sandbox_policy"],
      env: profile["env"] || %{},
      protocol_config: profile["protocol_config"] || %{},
      artifact_dir: Path.join([state.workspace.artifacts, "logs", "progress-summary"])
    }

    instructions = """
    You are Pika's read-only Progress Summary Agent. Summarize only the supplied snapshot. Do not
    modify files, run commands, use tools, or propose changes as completed work. Be concise and
    factual. Report current phase, completed work, active work, blockers/errors, measurements when
    present, and the next expected milestone. Clearly distinguish observation from inference.
    """

    with {:ok, handle} <- AgentBackend.start_link(module, backend_profile, self()),
         {:ok, session} <-
           AgentBackend.open_session(
             handle,
             state.workspace.repo,
             profile["model"],
             profile["reasoning_effort"],
             %{enabled: false},
             [],
             instructions
           ),
         {:ok, _turn_id} <-
           AgentBackend.start_turn(
             handle,
             "Summarize this Pika progress snapshot:\n\n" <> Jason.encode!(context, pretty: true)
           ) do
      timeout = Process.send_after(self(), :summary_timeout, min(state.interval, 5 * 60_000))

      %{
        state
        | run: %{
            handle: handle,
            session: session,
            context: context,
            content: "",
            timeout: timeout
          }
      }
    else
      _error -> schedule(state)
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

  defp persist(state, run, content) do
    ProgressSummaryStore.insert(state.campaign_id, %{
      phase: run.context.phase,
      attempt_ids: Enum.map(run.context.active_attempts, & &1.id),
      backend: to_string(run.session.backend),
      model: run.session.model,
      reasoning_effort: to_string(run.session.reasoning_effort || ""),
      content: content
    })
  end

  defp backend_command(_backend, [command, "app-server", "--listen", "stdio://"]),
    do: {command, []}

  defp backend_command(_backend, [command, "acp"]), do: {command, []}
  defp backend_command(_backend, [command | args]), do: {command, args}
  defp backend_command(:cursor_acp, nil), do: {"cursor-agent", []}
  defp backend_command(_, nil), do: {"codex", []}
  defp backend_module(:cursor_acp), do: Pika.AgentBackend.CursorACP
  defp backend_module(_), do: Pika.AgentBackend.CodexAppServer

  defp close(handle) do
    try do
      AgentBackend.close_session(handle)
    catch
      _, _ -> :ok
    end
  end
end
