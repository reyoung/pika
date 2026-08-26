defmodule Pika.Agent.Actor do
  @moduledoc "Ephemeral executor for exactly one v2 Work and one fresh Backend Session."

  use GenServer, restart: :temporary

  alias Pika.Agent.{
    BackendConfig,
    CommandRouter,
    ContextBundle,
    ConversationJournal,
    PromptBuilder,
    RecoveryContext,
    SessionBinding,
    ToolActivity,
    Directory,
    Work,
    WorkProjector,
    Workspace
  }

  alias Pika.AgentBackend
  alias Pika.Followup.Lifecycle, as: FollowupLifecycle
  alias Pika.Optimization.{RoleRegistry, RuntimeConfig}
  alias Pika.ProgressSummary.Lifecycle, as: ProgressSummaryLifecycle

  @stream_flush_ms 100

  def start_link(opts), do: GenServer.start_link(__MODULE__, opts)

  def invoke(actor, operation, arguments),
    do: GenServer.call(actor, {:invoke, operation, arguments}, :infinity)

  def status(actor), do: GenServer.call(actor, :status)
  def kickoff(actor, message), do: GenServer.call(actor, {:kickoff, message}, :infinity)
  def stop(actor), do: GenServer.call(actor, :stop, :infinity)

  def deliver_followup(actor, request_id, message),
    do: GenServer.call(actor, {:deliver_followup, request_id, message}, :infinity)

  @impl true
  def init(opts) do
    Process.flag(:trap_exit, true)
    work = Keyword.fetch!(opts, :work)

    state = %{
      work: work,
      opts: opts,
      config: nil,
      paths: nil,
      bundle: nil,
      prompt: nil,
      pika_session: nil,
      provider_session: nil,
      agent: nil,
      handle: nil,
      monitor: nil,
      token: nil,
      directory: Keyword.get(opts, :directory, Directory),
      phase: :preparing,
      turn_db_id: nil,
      active_provider_turn_id: nil,
      output_messages: [],
      output_flushed_at_ms: nil,
      terminal_called: false,
      pending_question_continuation: nil,
      active_followup_request_id: nil,
      closed?: false
    }

    {:ok, state, {:continue, :open}}
  end

  @impl true
  def handle_continue(:open, state) do
    case open(state) do
      {:ok, state} ->
        {:noreply, state}

      {:error, reason, state} ->
        notify(state, {:agent_actor_failed, state.work, reason})
        {:stop, {:shutdown, reason}, cleanup(state, "failed", inspect(reason))}
    end
  end

  @impl true
  def handle_call(:status, _from, state) do
    {:reply,
     %{
       work: state.work,
       phase: state.phase,
       session_id: state.pika_session && state.pika_session.id,
       provider_session_id: state.provider_session && state.provider_session.backend_session_id
     }, state}
  end

  def handle_call({:kickoff, message}, _from, %{phase: phase} = state)
      when phase in [:awaiting_user_kickoff, :awaiting_user] and is_binary(message) do
    case start_turn(state, message) do
      {:ok, state} -> {:reply, :ok, state}
      {:error, reason, state} -> {:reply, {:error, reason}, state}
    end
  end

  def handle_call({:kickoff, message}, _from, %{phase: :running} = state)
      when is_binary(message) do
    case steer_turn(state, message) do
      {:ok, state} -> {:reply, :ok, state}
      {:error, reason, state} -> {:reply, {:error, reason}, state}
    end
  end

  def handle_call({:kickoff, _message}, _from, state),
    do: {:reply, {:error, {:actor_not_awaiting_user, state.phase}}, state}

  def handle_call({:invoke, operation, arguments}, _from, state) do
    result =
      CommandRouter.invoke(
        session_binding(state),
        operation,
        stringify_keys(arguments),
        state.config,
        question_handler: Keyword.get(state.opts, :question_handler),
        now: Keyword.get(state.opts, :now, DateTime.utc_now())
      )

    state = record_mcp(state, operation, result)

    state =
      case result do
        {:ok, _value} ->
          if terminal_operation?(state.work.role_id, operation) do
            send(self(), :finish_terminal)
            %{state | terminal_called: true}
          else
            state
          end

        {:error, _reason} ->
          state
      end

    {:reply, result, state}
  end

  def handle_call({:deliver_followup, request_id, message}, _from, state) do
    with true <- state.phase == :awaiting_followup || {:error, :target_not_awaiting_followup},
         {:ok, _request} <- FollowupLifecycle.target_turn_started(request_id),
         {:ok, state} <- start_turn(state, message) do
      {:reply, :ok, %{state | active_followup_request_id: request_id}}
    else
      {:error, reason} -> {:reply, {:error, reason}, state}
    end
  end

  def handle_call(:stop, _from, state) do
    state = interrupt(state, :actor_stopped)
    {:stop, :normal, :ok, state}
  end

  @impl true
  def handle_info(:finish_terminal, state) do
    if WorkProjector.terminal?(state.work) do
      if state.active_followup_request_id,
        do: FollowupLifecycle.target_turn_terminal(state.active_followup_request_id)

      state = maybe_deliver_generated_followup(state)
      notify(state, {:agent_actor_completed, state.work})
      {:stop, :normal, cleanup(state, "completed", "terminal_mcp")}
    else
      {:noreply, state}
    end
  end

  def handle_info(
        {:pika_baseline_questions_answered, _batch_id, _session_id, _answers},
        %{closed?: true} = state
      ),
      do: {:noreply, state}

  def handle_info(
        {:pika_baseline_questions_answered, batch_id, session_id, answers},
        state
      ) do
    if state.work.role_id == "baseline_alignment" and state.pika_session.id == session_id do
      message = question_continuation_message(batch_id, answers)

      case state.phase do
        :running ->
          {:noreply, %{state | pending_question_continuation: message}}

        phase when phase in [:awaiting_user_kickoff, :awaiting_user] ->
          start_question_continuation(state, message)

        _other ->
          {:noreply, %{state | pending_question_continuation: message}}
      end
    else
      {:noreply, state}
    end
  end

  def handle_info({:pika_backend_event, _event}, %{closed?: true} = state),
    do: {:noreply, state}

  def handle_info({:pika_backend_event, event}, state) do
    case event.type do
      :message_delta ->
        delta = event.data[:delta] || event.data["delta"] || ""

        state =
          state
          |> append_message_delta(event, to_string(delta))
          |> maybe_flush_stream_output()

        {:noreply, state}

      :message_completed ->
        state =
          state
          |> complete_message(event)
          |> maybe_flush_stream_output(true)

        {:noreply, state}

      type when type in [:tool_started, :tool_updated, :tool_completed, :file_changed] ->
        state = maybe_flush_stream_output(state, true)
        {:noreply, record_tool_activity(state, event)}

      :turn_completed ->
        handle_turn_completed(state, event)

      type when type in [:backend_error, :process_exited] ->
        reason = {type, event.data}
        state = backend_failed(state, reason)
        notify(state, {:agent_actor_interrupted, state.work, reason})
        {:stop, {:shutdown, reason}, state}

      _other ->
        {:noreply, state}
    end
  end

  def handle_info({:DOWN, monitor, :process, _pid, reason}, %{monitor: monitor} = state) do
    state = backend_failed(state, {:backend_down, reason})
    notify(state, {:agent_actor_interrupted, state.work, reason})
    {:stop, {:shutdown, reason}, state}
  end

  def handle_info({:EXIT, _pid, _reason}, state), do: {:noreply, state}
  def handle_info(_message, state), do: {:noreply, state}

  @impl true
  def terminate(_reason, %{closed?: true}), do: :ok

  def terminate(_reason, state) do
    try do
      _ = cleanup(state, "interrupted", "actor_terminated")
    rescue
      _error -> :ok
    catch
      :exit, _reason -> :ok
    end

    :ok
  end

  defp open(state) do
    work = state.work
    workspace = Keyword.fetch!(state.opts, :workspace)

    with {:ok, config} <- RuntimeConfig.load(workspace),
         {:ok, paths} <- Workspace.resolve(config, work),
         previous? <- previous_sessions?(work),
         :ok <- interrupt_orphan_sessions(work),
         :ok <- recover_interrupted_auxiliary(work, previous?),
         session_id <- ConversationJournal.allocate_session_id(),
         {:ok, bundle} <-
           ContextBundle.build(config, session_id, work.role_id, work.kind, work.id),
         {:ok, recoveries} <- recoveries(previous?, config, session_id, work, paths),
         {:ok, prompt} <- PromptBuilder.build(config, work, bundle, recoveries),
         {:ok, agent} <- BackendConfig.select(config, work),
         :ok <- ensure_role_started(work),
         {:ok, pika_session} <-
           ConversationJournal.start_session(
             work.role_id,
             work.kind,
             work.id,
             Pika.Optimization.Config.Agent.snapshot(agent),
             prompt.system,
             bundle.contents,
             id: session_id,
             recovery_sequence: recovery_sequence(recoveries)
           ),
         {:ok, state} <-
           open_backend(%{
             state
             | config: config,
               paths: paths,
               bundle: bundle,
               prompt: prompt,
               pika_session: pika_session,
               agent: agent
           }) do
      case activate_with_pending_followup(state, prompt.activation) do
        {:ok, state} ->
          notify(state, {:agent_actor_started, work, pika_session.id})
          {:ok, state}

        {:error, reason, failed_state} ->
          {:error, reason, cleanup(failed_state, "failed", inspect(reason))}

        {:error, reason} ->
          {:error, reason, cleanup(state, "failed", inspect(reason))}
      end
    else
      {:error, reason} -> {:error, reason, state}
    end
  end

  defp open_backend(state) do
    overrides = Keyword.get(state.opts, :backend_modules, %{})
    artifact_dir = Path.join([state.bundle.directory, "backend"])
    File.mkdir_p!(artifact_dir)

    with {:ok, module} <- BackendConfig.module(state.agent, overrides),
         launch_config <- BackendConfig.launch_config(state.agent, artifact_dir) do
      case Directory.issue(session_binding(state, token_actor: self()), state.directory) do
        {:ok, token} -> start_and_open_backend(state, module, launch_config, token)
        {:error, reason} -> fail_open_session(state, reason)
      end
    end
  end

  defp start_and_open_backend(state, module, launch_config, token) do
    case AgentBackend.start_link(module, launch_config, self()) do
      {:ok, handle} ->
        mcp = %{
          url: Keyword.get(state.opts, :mcp_url, default_mcp_url()),
          token: token,
          role: state.work.role_id,
          work_kind: state.work.kind,
          work_id: state.work.id,
          coordinator: self()
        }

        case AgentBackend.open_session(
               handle,
               state.paths.cwd,
               state.agent.model,
               state.agent.reasoning_effort,
               mcp,
               [],
               state.prompt.system
             ) do
          {:ok, provider_session} ->
            provider_id = provider_session.backend_session_id || provider_session.id

            {:ok, _session} =
              ConversationJournal.bind_provider(state.pika_session.id, provider_id)

            {:ok,
             %{
               state
               | handle: handle,
                 provider_session: provider_session,
                 token: token,
                 monitor: Process.monitor(handle.pid)
             }}

          {:error, reason} ->
            Directory.revoke(token, state.directory)
            AgentBackend.close_session(handle)
            fail_open_session(state, {:backend_session_open_failed, reason})
        end

      {:error, reason} ->
        Directory.revoke(token, state.directory)
        fail_open_session(state, {:backend_start_failed, reason})
    end
  end

  defp fail_open_session(state, reason) do
    _ = ConversationJournal.interrupt_session(state.pika_session.id, inspect(reason))
    {:error, reason}
  end

  defp activate(state, :await_user_kickoff),
    do: {:ok, %{state | phase: :awaiting_user_kickoff}}

  defp activate(state, {:start_turn, prompt}), do: start_turn(state, prompt)

  defp activate_with_pending_followup(
         %{work: %Work{role_id: role} = work} = state,
         activation
       )
       when role in ["baseline_verify", "iteration", "integration"] do
    case FollowupLifecycle.active_for_target(role, work.kind, work.id) do
      nil ->
        activate(state, activation)

      pending ->
        with {:ok, request} <- FollowupLifecycle.retarget(pending.id, state.pika_session.id) do
          case request.status do
            status when status in ["generating", "generator_running"] ->
              {:ok, %{state | phase: :awaiting_followup}}

            "generated" ->
              with {:ok, delivered} <- FollowupLifecycle.deliver(request.id),
                   {:ok, _running} <- FollowupLifecycle.target_turn_started(request.id),
                   {:ok, state} <- start_turn(state, delivered.message) do
                {:ok, %{state | active_followup_request_id: request.id}}
              end
          end
        end
    end
  end

  defp activate_with_pending_followup(state, activation), do: activate(state, activation)

  defp start_turn(state, prompt) do
    with {:ok, turn} <-
           ConversationJournal.start_turn(state.pika_session.id, [
             %{"role" => "user", "content" => prompt}
           ]),
         {:ok, provider_turn_id} <- AgentBackend.start_turn(state.handle, prompt) do
      new_state =
        Map.merge(state, %{
          phase: :running,
          turn_db_id: turn.id,
          active_provider_turn_id: provider_turn_id,
          output_messages: [],
          terminal_called: false,
          pending_question_continuation: nil
        })

      {:ok, Map.put(new_state, :output_flushed_at_ms, nil)}
    else
      {:error, reason} -> {:error, {:turn_start_failed, reason}, state}
    end
  end

  defp steer_turn(state, message) do
    with {:ok, _turn} <-
           ConversationJournal.append_input(state.turn_db_id, %{
             "role" => "user",
             "content" => message
           }),
         {:ok, provider_turn_id} <- AgentBackend.steer(state.handle, message) do
      {:ok, %{state | active_provider_turn_id: provider_turn_id}}
    else
      {:error, reason} -> {:error, {:turn_steer_failed, reason}, state}
    end
  end

  defp start_question_continuation(state, message) do
    state = %{state | pending_question_continuation: nil}

    case start_turn(state, message) do
      {:ok, state} ->
        {:noreply, state}

      {:error, reason, state} ->
        state = backend_failed(state, {:question_continuation_failed, reason})
        notify(state, {:agent_actor_interrupted, state.work, reason})
        {:stop, {:shutdown, reason}, state}
    end
  end

  defp question_continuation_message(batch_id, answers) do
    encoded_answers = Jason.encode!(answers, pretty: true)

    """
    用户已经在 Pika UI 中完成 `ask_questions` 批次 #{batch_id}。以下是整批最终答案：

    ```json
    #{encoded_answers}
    ```

    立即使用这些答案继续当前 Baseline Alignment 工作。不要再次声称仍在等待这批答案，也不要要求用户重复提交；继续准备、验证并通过 MCP 提交 Baseline Definition。
    """
    |> String.trim()
  end

  defp handle_turn_completed(
         %{active_provider_turn_id: active_turn_id} = state,
         %{turn_id: completed_turn_id}
       )
       when is_binary(active_turn_id) and is_binary(completed_turn_id) and
              active_turn_id != completed_turn_id do
    {:noreply, state}
  end

  defp handle_turn_completed(state, _event) do
    state = finish_journal_turn(state, "completed")
    state = %{state | active_provider_turn_id: nil}

    cond do
      state.terminal_called or WorkProjector.terminal?(state.work) ->
        send(self(), :finish_terminal)
        {:noreply, state}

      is_binary(state.pending_question_continuation) ->
        start_question_continuation(state, state.pending_question_continuation)

      state.work.role_id == "baseline_alignment" ->
        {:noreply, %{state | phase: :awaiting_user}}

      state.work.role_id in ["baseline_verify", "iteration", "integration"] ->
        continue_or_wait_followup(state)

      state.work.role_id in [
        "baseline_verify_followup",
        "iteration_followup",
        "integration_followup"
      ] ->
        fail_followup_generator(state)

      state.work.role_id == "progress_summary" ->
        fail_progress_summary(state)

      true ->
        {:noreply, %{state | phase: :awaiting_user}}
    end
  end

  defp continue_or_wait_followup(state) do
    operation = required_operation(state.work.role_id)
    recovery_state = Workspace.recovery_state(state.work, state.paths)

    request_result =
      if state.active_followup_request_id do
        FollowupLifecycle.target_turn_incomplete(
          state.active_followup_request_id,
          state.config,
          recovery_state
        )
      else
        FollowupLifecycle.request(
          state.config,
          state.pika_session.id,
          operation,
          recovery_state
        )
      end

    state = %{state | active_followup_request_id: nil}

    case request_result do
      {:ok, %{status: "generated"} = request} ->
        with {:ok, delivered} <- FollowupLifecycle.deliver(request.id),
             {:ok, _running} <- FollowupLifecycle.target_turn_started(request.id),
             {:ok, state} <- start_turn(state, delivered.message) do
          {:noreply, %{state | active_followup_request_id: request.id}}
        else
          {:error, reason} ->
            state = backend_failed(state, {:followup_delivery_failed, reason})
            {:stop, {:shutdown, reason}, state}
        end

      {:ok, %{status: "generating"}} ->
        {:noreply, %{state | phase: :awaiting_followup}}

      {:ok, %{status: "exhausted"}} ->
        {:stop, :normal, cleanup(state, "failed", "follow_up_exhausted")}

      {:error, reason} ->
        state = backend_failed(state, {:followup_request_failed, reason})
        {:stop, {:shutdown, reason}, state}
    end
  end

  defp fail_followup_generator(state) do
    {:ok, request_id} = integer_id(state.work.id)
    _ = FollowupLifecycle.generator_failed(request_id, :completed_without_submit_followup_message)
    {:stop, :normal, cleanup(state, "failed", "missing_submit_followup_message")}
  end

  defp fail_progress_summary(state) do
    {:ok, request_id} = integer_id(state.work.id)

    _ =
      ProgressSummaryLifecycle.fail_attempt(
        request_id,
        :completed_without_submit_progress_summary
      )

    {:stop, :normal, cleanup(state, "failed", "missing_submit_progress_summary")}
  end

  defp maybe_deliver_generated_followup(%{work: %Work{role_id: role, id: id}} = state)
       when role in ~w(baseline_verify_followup iteration_followup integration_followup) do
    with {:ok, request_id} <- integer_id(id),
         {:ok, request} <- FollowupLifecycle.fetch(request_id),
         {:ok, delivered} <- FollowupLifecycle.deliver(request_id),
         {:ok, actor} <-
           Directory.lookup_work(
             request.target_role,
             request.target_work_kind,
             request.target_work_id,
             state.directory
           ),
         :ok <- deliver_followup(actor, request_id, delivered.message) do
      state
    else
      {:error, reason} ->
        notify(state, {:followup_delivery_failed, state.work, reason})
        state
    end
  end

  defp maybe_deliver_generated_followup(state), do: state

  defp backend_failed(state, reason) do
    state = finish_journal_turn(state, "interrupted")

    case state.work.role_id do
      role when role in ~w(baseline_verify_followup iteration_followup integration_followup) ->
        with {:ok, id} <- integer_id(state.work.id),
             do: FollowupLifecycle.generator_failed(id, reason)

      "progress_summary" ->
        with {:ok, id} <- integer_id(state.work.id),
             do: ProgressSummaryLifecycle.fail_attempt(id, reason)

      _other ->
        ConversationJournal.interrupt_session(state.pika_session.id, inspect(reason))
    end

    cleanup_resources(%{state | closed?: true})
  end

  defp interrupt(state, reason) do
    state = finish_journal_turn(state, "interrupted")

    if state.handle do
      _ = AgentBackend.interrupt(state.handle)
    end

    if state.pika_session do
      _ = ConversationJournal.interrupt_session(state.pika_session.id, inspect(reason))
    end

    cleanup_resources(%{state | closed?: true})
  end

  defp cleanup(state, status, reason) do
    unless state.closed? do
      if state.pika_session do
        _ =
          safe_session_end(state.pika_session.id, status, reason)
      end
    end

    cleanup_resources(%{state | closed?: true, phase: String.to_atom(status)})
  end

  defp cleanup_resources(state) do
    if state.token do
      try do
        Directory.revoke(state.token, state.directory)
      catch
        :exit, _reason -> :ok
      end
    end

    if state.handle do
      try do
        AgentBackend.close_session(state.handle)
      catch
        _, _ -> :ok
      end
    end

    state
  end

  defp safe_session_end(session_id, status, reason) do
    if status in ["completed", "failed"] do
      ConversationJournal.complete_session(session_id, status, reason)
    else
      ConversationJournal.interrupt_session(session_id, reason)
    end
  rescue
    _error -> :ok
  catch
    :exit, _reason -> :ok
  end

  defp finish_journal_turn(%{turn_db_id: nil} = state, _reason), do: state

  defp finish_journal_turn(state, reason) do
    state = maybe_flush_stream_output(state, true)

    _ = ConversationJournal.finish_turn(state.turn_db_id, reason)

    state
    |> Map.merge(%{turn_db_id: nil, output_messages: []})
    |> Map.put(:output_flushed_at_ms, nil)
  end

  defp maybe_flush_stream_output(state, force? \\ false)

  defp maybe_flush_stream_output(%{turn_db_id: nil} = state, _force?), do: state

  defp maybe_flush_stream_output(state, force?) do
    messages = state_output_messages(state)
    now_ms = System.monotonic_time(:millisecond)

    output_flushed_at_ms = Map.get(state, :output_flushed_at_ms)

    cond do
      messages == [] ->
        state

      force? or is_nil(output_flushed_at_ms) or
          now_ms - output_flushed_at_ms >= @stream_flush_ms ->
        _ = ConversationJournal.stream_outputs(state.turn_db_id, messages)
        Map.put(state, :output_flushed_at_ms, now_ms)

      true ->
        state
    end
  end

  defp append_message_delta(state, _event, ""), do: state

  defp append_message_delta(state, event, delta) do
    item_id = message_item_id(event)

    update_output_message(state, item_id, fn message ->
      message
      |> Map.put("role", "assistant")
      |> Map.put("content", (message["content"] || "") <> delta)
      |> Map.put("complete", false)
      |> put_message_metadata("phase", event.data[:phase] || event.data["phase"])
      |> put_message_metadata("delivery", event.data[:delivery] || event.data["delivery"])
    end)
  end

  defp complete_message(state, event) do
    item = event.data[:item] || event.data["item"] || %{}
    text = item["text"] || item["content"]

    if is_binary(text) do
      update_output_message(state, item["id"] || message_item_id(event), fn message ->
        message
        |> Map.put("role", "assistant")
        |> Map.put("content", text)
        |> Map.put("complete", true)
        |> put_message_metadata("phase", item["phase"])
        |> put_message_metadata("delivery", item["delivery"])
      end)
    else
      state
    end
  end

  defp update_output_message(state, item_id, update) do
    {messages, found?} =
      Enum.map_reduce(state_output_messages(state), false, fn message, found? ->
        if message["id"] == item_id do
          {update.(message), true}
        else
          {message, found?}
        end
      end)

    messages = if found?, do: messages, else: messages ++ [update.(%{"id" => item_id})]
    Map.put(state, :output_messages, messages)
  end

  defp state_output_messages(state) do
    case Map.fetch(state, :output_messages) do
      {:ok, messages} ->
        messages

      :error ->
        case Map.get(state, :output, "") do
          "" ->
            []

          legacy_output ->
            [
              %{
                "id" => "turn:legacy",
                "role" => "assistant",
                "content" => legacy_output,
                "complete" => false
              }
            ]
        end
    end
  end

  defp message_item_id(event) do
    event.data[:item_id] || event.data["item_id"] || event.data["itemId"] ||
      "turn:#{event.turn_id || "unknown"}"
  end

  defp put_message_metadata(message, _key, nil), do: message
  defp put_message_metadata(message, key, value), do: Map.put(message, key, value)

  defp record_mcp(%{turn_db_id: nil} = state, _operation, _result), do: state

  defp record_mcp(state, operation, result) do
    record =
      case result do
        {:ok, _value} ->
          %{"name" => operation, "status" => "ok"}

        {:error, reason} ->
          %{"name" => operation, "status" => "error", "summary" => inspect(reason)}
      end

    _ = ConversationJournal.append_mcp_call(state.turn_db_id, record)
    state
  end

  defp record_tool_activity(%{turn_db_id: nil} = state, _event), do: state

  defp record_tool_activity(state, event) do
    case ToolActivity.from_event(event) do
      {:ok, activity} ->
        _ = ConversationJournal.upsert_tool_call(state.turn_db_id, activity)
        state

      :ignore ->
        state
    end
  end

  defp session_binding(state, opts \\ []) do
    %SessionBinding{
      actor: Keyword.get(opts, :token_actor, self()),
      role_id: state.work.role_id,
      work_kind: state.work.kind,
      work_id: state.work.id,
      session_id: state.pika_session.id,
      context_file: state.bundle.context_file,
      work_root: state.paths.work_root
    }
  end

  defp ensure_role_started(%Work{role_id: role, id: id})
       when role in ~w(baseline_verify_followup iteration_followup integration_followup) do
    with {:ok, request_id} <- integer_id(id),
         {:ok, request} <- FollowupLifecycle.fetch(request_id) do
      case request.status do
        "generating" -> FollowupLifecycle.start_generator(request_id) |> ok_only()
        "generator_running" -> :ok
        status -> {:error, {:followup_request_not_runnable, status}}
      end
    end
  end

  defp ensure_role_started(%Work{role_id: "progress_summary", id: id}) do
    with {:ok, request_id} <- integer_id(id),
         {:ok, request} <- ProgressSummaryLifecycle.fetch(request_id) do
      case request.status do
        "requested" -> ProgressSummaryLifecycle.start(request_id) |> ok_only()
        "running" -> :ok
        status -> {:error, {:progress_summary_not_runnable, status}}
      end
    end
  end

  defp ensure_role_started(_work), do: :ok

  defp previous_sessions?(work) do
    Pika.Repo.query!(
      "SELECT 1 FROM agent_sessions WHERE optimization_id = 'optimization' AND role = ? AND work_kind = ? AND work_id = ? LIMIT 1",
      [work.role_id, to_string(work.kind), work.id]
    ).rows != []
  end

  defp interrupt_orphan_sessions(work) do
    ConversationJournal.interrupt_open_work_sessions(
      work.role_id,
      work.kind,
      work.id,
      "new_actor_recovery"
    )
  end

  defp recover_interrupted_auxiliary(
         %Work{role_id: role, id: id},
         true
       )
       when role in ~w(baseline_verify_followup iteration_followup integration_followup) do
    with {:ok, request_id} <- integer_id(id),
         {:ok, request} <- FollowupLifecycle.fetch(request_id) do
      if request.status == "generator_running" do
        FollowupLifecycle.generator_failed(request_id, :orphaned_generator_session) |> ok_only()
      else
        :ok
      end
    end
  end

  defp recover_interrupted_auxiliary(%Work{role_id: "progress_summary", id: id}, true) do
    with {:ok, request_id} <- integer_id(id),
         {:ok, request} <- ProgressSummaryLifecycle.fetch(request_id) do
      if request.status == "running" do
        ProgressSummaryLifecycle.fail_attempt(request_id, :orphaned_summary_session) |> ok_only()
      else
        :ok
      end
    end
  end

  defp recover_interrupted_auxiliary(_work, _previous), do: :ok

  defp recoveries(false, _config, _session_id, _work, _paths), do: {:ok, []}

  defp recoveries(true, config, session_id, work, paths) do
    case RecoveryContext.build_for_work(
           config.workspace,
           session_id,
           work.role_id,
           work.kind,
           work.id,
           Workspace.recovery_state(work, paths)
         ) do
      {:ok, recovery} -> {:ok, [recovery]}
      {:error, reason} -> {:error, reason}
    end
  end

  defp terminal_operation?(role_id, operation) do
    {:ok, definition} = RoleRegistry.fetch(role_id)
    operation in definition.terminal_commands
  end

  defp required_operation("baseline_verify"), do: "finish_baseline_verification"
  defp required_operation("iteration"), do: "finish_iteration"
  defp required_operation("integration"), do: "finish_integration"

  defp integer_id(value) do
    case Integer.parse(value) do
      {id, ""} when id > 0 -> {:ok, id}
      _other -> {:error, {:invalid_work_id, value}}
    end
  end

  defp recovery_sequence([]), do: 0
  defp recovery_sequence(recoveries), do: List.last(recoveries).sequence
  defp ok_only({:ok, _value}), do: :ok
  defp ok_only({:error, _reason} = error), do: error

  defp stringify_keys(map) do
    Map.new(map, fn {key, value} -> {to_string(key), value} end)
  end

  defp default_mcp_url do
    endpoint = Application.get_env(:pika, PikaWeb.Endpoint, [])
    http = endpoint[:http] || []
    port = http[:port] || 4000
    "http://127.0.0.1:#{port}/mcp"
  end

  defp notify(state, message) do
    if pid = Keyword.get(state.opts, :notify), do: send(pid, message)
    :ok
  end
end
