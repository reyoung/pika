defmodule Pika.AgentBackend.CursorACP do
  @moduledoc "Cursor Agent ACP v1 stdio adapter."

  use GenServer
  @behaviour Pika.AgentBackend

  alias Pika.AgentBackend.{Error, Event, Id, JSONLPort, LaunchConfig, PermissionPolicy, Session}

  @protocol "acp-v1"
  @default_command "cursor-agent"
  @rpc_timeout 60_000

  @impl true
  def start_link(profile, event_sink), do: GenServer.start_link(__MODULE__, {profile, event_sink})

  @impl true
  def open_session(server, cwd, model, reasoning_effort, mcp, skill_roots, instructions) do
    cwd = Path.expand(cwd)
    skill_roots = Enum.map(skill_roots, &Path.expand/1)

    result =
      with :ok <-
             GenServer.call(
               server,
               {:install_system_instructions, cwd, instructions},
               @rpc_timeout
             ),
           :ok <- GenServer.call(server, {:start_transport, mcp}, @rpc_timeout),
           {:ok, initialize_response} <- rpc(server, "initialize", initialize_params()),
           :ok <- GenServer.call(server, {:provider_capabilities, initialize_response}),
           {:ok, backend_session_id, resumed, resume_error} <-
             open_cursor_session(
               server,
               initialize_response,
               Map.get(mcp, :resume_session_id),
               cwd,
               mcp,
               skill_roots
             ),
           :ok <- maybe_select_model(server, backend_session_id, model),
           {:ok, session} <-
             GenServer.call(
               server,
               {:establish_session, backend_session_id, cwd, model, reasoning_effort, skill_roots,
                resumed, resume_error}
             ) do
        {:ok, session}
      end

    case result do
      {:ok, _session} = ok ->
        ok

      error ->
        GenServer.call(server, :remove_system_instructions, @rpc_timeout)
        error
    end
  end

  @impl true
  def start_turn(server, input), do: GenServer.call(server, {:start_prompt, input}, @rpc_timeout)

  @impl true
  def steer(server, input), do: GenServer.call(server, {:steer, input}, @rpc_timeout)

  @impl true
  def interrupt(server), do: GenServer.call(server, :interrupt, @rpc_timeout)

  @impl true
  def close_session(server) do
    case GenServer.call(server, :close_context) do
      {:ok, %{wire_close: true, backend_session_id: session_id}} ->
        case rpc(server, "session/close", %{"sessionId" => session_id}) do
          {:ok, _} ->
            GenServer.call(server, :close_transport)

          {:error, %Error{code: :provider_error, details: %{raw: %{"code" => -32_601}}}} ->
            GenServer.call(server, {:disable_wire_close_and_close, :method_not_found})

          {:error, error} ->
            {:error, error}
        end

      {:ok, _context} ->
        GenServer.call(server, :close_transport)

      {:error, error} ->
        {:error, error}
    end
  end

  @impl true
  def capabilities(server), do: GenServer.call(server, :capabilities)

  def process_os_pid(server), do: GenServer.call(server, :process_os_pid)

  @impl true
  def init({profile, event_sink}) do
    profile = LaunchConfig.normalize(profile)

    {:ok,
     %{
       profile: profile,
       event_sink: event_sink,
       session_id: Ecto.UUID.generate(),
       transport: nil,
       pending: %{},
       next_id: 1,
       backend_session_id: nil,
       active_turn_id: nil,
       text_segment_sequence: 0,
       text_segment_kind: nil,
       steer_waiter: nil,
       cwd: nil,
       model: nil,
       reasoning_effort: nil,
       skill_roots: [],
       provider_capabilities: %{},
       wire_close: false,
       jsonl_path: nil,
       instruction_rule: nil,
       loading_session: false,
       closed: false
     }}
  end

  @impl true
  def handle_call({:install_system_instructions, cwd, instructions}, _from, state) do
    case install_instruction_rule(cwd, state.session_id, instructions) do
      {:ok, rule} ->
        {:reply, :ok, %{state | instruction_rule: rule}}

      {:error, reason} ->
        {:reply,
         rpc_error(
           :system_instructions_failed,
           "could not install Cursor system instructions",
           reason
         ), state}
    end
  end

  def handle_call(:remove_system_instructions, _from, state) do
    {:reply, :ok, cleanup_instruction_rule(state)}
  end

  @impl true
  def handle_call({:start_transport, _mcp}, _from, %{transport: nil} = state) do
    profile = state.profile
    artifact_dir = Path.expand(profile.artifact_dir)
    jsonl_path = Path.join(artifact_dir, "cursor-#{state.session_id}.jsonl")
    stderr_path = Path.join(artifact_dir, "cursor-#{state.session_id}.stderr.log")
    command = profile.command || @default_command

    args =
      profile.args ++
        PermissionPolicy.cursor_cli_args(profile.approval_policy, profile.sandbox_policy) ++
        ["--approve-mcps", "--trust", "acp"]

    case JSONLPort.start_link(
           owner: self(),
           command: command,
           args: args,
           env: profile.env,
           stderr_path: stderr_path,
           jsonl_path: jsonl_path
         ) do
      {:ok, transport} ->
        {:reply, :ok, %{state | transport: transport, jsonl_path: jsonl_path}}

      {:error, reason} ->
        {:reply, rpc_error(:process_start_failed, "could not start Cursor ACP", reason), state}
    end
  end

  def handle_call({:start_transport, _mcp}, _from, state), do: {:reply, :ok, state}

  def handle_call({:rpc, method, params}, from, state) do
    {id, state} = next_request(state, method, params, {:call, from})

    if id do
      {:noreply, state}
    else
      {:reply, rpc_error(:transport_closed, "Cursor ACP transport is closed", nil), state}
    end
  end

  def handle_call({:provider_capabilities, response}, _from, state) do
    capabilities = response["agentCapabilities"] || %{}
    wire_close = is_map(get_in(capabilities, ["sessionCapabilities", "close"]))
    {:reply, :ok, %{state | provider_capabilities: capabilities, wire_close: wire_close}}
  end

  def handle_call(
        {:establish_session, backend_id, cwd, model, effort, skill_roots, resumed, resume_error},
        _from,
        state
      ) do
    session = %Session{
      id: state.session_id,
      backend: :cursor_acp,
      backend_protocol: @protocol,
      backend_session_id: backend_id,
      cwd: cwd,
      model: model,
      reasoning_effort: effort,
      jsonl_path: state.jsonl_path,
      resumed: resumed,
      resume_error: resume_error
    }

    state = %{
      state
      | backend_session_id: backend_id,
        cwd: cwd,
        model: model,
        reasoning_effort: effort,
        skill_roots: skill_roots
    }

    emit(state, :session_started, data: %{protocol: @protocol, wire_close: state.wire_close})
    {:reply, {:ok, session}, state}
  end

  def handle_call({:start_prompt, input}, _from, state) do
    if state.backend_session_id && is_nil(state.active_turn_id) do
      {turn_id, state} = send_prompt(state, input)
      {:reply, {:ok, turn_id}, state}
    else
      {:reply,
       rpc_error(:turn_already_active, "Cursor ACP already has an active prompt turn", nil),
       state}
    end
  end

  def handle_call({:steer, _input}, _from, %{active_turn_id: nil} = state) do
    {:reply, rpc_error(:steer_failed, "there is no active Cursor turn to steer", nil), state}
  end

  def handle_call({:steer, _input}, _from, %{steer_waiter: waiter} = state)
      when not is_nil(waiter) do
    {:reply, rpc_error(:steer_failed, "a Cursor steer is already pending", nil), state}
  end

  def handle_call({:steer, input}, from, state) do
    :ok = send_notification(state, "session/cancel", %{"sessionId" => state.backend_session_id})

    {:noreply,
     %{state | steer_waiter: %{from: from, input: input, cancelled_turn_id: state.active_turn_id}}}
  end

  def handle_call(:interrupt, _from, %{active_turn_id: nil} = state) do
    {:reply, rpc_error(:no_active_turn, "there is no active Cursor turn", nil), state}
  end

  def handle_call(:interrupt, _from, state) do
    result =
      send_notification(state, "session/cancel", %{"sessionId" => state.backend_session_id})

    {:reply, result, state}
  end

  def handle_call(:capabilities, _from, state) do
    {:reply,
     %{
       protocol: @protocol,
       native_steer: false,
       simulated_steer: :cancel_then_prompt,
       interrupt: true,
       close: if(state.wire_close, do: :protocol, else: :process_fallback),
       http_mcp: get_in(state.provider_capabilities, ["mcpCapabilities", "http"]) == true,
       system_instructions: :project_rule,
       skill_roots: :additional_directories,
       provider_resume:
         if(state.provider_capabilities["loadSession"] == true,
           do: :session_load,
           else: false
         ),
       provider_resume_required: false
     }, state}
  end

  def handle_call(:close_context, _from, %{backend_session_id: nil} = state) do
    {:reply, rpc_error(:session_not_open, "Cursor session has not been opened", nil), state}
  end

  def handle_call(:close_context, _from, state) do
    {:reply, {:ok, Map.take(state, [:wire_close, :backend_session_id])}, state}
  end

  def handle_call(:close_transport, _from, state), do: close_transport(state, nil)

  def handle_call({:disable_wire_close_and_close, reason}, _from, state) do
    close_transport(%{state | wire_close: false}, reason)
  end

  def handle_call(:process_os_pid, _from, %{transport: nil} = state), do: {:reply, nil, state}

  def handle_call(:process_os_pid, _from, state),
    do: {:reply, JSONLPort.os_pid(state.transport), state}

  def handle_call({:loading_session, loading?}, _from, state),
    do: {:reply, :ok, %{state | loading_session: loading?}}

  @impl true
  def handle_info(
        {:backend_wire, transport, %{"id" => id, "method" => method} = request},
        %{transport: transport} = state
      ) do
    handle_client_request(id, method, request["params"] || %{}, state)
  end

  def handle_info(
        {:backend_wire, transport, %{"id" => id} = message},
        %{transport: transport} = state
      ) do
    handle_response(id, message, state)
  end

  def handle_info(
        {:backend_wire, transport, %{"method" => "session/update", "params" => params}},
        %{transport: transport} = state
      ) do
    if state.loading_session do
      {:noreply, state}
    else
      {:noreply, map_session_update(params["update"] || %{}, state)}
    end
  end

  def handle_info({:backend_wire, transport, _message}, %{transport: transport} = state),
    do: {:noreply, state}

  def handle_info({:backend_wire_error, transport, line, error}, %{transport: transport} = state) do
    emit(state, :backend_error,
      data: %{code: :invalid_json, line: line, error: Exception.message(error)}
    )

    {:noreply, state}
  end

  def handle_info(
        {:backend_process_exited, transport, status, closed_by_client},
        %{transport: transport} = state
      ) do
    Enum.each(state.pending, fn {_id, pending} -> reply_pending_error(pending, status) end)

    emit(state, :process_exited,
      data: %{status: status, expected: closed_by_client || state.closed}
    )

    {:stop, :normal, %{state | transport: nil, pending: %{}, active_turn_id: nil}}
  end

  def handle_info(_message, state), do: {:noreply, state}

  @impl true
  def terminate(_reason, %{transport: transport} = state) when is_pid(transport) do
    if Process.alive?(transport), do: JSONLPort.close(transport)
    cleanup_instruction_rule(state)
    :ok
  end

  def terminate(_reason, state) do
    cleanup_instruction_rule(state)
    :ok
  end

  defp rpc(server, method, params),
    do: GenServer.call(server, {:rpc, method, params}, @rpc_timeout)

  defp next_request(%{transport: nil} = state, _method, _params, _pending), do: {nil, state}

  defp next_request(state, method, params, pending_value) do
    id = state.next_id

    :ok =
      JSONLPort.send_message(state.transport, %{
        "jsonrpc" => "2.0",
        "id" => id,
        "method" => method,
        "params" => params
      })

    {id, %{state | next_id: id + 1, pending: Map.put(state.pending, id, pending_value)}}
  end

  defp send_notification(state, method, params) do
    JSONLPort.send_message(state.transport, %{
      "jsonrpc" => "2.0",
      "method" => method,
      "params" => params
    })
  end

  defp send_prompt(state, input) do
    turn_id = Id.new("turn")
    prompt = normalize_prompt(input)

    {_id, state} =
      next_request(
        state,
        "session/prompt",
        %{"sessionId" => state.backend_session_id, "prompt" => prompt},
        {:prompt, turn_id}
      )

    state =
      Map.merge(state, %{
        active_turn_id: turn_id,
        text_segment_sequence: 0,
        text_segment_kind: nil
      })

    emit(state, :turn_started, turn_id: turn_id)
    {turn_id, state}
  end

  defp handle_response(id, message, state) do
    case Map.pop(state.pending, id) do
      {nil, _pending} ->
        {:noreply, state}

      {{:call, from}, pending} ->
        reply = response_result(message)
        GenServer.reply(from, reply)
        {:noreply, %{state | pending: pending}}

      {{:prompt, turn_id}, pending} ->
        result = response_result(message)

        data =
          case result do
            {:ok, value} -> value
            {:error, %Error{} = error} -> %{stopReason: "error", error: Map.from_struct(error)}
          end

        emit(state, :turn_completed, turn_id: turn_id, data: data)
        state = %{state | pending: pending, active_turn_id: nil}
        continue_after_cancel(state, data)
    end
  end

  defp continue_after_cancel(%{steer_waiter: nil} = state, _data), do: {:noreply, state}

  defp continue_after_cancel(state, data) do
    if stop_reason(data) == "cancelled" do
      %{from: from, input: input} = state.steer_waiter
      {turn_id, state} = send_prompt(%{state | steer_waiter: nil}, input)
      GenServer.reply(from, {:ok, turn_id})
      {:noreply, state}
    else
      GenServer.reply(
        state.steer_waiter.from,
        rpc_error(:steer_failed, "Cursor cancel did not complete with stopReason=cancelled", data)
      )

      {:noreply, %{state | steer_waiter: nil}}
    end
  end

  defp handle_client_request(id, "session/request_permission", params, state) do
    options = params["options"] || []

    selected = PermissionPolicy.cursor_permission_option(options, state.profile.approval_policy)

    result =
      if selected do
        %{"outcome" => %{"outcome" => "selected", "optionId" => selected["optionId"]}}
      else
        %{"outcome" => %{"outcome" => "cancelled"}}
      end

    :ok =
      JSONLPort.send_message(state.transport, %{
        "jsonrpc" => "2.0",
        "id" => id,
        "result" => result
      })

    {:noreply, state}
  end

  defp handle_client_request(id, method, _params, state) do
    :ok =
      JSONLPort.send_message(state.transport, %{
        "jsonrpc" => "2.0",
        "id" => id,
        "error" => %{"code" => -32_601, "message" => "unsupported client method: #{method}"}
      })

    {:noreply, state}
  end

  defp map_session_update(%{"sessionUpdate" => "agent_message_chunk"} = update, state) do
    emit_text_chunk(update, state, "commentary")
  end

  defp map_session_update(%{"sessionUpdate" => "agent_thought_chunk"} = update, state) do
    emit_text_chunk(update, state, "reasoning")
  end

  defp map_session_update(%{"sessionUpdate" => "plan"} = update, state) do
    emit(state, :plan_updated, data: update)
    state
  end

  defp map_session_update(%{"sessionUpdate" => "tool_call"} = update, state) do
    emit(state, :tool_started, data: update)
    Map.put(state, :text_segment_kind, nil)
  end

  defp map_session_update(%{"sessionUpdate" => "tool_call_update"} = update, state) do
    status = update["status"]
    event = if status in ["completed", "failed"], do: :tool_completed, else: :tool_updated
    emit(state, event, data: update)

    Enum.each(update["content"] || [], fn content ->
      case content do
        %{"type" => "terminal", "terminalId" => _} ->
          emit(state, :command_output,
            data:
              content
              |> Map.put(
                "command_id",
                update["toolCallId"] || update["id"] || content["terminalId"]
              )
              |> Map.put("update_mode", "replace")
          )

        %{"type" => "diff"} ->
          emit(state, :file_changed, data: content)

        _ ->
          :ok
      end
    end)

    case cursor_raw_output(update["rawOutput"]) do
      output when is_binary(output) ->
        emit(state, :command_output,
          data: %{
            "command_id" => update["toolCallId"] || update["id"],
            "update_mode" => "replace",
            "output" => output,
            "status" => status || "running"
          }
        )

      nil ->
        :ok
    end

    state
  end

  defp map_session_update(%{"sessionUpdate" => "usage_update"} = update, state) do
    emit(state, :usage_updated, data: update)
    state
  end

  defp map_session_update(_update, state), do: state

  defp emit_text_chunk(update, state, phase) do
    content = update["content"] || %{}
    delta = content["text"] || update["textDelta"] || ""
    {item_id, state} = text_segment(state, phase)

    emit(state, :message_delta,
      data: %{delta: delta, item_id: item_id, phase: phase, delivery: "stream"}
    )

    state
  end

  defp text_segment(state, phase) do
    sequence = Map.get(state, :text_segment_sequence, 0)

    if Map.get(state, :text_segment_kind) == phase and sequence > 0 do
      {"cursor-#{phase}-#{sequence}", state}
    else
      sequence = sequence + 1

      {"cursor-#{phase}-#{sequence}",
       state
       |> Map.put(:text_segment_sequence, sequence)
       |> Map.put(:text_segment_kind, phase)}
    end
  end

  defp cursor_raw_output(output) when is_binary(output), do: output

  defp cursor_raw_output(output) when is_map(output) do
    output["content"] || output["output"] || output["stdout"] || output[:content] ||
      output[:output] || output[:stdout]
  end

  defp cursor_raw_output(_output), do: nil

  defp close_transport(%{transport: nil} = state, _fallback_reason),
    do: {:reply, :ok, cleanup_instruction_rule(state)}

  defp close_transport(state, fallback_reason) do
    :ok = JSONLPort.close(state.transport)

    emit(state, :process_exited,
      data: %{status: :closed, expected: true, wire_close_fallback: fallback_reason}
    )

    {:reply, :ok,
     state
     |> Map.merge(%{transport: nil, closed: true})
     |> cleanup_instruction_rule()}
  end

  defp install_instruction_rule(cwd, session_id, instructions)
       when is_binary(instructions) and instructions != "" do
    relative_path = ".cursor/rules/pika-system-#{session_id}.mdc"
    path = Path.join(cwd, relative_path)

    with :ok <- File.mkdir_p(Path.dirname(path)),
         :ok <- ensure_missing(path),
         :ok <- File.write(path, cursor_rule(instructions)),
         {:ok, exclude} <- install_git_exclude(cwd, relative_path, session_id) do
      {:ok, %{path: path, exclude: exclude}}
    else
      {:error, reason} ->
        File.rm(path)
        {:error, reason}
    end
  end

  defp install_instruction_rule(_cwd, _session_id, instructions),
    do: {:error, {:invalid_system_instructions, instructions}}

  defp install_git_exclude(cwd, relative_path, session_id) do
    case System.cmd("git", ["-C", cwd, "rev-parse", "--git-path", "info/exclude"],
           stderr_to_stdout: true
         ) do
      {raw_path, 0} ->
        exclude_path = raw_path |> String.trim() |> expand_from(cwd)
        marker = "# pika-system-#{session_id}\n/#{relative_path}\n"

        with :ok <- File.mkdir_p(Path.dirname(exclude_path)),
             {:ok, existing} <- read_or_empty(exclude_path),
             :ok <- File.write(exclude_path, append_block(existing, marker)) do
          {:ok, %{path: exclude_path, marker: marker}}
        end

      {_output, _status} ->
        {:ok, nil}
    end
  end

  defp cleanup_instruction_rule(%{instruction_rule: nil} = state), do: state

  defp cleanup_instruction_rule(%{instruction_rule: rule} = state) do
    File.rm(rule.path)

    case rule.exclude do
      %{path: exclude_path, marker: marker} ->
        with {:ok, contents} <- File.read(exclude_path) do
          File.write(exclude_path, String.replace(contents, marker, "", global: false))
        end

      nil ->
        :ok
    end

    %{state | instruction_rule: nil}
  end

  defp cursor_rule(instructions) do
    "---\ndescription: Pika Backend Session system instructions\nalwaysApply: true\n---\n\n" <>
      instructions <> "\n"
  end

  defp ensure_missing(path) do
    if File.exists?(path), do: {:error, :instruction_rule_exists}, else: :ok
  end

  defp read_or_empty(path) do
    case File.read(path) do
      {:ok, contents} -> {:ok, contents}
      {:error, :enoent} -> {:ok, ""}
      {:error, reason} -> {:error, reason}
    end
  end

  defp append_block("", block), do: block

  defp append_block(existing, block) when is_binary(existing) do
    separator = if String.ends_with?(existing, "\n"), do: "", else: "\n"
    existing <> separator <> block
  end

  defp expand_from(path, cwd) do
    if Path.type(path) == :absolute, do: path, else: Path.expand(path, cwd)
  end

  defp initialize_params do
    %{
      "protocolVersion" => 1,
      "clientCapabilities" => %{"fs" => %{"readTextFile" => false, "writeTextFile" => false}},
      "clientInfo" => %{"name" => "pika", "version" => "0.0.1"}
    }
  end

  defp session_new_params(cwd, mcp, skill_roots) do
    %{
      "cwd" => cwd,
      "additionalDirectories" => skill_roots,
      "mcpServers" => [
        %{
          "type" => "http",
          "name" => "pika",
          "url" => Map.fetch!(mcp, :url),
          "headers" => [
            %{"name" => "Authorization", "value" => "Bearer #{Map.fetch!(mcp, :token)}"}
          ]
        }
      ]
    }
  end

  defp open_cursor_session(server, initialize_response, resume_session_id, cwd, mcp, skill_roots)
       when is_binary(resume_session_id) and resume_session_id != "" do
    if get_in(initialize_response, ["agentCapabilities", "loadSession"]) == true do
      :ok = GenServer.call(server, {:loading_session, true})

      resume_result =
        rpc(
          server,
          "session/load",
          session_load_params(resume_session_id, cwd, mcp, skill_roots)
        )

      :ok = GenServer.call(server, {:loading_session, false})

      case resume_result do
        {:ok, _response} ->
          {:ok, resume_session_id, true, nil}

        {:error, resume_error} ->
          open_new_cursor_session(server, cwd, mcp, skill_roots, resume_error)
      end
    else
      open_new_cursor_session(server, cwd, mcp, skill_roots, :session_load_unsupported)
    end
  end

  defp open_cursor_session(
         server,
         _initialize_response,
         _resume_session_id,
         cwd,
         mcp,
         skill_roots
       ),
       do: open_new_cursor_session(server, cwd, mcp, skill_roots, nil)

  defp open_new_cursor_session(server, cwd, mcp, skill_roots, resume_error) do
    with {:ok, response} <- rpc(server, "session/new", session_new_params(cwd, mcp, skill_roots)),
         {:ok, backend_session_id} <- fetch_id(response, "sessionId") do
      {:ok, backend_session_id, false, resume_error}
    end
  end

  defp session_load_params(session_id, cwd, mcp, skill_roots) do
    session_new_params(cwd, mcp, skill_roots)
    |> Map.put("sessionId", session_id)
  end

  defp maybe_select_model(_server, _session_id, nil), do: :ok

  defp maybe_select_model(server, session_id, model) do
    with {:ok, _} <-
           rpc(server, "session/set_config_option", %{
             "sessionId" => session_id,
             "configId" => "model",
             "value" => model
           }) do
      :ok
    end
  end

  defp normalize_prompt(text) when is_binary(text), do: [%{"type" => "text", "text" => text}]
  defp normalize_prompt(items) when is_list(items), do: items

  defp response_result(%{"result" => result}), do: {:ok, result}

  defp response_result(%{"error" => error}),
    do: rpc_error(:provider_error, error["message"] || "Cursor ACP RPC error", error)

  defp fetch_id(map, key) do
    case map[key] do
      id when is_binary(id) -> {:ok, id}
      _ -> rpc_error(:invalid_response, "provider response did not contain #{key}", map)
    end
  end

  defp stop_reason(map), do: map["stopReason"] || map[:stopReason]

  defp reply_pending_error({:call, from}, status),
    do: GenServer.reply(from, rpc_error(:process_exited, "Cursor ACP exited", status))

  defp reply_pending_error({:prompt, _turn_id}, _status), do: :ok

  defp emit(state, type, attrs) do
    event =
      Event.new(type, :cursor_acp, state.session_id, %{
        backend_session_id: state.backend_session_id,
        turn_id: Keyword.get(attrs, :turn_id, state.active_turn_id),
        data: Keyword.get(attrs, :data, %{})
      })

    _ = Pika.CommandConsole.capture(event, state.profile.artifact_dir, state.cwd)

    case state.event_sink do
      sink when is_pid(sink) -> send(sink, {:pika_backend_event, event})
      sink when is_function(sink, 1) -> sink.(event)
    end
  end

  defp rpc_error(code, message, details),
    do: {:error, %Error{code: code, message: message, details: %{raw: details}}}
end
