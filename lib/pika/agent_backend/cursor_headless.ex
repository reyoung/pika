defmodule Pika.AgentBackend.CursorHeadless do
  @moduledoc "Cursor Agent headless/print-mode adapter with one subprocess per turn."

  use GenServer
  @behaviour Pika.AgentBackend

  alias Pika.AgentBackend.{Error, Event, Id, JSONLPort, JSONLWriter, LaunchConfig}
  alias Pika.AgentBackend.{PermissionPolicy, Session}

  @protocol "cursor-headless-stream-json-v1"
  @default_command "cursor-agent"
  @control_timeout 15_000
  @call_timeout 30_000

  @impl true
  def start_link(profile, event_sink), do: GenServer.start_link(__MODULE__, {profile, event_sink})

  @impl true
  def open_session(server, cwd, model, reasoning_effort, mcp, skill_roots, instructions) do
    GenServer.call(
      server,
      {:open_session, Path.expand(cwd), model, reasoning_effort, mcp,
       Enum.map(skill_roots, &Path.expand/1), instructions},
      @call_timeout
    )
  end

  @impl true
  def start_turn(server, input), do: GenServer.call(server, {:start_turn, input}, @call_timeout)

  @impl true
  def steer(server, input), do: GenServer.call(server, {:steer, input}, @call_timeout)

  @impl true
  def interrupt(server), do: GenServer.call(server, :interrupt, @call_timeout)

  @impl true
  def close_session(server), do: GenServer.call(server, :close_session, @call_timeout)

  @impl true
  def capabilities(server), do: GenServer.call(server, :capabilities)

  def process_os_pid(server), do: GenServer.call(server, :process_os_pid)

  @impl true
  def init({profile, event_sink}) do
    profile = LaunchConfig.normalize(profile)
    session_id = Ecto.UUID.generate()
    artifact_dir = Path.expand(profile.artifact_dir)

    {:ok,
     %{
       profile: profile,
       event_sink: event_sink,
       session_id: session_id,
       backend_session_id: nil,
       cwd: nil,
       model: nil,
       reasoning_effort: nil,
       plugin_dir: nil,
       jsonl_path: Path.join(artifact_dir, "cursor-headless-#{session_id}.jsonl"),
       stderr_path: Path.join(artifact_dir, "cursor-headless-#{session_id}.stderr.log"),
       transport: nil,
       active_turn_id: nil,
       message_item_id: nil,
       accumulated_text: "",
       terminal_result: nil,
       closed: false
     }}
  end

  @impl true
  def handle_call(
        {:open_session, cwd, model, effort, mcp, skill_roots, instructions},
        _from,
        %{backend_session_id: nil} = state
      ) do
    result =
      with :ok <- validate_open_input(cwd, mcp, instructions),
           :ok <- ensure_authenticated(state, cwd),
           {:ok, backend_session_id} <- create_chat(state, cwd),
           {:ok, plugin_dir} <- install_session_plugin(state, mcp, skill_roots, instructions) do
        File.mkdir_p!(Path.dirname(state.jsonl_path))

        JSONLWriter.append(state.jsonl_path, "control", %{
          "action" => "session_opened",
          "backend_session_id" => backend_session_id,
          "auth" => "cursor_cli"
        })

        session = %Session{
          id: state.session_id,
          backend: :cursor_headless,
          backend_protocol: @protocol,
          backend_session_id: backend_session_id,
          cwd: cwd,
          model: model,
          reasoning_effort: effort,
          jsonl_path: state.jsonl_path
        }

        state = %{
          state
          | backend_session_id: backend_session_id,
            cwd: cwd,
            model: model,
            reasoning_effort: effort,
            plugin_dir: plugin_dir
        }

        emit(state, :session_started,
          data: %{protocol: @protocol, process_lifecycle: :per_turn, auth: :cursor_cli}
        )

        {:ok, session, state}
      end

    case result do
      {:ok, session, state} ->
        {:reply, {:ok, session}, state}

      {:error, %Error{} = error} ->
        {:reply, {:error, error}, cleanup_plugin(state)}

      {:error, reason} ->
        {:reply, error(:session_open_failed, "could not open Cursor headless session", reason),
         cleanup_plugin(state)}
    end
  end

  def handle_call(
        {:open_session, _cwd, _model, _effort, _mcp, _skills, _instructions},
        _from,
        state
      ) do
    {:reply, error(:session_already_open, "Cursor headless session is already open", nil), state}
  end

  def handle_call({:start_turn, input}, _from, state) do
    case start_headless_turn(state, input) do
      {:ok, turn_id, state} -> {:reply, {:ok, turn_id}, state}
      {:error, %Error{} = reason, state} -> {:reply, {:error, reason}, state}
    end
  end

  def handle_call({:steer, _input}, _from, %{active_turn_id: nil} = state) do
    {:reply, error(:steer_failed, "there is no active Cursor headless turn to steer", nil), state}
  end

  def handle_call({:steer, input}, _from, state) do
    state = interrupt_active_turn(state, "steered")

    case start_headless_turn(state, input) do
      {:ok, turn_id, state} -> {:reply, {:ok, turn_id}, state}
      {:error, %Error{} = reason, state} -> {:reply, {:error, reason}, state}
    end
  end

  def handle_call(:interrupt, _from, %{active_turn_id: nil} = state) do
    {:reply, error(:no_active_turn, "there is no active Cursor headless turn", nil), state}
  end

  def handle_call(:interrupt, _from, state) do
    {:reply, :ok, interrupt_active_turn(state, "interrupted")}
  end

  def handle_call(:close_session, _from, state) do
    state =
      if state.active_turn_id, do: interrupt_active_turn(state, "session_closed"), else: state

    state = cleanup_plugin(state)
    emit(state, :process_exited, data: %{status: :closed, expected: true, os_pid: nil})
    {:reply, :ok, %{state | closed: true}}
  end

  def handle_call(:capabilities, _from, state) do
    {:reply,
     %{
       protocol: @protocol,
       native_steer: false,
       simulated_steer: :interrupt_then_resume,
       interrupt: true,
       close: :per_turn_process,
       process_lifecycle: :per_turn,
       http_mcp: true,
       system_instructions: :temporary_plugin_rule,
       skill_roots: :temporary_plugin_skills,
       provider_resume: :within_pika_session,
       provider_resume_required: false,
       reasoning_stream: false,
       reasoning_stream_reason: :cursor_print_mode_suppresses_thinking,
       automatic_retry: false
     }, state}
  end

  def handle_call(:process_os_pid, _from, %{transport: transport} = state)
      when is_pid(transport) do
    pid = if Process.alive?(transport), do: JSONLPort.os_pid(transport), else: nil
    {:reply, pid, state}
  end

  def handle_call(:process_os_pid, _from, state), do: {:reply, nil, state}

  @impl true
  def handle_info({:backend_wire, transport, message}, %{transport: transport} = state) do
    {:noreply, map_stream_event(message, state)}
  end

  def handle_info({:backend_wire, _transport, _message}, state), do: {:noreply, state}

  def handle_info(
        {:backend_wire_error, transport, line, decode_error},
        %{transport: transport} = state
      ) do
    emit(state, :backend_error,
      data: %{
        code: :invalid_stream_json,
        line: line,
        error: Exception.message(decode_error),
        retry: false
      }
    )

    {:noreply, state}
  end

  def handle_info(
        {:backend_process_exited, transport, status, _closed_by_client},
        %{transport: transport} = state
      ) do
    state = %{state | transport: nil}

    cond do
      is_nil(state.active_turn_id) ->
        {:noreply, reset_turn_state(state)}

      successful_result?(state.terminal_result, status) ->
        emit(state, :turn_completed,
          data:
            state.terminal_result
            |> Map.put_new("status", "completed")
            |> Map.put("process_status", status)
        )

        {:noreply, reset_turn_state(state)}

      true ->
        failure = %{
          code: :headless_turn_failed,
          status: status,
          terminal_result: state.terminal_result,
          retry: false,
          message: stderr_tail(state.stderr_path)
        }

        emit(state, :backend_error, data: failure)
        emit(state, :process_exited, data: %{status: status, expected: false, retry: false})
        {:noreply, reset_turn_state(state)}
    end
  end

  def handle_info(_message, state), do: {:noreply, state}

  @impl true
  def terminate(_reason, state) do
    if is_pid(state.transport) and Process.alive?(state.transport),
      do: JSONLPort.close(state.transport)

    cleanup_plugin(state)
    :ok
  end

  defp start_headless_turn(%{backend_session_id: nil} = state, _input) do
    {:error, unwrap(error(:session_not_open, "Cursor headless session is not open", nil)), state}
  end

  defp start_headless_turn(%{closed: true} = state, _input) do
    {:error, unwrap(error(:session_closed, "Cursor headless session is closed", nil)), state}
  end

  defp start_headless_turn(%{transport: transport} = state, _input) when is_pid(transport) do
    {:error,
     unwrap(error(:turn_already_active, "Cursor headless already has an active turn", nil)),
     state}
  end

  defp start_headless_turn(state, input) do
    with {:ok, prompt} <- normalize_prompt(input) do
      turn_id = Id.new("turn")
      item_id = "cursor-headless-message-#{turn_id}"
      args = turn_args(state, prompt)

      JSONLWriter.append(state.jsonl_path, "out", %{
        "type" => "pika_turn",
        "turn_id" => turn_id,
        "backend_session_id" => state.backend_session_id,
        "prompt" => prompt,
        "model" => model_with_effort(state.model, state.reasoning_effort)
      })

      case JSONLPort.start_link(
             owner: self(),
             command: state.profile.command || @default_command,
             args: args,
             env: state.profile.env,
             cwd: state.cwd,
             stderr_path: state.stderr_path,
             jsonl_path: state.jsonl_path
           ) do
        {:ok, transport} ->
          state = %{
            state
            | transport: transport,
              active_turn_id: turn_id,
              message_item_id: item_id,
              accumulated_text: "",
              terminal_result: nil
          }

          emit(state, :turn_started, turn_id: turn_id, data: %{process_lifecycle: :per_turn})
          {:ok, turn_id, state}

        {:error, reason} ->
          {:error,
           unwrap(error(:process_start_failed, "could not start Cursor headless turn", reason)),
           state}
      end
    else
      {:error, %Error{} = reason} -> {:error, reason, state}
    end
  end

  defp turn_args(state, prompt) do
    state.profile.args ++
      [
        "-p",
        "--resume",
        state.backend_session_id,
        "--output-format",
        "stream-json",
        "--stream-partial-output"
      ] ++
      model_args(state.model, state.reasoning_effort) ++
      PermissionPolicy.cursor_cli_args(
        state.profile.approval_policy,
        state.profile.sandbox_policy
      ) ++ ["--approve-mcps", "--trust", "--plugin-dir", state.plugin_dir, prompt]
  end

  defp map_stream_event(%{"type" => "assistant", "message" => message}, state) do
    delta = text_content(message["content"])

    if delta == "" do
      state
    else
      emit(state, :message_delta,
        data: %{
          delta: delta,
          item_id: state.message_item_id,
          phase: "commentary",
          delivery: "stream"
        }
      )

      %{state | accumulated_text: state.accumulated_text <> delta}
    end
  end

  defp map_stream_event(%{"type" => "tool_call", "subtype" => subtype} = event, state)
       when subtype in ["started", "completed"] do
    {event_type, status} =
      if subtype == "started",
        do: {:tool_started, "running"},
        else: {:tool_completed, "completed"}

    item = canonical_tool_item(event, status)
    data = %{"item" => item, "stage" => subtype, "provider_event" => "cursor_headless"}
    emit(state, event_type, data: data)
    maybe_emit_command_output(state, item, event_type)
    state
  end

  defp map_stream_event(%{"type" => "result"} = result, state) do
    state
    |> reconcile_terminal_text(result["result"])
    |> Map.put(:terminal_result, result)
  end

  defp map_stream_event(_event, state), do: state

  defp reconcile_terminal_text(state, final) when not is_binary(final) or final == "", do: state
  defp reconcile_terminal_text(%{accumulated_text: final} = state, final), do: state

  defp reconcile_terminal_text(state, final) do
    if String.starts_with?(final, state.accumulated_text) do
      suffix = String.replace_prefix(final, state.accumulated_text, "")

      if suffix != "" do
        emit(state, :message_delta,
          data: %{
            delta: suffix,
            item_id: state.message_item_id,
            phase: "final_answer",
            delivery: "terminal_repair"
          }
        )
      end
    else
      emit(state, :message_completed,
        data: %{
          item: %{
            "id" => state.message_item_id,
            "type" => "agentMessage",
            "text" => final,
            "phase" => "final_answer",
            "delivery" => "terminal_repair"
          }
        }
      )
    end

    %{state | accumulated_text: final}
  end

  defp interrupt_active_turn(state, reason) do
    turn_id = state.active_turn_id
    os_pid = if is_pid(state.transport), do: safe_os_pid(state.transport), else: nil

    if is_pid(state.transport) and Process.alive?(state.transport),
      do: JSONLPort.close(state.transport)

    emit(state, :turn_completed,
      turn_id: turn_id,
      data: %{status: "interrupted", stopReason: "interrupted", reason: reason, os_pid: os_pid}
    )

    state
    |> Map.put(:transport, nil)
    |> reset_turn_state()
  end

  defp ensure_authenticated(state, cwd) do
    case run_control(state, cwd, ["status"]) do
      {:ok, output} ->
        normalized = String.downcase(output)

        if String.contains?(normalized, "not authenticated") or
             String.contains?(normalized, "not logged in") do
          error(
            :not_authenticated,
            "Cursor CLI is not authenticated; run cursor-agent login",
            output
          )
        else
          :ok
        end

      {:error, reason} ->
        error(
          :not_authenticated,
          "Cursor CLI authentication check failed; run cursor-agent login",
          reason
        )
    end
  end

  defp create_chat(state, cwd) do
    with {:ok, output} <- run_control(state, cwd, ["create-chat"]),
         {:ok, id} <- parse_chat_id(output) do
      {:ok, id}
    else
      {:error, %Error{} = reason} -> {:error, reason}
      {:error, reason} -> error(:create_chat_failed, "Cursor CLI could not create a chat", reason)
    end
  end

  defp run_control(state, cwd, extra_args) do
    command = state.profile.command || @default_command
    args = state.profile.args ++ extra_args

    task =
      Task.async(fn ->
        System.cmd(command, args,
          cd: cwd,
          env: Map.to_list(state.profile.env),
          stderr_to_stdout: true
        )
      end)

    case Task.yield(task, @control_timeout) || Task.shutdown(task, :brutal_kill) do
      {:ok, {output, 0}} -> {:ok, output}
      {:ok, {output, status}} -> {:error, %{status: status, output: output}}
      {:exit, reason} -> {:error, reason}
      nil -> {:error, :timeout}
    end
  rescue
    exception -> {:error, Exception.message(exception)}
  end

  defp parse_chat_id(output) do
    uuid =
      Regex.run(
        ~r/[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}/i,
        output
      )
      |> List.wrap()
      |> List.first()

    candidate =
      uuid ||
        output
        |> String.split("\n", trim: true)
        |> List.last()
        |> to_string()
        |> String.trim()

    if candidate == "", do: {:error, :missing_chat_id}, else: {:ok, candidate}
  end

  defp install_session_plugin(state, mcp, skill_roots, instructions) do
    suffix = Base.url_encode64(:crypto.strong_rand_bytes(9), padding: false)

    plugin_dir =
      Path.join(System.tmp_dir!(), "pika-cursor-headless-#{state.session_id}-#{suffix}")

    try do
      File.mkdir_p!(Path.join(plugin_dir, ".cursor-plugin"))
      File.mkdir_p!(Path.join(plugin_dir, "rules"))
      File.chmod!(plugin_dir, 0o700)

      File.write!(
        Path.join([plugin_dir, ".cursor-plugin", "plugin.json"]),
        Jason.encode!(%{
          name: "pika-session",
          version: "1.0.0",
          description: "Ephemeral Pika backend session context"
        })
      )

      File.write!(Path.join([plugin_dir, "rules", "pika-system.mdc"]), cursor_rule(instructions))

      mcp_path = Path.join(plugin_dir, "mcp.json")

      File.write!(
        mcp_path,
        Jason.encode!(%{
          mcpServers: %{
            pika: %{
              url: Map.fetch!(mcp, :url),
              headers: %{Authorization: "Bearer #{Map.fetch!(mcp, :token)}"}
            }
          }
        })
      )

      File.chmod!(mcp_path, 0o600)
      copy_skills(plugin_dir, skill_roots)
      {:ok, plugin_dir}
    rescue
      exception ->
        File.rm_rf(plugin_dir)
        {:error, Exception.message(exception)}
    end
  end

  defp copy_skills(_plugin_dir, []), do: :ok

  defp copy_skills(plugin_dir, skill_roots) do
    destination_root = Path.join(plugin_dir, "skills")
    File.mkdir_p!(destination_root)

    Enum.with_index(skill_roots, 1)
    |> Enum.each(fn {source, index} ->
      name = source |> Path.basename() |> String.replace(~r/[^a-zA-Z0-9._-]/, "-")
      destination = Path.join(destination_root, "#{index}-#{name}")

      case File.cp_r(source, destination) do
        {:ok, _files} -> :ok
        {:error, reason, file} -> raise "could not copy skill #{file}: #{inspect(reason)}"
      end
    end)
  end

  defp cleanup_plugin(%{plugin_dir: nil} = state), do: state

  defp cleanup_plugin(state) do
    File.rm_rf(state.plugin_dir)
    %{state | plugin_dir: nil}
  end

  defp validate_open_input(cwd, mcp, instructions) do
    cond do
      not File.dir?(cwd) ->
        error(:invalid_workspace, "Cursor workspace does not exist", cwd)

      not is_binary(instructions) or instructions == "" ->
        error(:invalid_system_instructions, "Cursor system instructions are empty", instructions)

      not is_binary(mcp[:url]) or not is_binary(mcp[:token]) ->
        error(:invalid_mcp_config, "Cursor headless requires MCP URL and token", nil)

      true ->
        :ok
    end
  end

  defp normalize_prompt(input) when is_binary(input), do: {:ok, input}

  defp normalize_prompt(input) when is_list(input) do
    if Enum.all?(
         input,
         &(is_map(&1) and (&1["type"] in [nil, "text"] or &1[:type] in [nil, :text]))
       ) do
      text = Enum.map_join(input, "\n", &(&1["text"] || &1[:text] || ""))
      {:ok, text}
    else
      error(:unsupported_input, "Cursor headless only supports text input", input)
    end
  end

  defp normalize_prompt(input),
    do: error(:unsupported_input, "Cursor headless only supports text input", input)

  defp model_args(nil, _effort), do: []
  defp model_args("", _effort), do: []
  defp model_args(model, effort), do: ["--model", model_with_effort(model, effort)]

  defp model_with_effort(nil, _effort), do: nil
  defp model_with_effort(model, nil), do: model
  defp model_with_effort(model, ""), do: model

  defp model_with_effort(model, effort) do
    effort = to_string(effort)

    case Regex.run(~r/^(.*)\[([^]]*)\]$/, model, capture: :all_but_first) do
      [base, raw_params] ->
        params =
          raw_params
          |> String.split(",", trim: true)
          |> Enum.map(&String.trim/1)
          |> Enum.reject(&String.starts_with?(&1, "effort="))

        "#{base}[#{Enum.join(params ++ ["effort=#{effort}"], ",")}]"

      _ ->
        "#{model}[effort=#{effort}]"
    end
  end

  defp text_content(contents) when is_list(contents) do
    Enum.map_join(contents, "", fn
      %{"type" => "text", "text" => text} when is_binary(text) -> text
      %{type: :text, text: text} when is_binary(text) -> text
      _ -> ""
    end)
  end

  defp text_content(text) when is_binary(text), do: text
  defp text_content(_contents), do: ""

  defp canonical_tool_item(event, status) do
    call_id = event["call_id"] || Id.new("tool")
    tool_call = event["tool_call"] || %{}
    {provider_kind, details} = List.first(Map.to_list(tool_call)) || {"tool", %{}}
    details = if is_map(details), do: details, else: %{}
    args = details["args"] || %{}
    type = canonical_tool_type(provider_kind)

    %{
      "id" => call_id,
      "type" => type,
      "kind" => canonical_kind(type),
      "name" => provider_kind,
      "title" => humanize_tool(provider_kind),
      "status" => tool_status(details, status),
      "rawInput" => args,
      "command" => if(type == "commandExecution", do: command_from(args)),
      "server" => mcp_value(details, args, ~w(server serverName server_name)),
      "tool" => mcp_value(details, args, ~w(tool toolName tool_name name)),
      "result" => details["result"]
    }
    |> Enum.reject(fn {_key, value} -> is_nil(value) end)
    |> Map.new()
  end

  defp canonical_tool_type(kind) do
    normalized = String.downcase(kind)

    cond do
      String.contains?(normalized, ["shell", "terminal", "command"]) ->
        "commandExecution"

      String.contains?(normalized, ["write", "edit", "delete", "move"]) ->
        "fileChange"

      String.contains?(normalized, "mcp") ->
        "mcpToolCall"

      String.contains?(normalized, "web") and String.contains?(normalized, "search") ->
        "webSearch"

      true ->
        "dynamicToolCall"
    end
  end

  defp canonical_kind("commandExecution"), do: "execute"
  defp canonical_kind("fileChange"), do: "edit"
  defp canonical_kind("webSearch"), do: "web_search"
  defp canonical_kind(_type), do: "tool"

  defp humanize_tool(kind) do
    kind
    |> String.replace_suffix("ToolCall", "")
    |> Macro.underscore()
    |> String.replace("_", " ")
    |> String.capitalize()
  end

  defp command_from(args) when is_map(args) do
    args["command"] || args["cmd"] || args["script"]
  end

  defp command_from(_args), do: nil

  defp tool_status(details, default) do
    cond do
      get_in(details, ["result", "error"]) -> "failed"
      get_in(details, ["result", "failure"]) -> "failed"
      true -> default
    end
  end

  defp mcp_value(details, args, keys) do
    Enum.find_value(keys, &(details[&1] || if(is_map(args), do: args[&1])))
  end

  defp maybe_emit_command_output(state, %{"type" => "commandExecution"} = item, :tool_completed) do
    output = nested_output(item["result"])

    if is_binary(output) do
      emit(state, :command_output,
        data: %{
          "command_id" => item["id"],
          "output" => output,
          "update_mode" => "replace",
          "status" => item["status"]
        }
      )
    end
  end

  defp maybe_emit_command_output(_state, _item, _event_type), do: :ok

  defp nested_output(value) when is_binary(value), do: value

  defp nested_output(value) when is_map(value) do
    Enum.find_value(~w(output stdout content text), &if(is_binary(value[&1]), do: value[&1])) ||
      Enum.find_value(Map.values(value), &nested_output/1)
  end

  defp nested_output(value) when is_list(value), do: Enum.find_value(value, &nested_output/1)
  defp nested_output(_value), do: nil

  defp successful_result?(%{"type" => "result"} = result, 0) do
    result["subtype"] == "success" and result["is_error"] != true
  end

  defp successful_result?(_result, _status), do: false

  defp reset_turn_state(state) do
    %{
      state
      | active_turn_id: nil,
        message_item_id: nil,
        accumulated_text: "",
        terminal_result: nil
    }
  end

  defp stderr_tail(path) do
    case File.read(path) do
      {:ok, contents} -> contents |> String.slice(-4_000, 4_000) |> String.trim()
      {:error, _reason} -> ""
    end
  end

  defp safe_os_pid(transport) do
    JSONLPort.os_pid(transport)
  catch
    :exit, _reason -> nil
  end

  defp cursor_rule(instructions) do
    "---\ndescription: Pika Backend Session system instructions\nalwaysApply: true\n---\n\n" <>
      instructions <> "\n"
  end

  defp emit(state, type, attrs) do
    event =
      Event.new(type, :cursor_headless, state.session_id, %{
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

  defp error(code, message, details),
    do: {:error, %Error{code: code, message: message, details: %{raw: details}}}

  defp unwrap({:error, %Error{} = reason}), do: reason
end
