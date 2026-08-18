defmodule Pika.AgentBackend.CodexAppServer do
  @moduledoc "Codex App Server stdio JSON-RPC adapter."

  use GenServer
  @behaviour Pika.AgentBackend

  alias Pika.AgentBackend.{CodexExecutable, Error, Event, Id, JSONLPort, Profile, Session}

  @protocol "codex-app-server-v2"
  @rpc_timeout 60_000

  @impl true
  def start_link(profile, event_sink), do: GenServer.start_link(__MODULE__, {profile, event_sink})

  @impl true
  def open_session(server, cwd, model, reasoning_effort, mcp, skill_roots) do
    cwd = Path.expand(cwd)
    skill_roots = Enum.map(skill_roots, &Path.expand/1)

    with :ok <- GenServer.call(server, {:start_transport, mcp}, @rpc_timeout),
         {:ok, _} <- rpc(server, "initialize", initialize_params()),
         :ok <- notify(server, "initialized", %{}),
         :ok <- configure_skills(server, cwd, skill_roots),
         {:ok, response} <-
           rpc(server, "thread/start", thread_start_params(cwd, model, reasoning_effort)),
         {:ok, backend_session_id} <- fetch_id(response, ["thread", "id"]),
         {:ok, session} <-
           GenServer.call(
             server,
             {:establish_session, backend_session_id, cwd, model, reasoning_effort, skill_roots}
           ) do
      {:ok, session}
    end
  end

  @impl true
  def start_turn(server, input) do
    with {:ok, state} <- GenServer.call(server, :turn_context),
         items <- normalize_input(input, state.skill_roots),
         params <-
           compact(%{
             "threadId" => state.backend_session_id,
             "input" => items,
             "effort" => normalize_effort(state.reasoning_effort),
             "approvalPolicy" => "never",
             "sandboxPolicy" => %{"type" => "dangerFullAccess"}
           }),
         {:ok, response} <- rpc(server, "turn/start", params),
         {:ok, turn_id} <- fetch_id(response, ["turn", "id"]) do
      GenServer.cast(server, {:active_turn, turn_id})
      {:ok, turn_id}
    end
  end

  @impl true
  def steer(server, input) do
    with {:ok, state} <- GenServer.call(server, :turn_context),
         true <- is_binary(state.active_turn_id) || steer_error(),
         params <- %{
           "threadId" => state.backend_session_id,
           "expectedTurnId" => state.active_turn_id,
           "input" => normalize_input(input, [])
         },
         {:ok, response} <- rpc(server, "turn/steer", params),
         {:ok, turn_id} <- fetch_id(response, ["turnId"]) do
      {:ok, turn_id}
    else
      {:error, %Error{} = error} -> {:error, error}
      false -> steer_error()
      other -> rpc_error(:steer_failed, "Codex turn/steer failed", other)
    end
  end

  @impl true
  def interrupt(server) do
    with {:ok, state} <- GenServer.call(server, :turn_context),
         true <- is_binary(state.active_turn_id) || no_active_turn_error(),
         {:ok, _} <-
           rpc(server, "turn/interrupt", %{
             "threadId" => state.backend_session_id,
             "turnId" => state.active_turn_id
           }) do
      :ok
    else
      {:error, %Error{} = error} -> {:error, error}
      false -> no_active_turn_error()
      other -> rpc_error(:interrupt_failed, "Codex turn/interrupt failed", other)
    end
  end

  @impl true
  def close_session(server), do: GenServer.call(server, :close, @rpc_timeout)

  @impl true
  def capabilities(server), do: GenServer.call(server, :capabilities)

  def process_os_pid(server), do: GenServer.call(server, :process_os_pid)

  @impl true
  def init({profile, event_sink}) do
    profile = Profile.normalize(profile)
    session_id = Id.new("session")

    {:ok,
     %{
       profile: profile,
       event_sink: event_sink,
       session_id: session_id,
       transport: nil,
       pending: %{},
       next_id: 1,
       backend_session_id: nil,
       active_turn_id: nil,
       cwd: nil,
       model: nil,
       reasoning_effort: nil,
       skill_roots: [],
       jsonl_path: nil,
       closed: false,
       process_exit_emitted: false
     }}
  end

  @impl true
  def handle_call({:start_transport, mcp}, _from, %{transport: nil} = state) do
    profile = state.profile
    artifact_dir = Path.expand(profile.artifact_dir)
    jsonl_path = Path.join(artifact_dir, "codex-#{state.session_id}.jsonl")
    stderr_path = Path.join(artifact_dir, "codex-#{state.session_id}.stderr.log")
    command = CodexExecutable.resolve(profile.command)

    args =
      profile.args ++
        ["app-server", "--listen", "stdio://"] ++ codex_mcp_args(mcp)

    env = Map.merge(profile.env, %{"PIKA_MCP_TOKEN" => Map.fetch!(mcp, :token)})

    case JSONLPort.start_link(
           owner: self(),
           command: command,
           args: args,
           env: env,
           stderr_path: stderr_path,
           jsonl_path: jsonl_path
         ) do
      {:ok, transport} ->
        {:reply, :ok, %{state | transport: transport, jsonl_path: jsonl_path}}

      {:error, reason} ->
        {:reply, rpc_error(:process_start_failed, "could not start Codex App Server", reason),
         state}
    end
  end

  def handle_call({:start_transport, _mcp}, _from, state), do: {:reply, :ok, state}

  def handle_call({:rpc, method, params}, from, state) do
    id = state.next_id
    message = %{"id" => id, "method" => method, "params" => params}

    case JSONLPort.send_message(state.transport, message) do
      :ok ->
        {:noreply, %{state | next_id: id + 1, pending: Map.put(state.pending, id, from)}}

      {:error, reason} ->
        {:reply, rpc_error(:transport_closed, "Codex transport is closed", reason), state}
    end
  end

  def handle_call({:notify, method, params}, _from, state) do
    result = JSONLPort.send_message(state.transport, %{"method" => method, "params" => params})
    {:reply, result, state}
  end

  def handle_call({:establish_session, backend_id, cwd, model, effort, skill_roots}, _from, state) do
    session = %Session{
      id: state.session_id,
      backend: :codex_app_server,
      backend_protocol: @protocol,
      backend_session_id: backend_id,
      cwd: cwd,
      model: model,
      reasoning_effort: effort,
      jsonl_path: state.jsonl_path
    }

    state = %{
      state
      | backend_session_id: backend_id,
        cwd: cwd,
        model: model,
        reasoning_effort: effort,
        skill_roots: skill_roots
    }

    emit(state, :session_started, data: %{protocol: @protocol})
    {:reply, {:ok, session}, state}
  end

  def handle_call(:turn_context, _from, state) do
    if state.backend_session_id do
      {:reply, {:ok, state}, state}
    else
      {:reply, rpc_error(:session_not_open, "Codex session has not been opened", nil), state}
    end
  end

  def handle_call(:capabilities, _from, state) do
    {:reply,
     %{
       protocol: @protocol,
       native_steer: true,
       interrupt: true,
       close: :process,
       http_mcp: true,
       skill_roots: true,
       provider_resume_required: false
     }, state}
  end

  def handle_call(:process_os_pid, _from, %{transport: nil} = state), do: {:reply, nil, state}

  def handle_call(:process_os_pid, _from, state),
    do: {:reply, JSONLPort.os_pid(state.transport), state}

  def handle_call(:close, _from, %{transport: nil} = state), do: {:reply, :ok, state}

  def handle_call(:close, _from, state) do
    :ok = JSONLPort.close(state.transport)
    emit(state, :process_exited, data: %{status: :closed, expected: true})
    {:reply, :ok, %{state | closed: true, transport: nil, process_exit_emitted: true}}
  end

  @impl true
  def handle_cast({:active_turn, turn_id}, state),
    do: {:noreply, %{state | active_turn_id: turn_id}}

  @impl true
  def handle_info(
        {:backend_wire, transport, %{"id" => id} = message},
        %{transport: transport} = state
      ) do
    cond do
      Map.has_key?(state.pending, id) -> handle_response(id, message, state)
      Map.has_key?(message, "method") -> handle_server_request(message, state)
      true -> {:noreply, state}
    end
  end

  def handle_info(
        {:backend_wire, transport, %{"method" => method, "params" => params}},
        %{transport: transport} = state
      ) do
    {:noreply, map_notification(method, params, state)}
  end

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
    Enum.each(state.pending, fn {_id, from} ->
      GenServer.reply(from, rpc_error(:process_exited, "Codex App Server exited", status))
    end)

    emit(state, :process_exited,
      data: %{status: status, expected: closed_by_client || state.closed}
    )

    {:stop, :normal,
     %{state | transport: nil, pending: %{}, active_turn_id: nil, process_exit_emitted: true}}
  end

  def handle_info(_message, state), do: {:noreply, state}

  @impl true
  def terminate(_reason, %{transport: transport}) when is_pid(transport) do
    if Process.alive?(transport), do: JSONLPort.close(transport)
    :ok
  end

  def terminate(_reason, _state), do: :ok

  defp rpc(server, method, params),
    do: GenServer.call(server, {:rpc, method, params}, @rpc_timeout)

  defp notify(server, method, params),
    do: GenServer.call(server, {:notify, method, params}, @rpc_timeout)

  defp handle_response(id, message, state) do
    {from, pending} = Map.pop!(state.pending, id)

    reply =
      case message do
        %{"result" => result} ->
          {:ok, result}

        %{"error" => error} ->
          rpc_error(:provider_error, error["message"] || "Codex RPC error", error)
      end

    GenServer.reply(from, reply)
    {:noreply, %{state | pending: pending}}
  end

  defp handle_server_request(%{"id" => id, "method" => method}, state) do
    result = approval_response(method)

    response =
      if result do
        %{"id" => id, "result" => result}
      else
        %{"id" => id, "error" => %{"code" => -32_601, "message" => "unsupported client request"}}
      end

    JSONLPort.send_message(state.transport, response)
    {:noreply, state}
  end

  defp approval_response(method)
       when method in ["execCommandApproval", "item/commandExecution/requestApproval"],
       do: %{"decision" => "acceptForSession"}

  defp approval_response(method)
       when method in ["applyPatchApproval", "item/fileChange/requestApproval"],
       do: %{"decision" => "acceptForSession"}

  defp approval_response(_), do: nil

  defp map_notification("turn/started", params, state) do
    turn_id = get_in(params, ["turn", "id"]) || params["turnId"]
    emit(state, :turn_started, turn_id: turn_id)
    %{state | active_turn_id: turn_id}
  end

  defp map_notification("item/agentMessage/delta", params, state) do
    emit(state, :message_delta,
      turn_id: params["turnId"],
      data: %{delta: params["delta"], item_id: params["itemId"]}
    )

    state
  end

  defp map_notification(method, params, state)
       when method in ["item/plan/delta", "turn/plan/updated"] do
    emit(state, :plan_updated, turn_id: params["turnId"], data: params)
    state
  end

  defp map_notification("item/started", %{"item" => item} = params, state) do
    map_item(:started, item, params, state)
  end

  defp map_notification("item/completed", %{"item" => item} = params, state) do
    map_item(:completed, item, params, state)
  end

  defp map_notification(method, params, state)
       when method in ["item/commandExecution/outputDelta", "command/exec/outputDelta"] do
    emit(state, :command_output, turn_id: params["turnId"], data: params)
    state
  end

  defp map_notification(method, params, state)
       when method in [
              "item/fileChange/outputDelta",
              "item/fileChange/patchUpdated",
              "turn/diff/updated"
            ] do
    emit(state, :file_changed, turn_id: params["turnId"], data: params)
    state
  end

  defp map_notification("item/mcpToolCall/progress", params, state) do
    emit(state, :tool_updated, turn_id: params["turnId"], data: params)
    state
  end

  defp map_notification("thread/tokenUsage/updated", params, state) do
    emit(state, :usage_updated, turn_id: params["turnId"], data: params["tokenUsage"] || params)
    state
  end

  defp map_notification("turn/completed", params, state) do
    turn = params["turn"] || %{}
    turn_id = turn["id"] || params["turnId"]
    emit(state, :turn_completed, turn_id: turn_id, data: turn)
    %{state | active_turn_id: nil}
  end

  defp map_notification("error", params, state) do
    emit(state, :backend_error, turn_id: params["turnId"], data: params)
    state
  end

  defp map_notification(_unknown, _params, state), do: state

  defp map_item(stage, item, params, state) do
    type = item["type"]

    event_type =
      case {stage, type} do
        {:started, "fileChange"} -> :file_changed
        {:completed, "fileChange"} -> :file_changed
        {:started, _} -> :tool_started
        {:completed, _} -> :tool_completed
      end

    emit(state, event_type, turn_id: params["turnId"], data: %{item: item})
    state
  end

  defp emit(state, type, attrs) do
    event =
      Event.new(type, :codex_app_server, state.session_id, %{
        backend_session_id: state.backend_session_id,
        turn_id: Keyword.get(attrs, :turn_id, state.active_turn_id),
        data: Keyword.get(attrs, :data, %{})
      })

    case state.event_sink do
      sink when is_pid(sink) -> send(sink, {:pika_backend_event, event})
      sink when is_function(sink, 1) -> sink.(event)
    end
  end

  defp initialize_params do
    %{
      "clientInfo" => %{"name" => "pika", "title" => "Pika", "version" => "0.0.1"},
      "capabilities" => %{"experimentalApi" => false}
    }
  end

  defp configure_skills(_server, _cwd, []), do: :ok

  defp configure_skills(server, cwd, skill_roots) do
    with {:ok, _} <- rpc(server, "skills/extraRoots/set", %{"extraRoots" => skill_roots}),
         {:ok, _} <- rpc(server, "skills/list", %{"cwds" => [cwd], "forceReload" => true}) do
      :ok
    end
  end

  defp thread_start_params(cwd, model, reasoning_effort) do
    compact(%{
      "cwd" => cwd,
      "model" => model,
      "approvalPolicy" => "never",
      "sandbox" => "danger-full-access",
      "ephemeral" => true,
      "config" => compact(%{"model_reasoning_effort" => normalize_effort(reasoning_effort)})
    })
  end

  defp normalize_input(input, skill_roots) do
    text_items =
      case input do
        text when is_binary(text) -> [%{"type" => "text", "text" => text}]
        items when is_list(items) -> items
      end

    skill_items =
      Enum.map(skill_roots, fn path ->
        %{"type" => "skill", "name" => skill_name(path), "path" => Path.join(path, "SKILL.md")}
      end)

    text_items ++ skill_items
  end

  defp skill_name(path) do
    case File.read(Path.join(path, "SKILL.md")) do
      {:ok, contents} ->
        case Regex.run(~r/^name:\s*(.+)$/m, contents) do
          [_, name] -> String.trim(name)
          _ -> Path.basename(path)
        end

      _ ->
        Path.basename(path)
    end
  end

  defp codex_mcp_args(mcp) do
    url = Map.fetch!(mcp, :url)

    [
      "-c",
      "mcp_servers.pika.url=#{Jason.encode!(url)}",
      "-c",
      "mcp_servers.pika.bearer_token_env_var=\"PIKA_MCP_TOKEN\"",
      "-c",
      "mcp_servers.pika.required=true"
    ]
  end

  defp fetch_id(map, path) do
    case get_in(map, path) do
      id when is_binary(id) ->
        {:ok, id}

      _ ->
        rpc_error(
          :invalid_response,
          "provider response did not contain #{Enum.join(path, ".")}",
          map
        )
    end
  end

  defp normalize_effort(nil), do: nil
  defp normalize_effort(value) when is_atom(value), do: Atom.to_string(value)
  defp normalize_effort(value), do: value

  defp compact(map), do: Map.reject(map, fn {_key, value} -> is_nil(value) end)

  defp steer_error, do: rpc_error(:steer_failed, "there is no active Codex turn to steer", nil)
  defp no_active_turn_error, do: rpc_error(:no_active_turn, "there is no active Codex turn", nil)

  defp rpc_error(code, message, details),
    do: {:error, %Error{code: code, message: message, details: %{raw: details}}}
end
