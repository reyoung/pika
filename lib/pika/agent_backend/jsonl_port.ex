defmodule Pika.AgentBackend.JSONLPort do
  @moduledoc "A non-PTY JSONL subprocess transport with isolated stderr and partial-line handling."

  use GenServer

  alias Pika.AgentBackend.JSONLWriter

  def start(opts), do: GenServer.start(__MODULE__, opts)
  def start_link(opts), do: GenServer.start_link(__MODULE__, opts)
  def send_message(server, message), do: GenServer.call(server, {:send, message})
  def close(server), do: GenServer.call(server, :close)
  def os_pid(server), do: GenServer.call(server, :os_pid)

  @impl true
  def init(opts) do
    Process.flag(:trap_exit, true)
    owner = Keyword.fetch!(opts, :owner)
    command = Keyword.fetch!(opts, :command)
    args = Keyword.get(opts, :args, [])
    env = Keyword.get(opts, :env, %{})
    stderr_path = Keyword.fetch!(opts, :stderr_path)
    jsonl_path = Keyword.fetch!(opts, :jsonl_path)
    cwd = Keyword.get(opts, :cwd)

    File.mkdir_p!(Path.dirname(stderr_path))
    File.touch!(stderr_path)

    wrapper = System.find_executable("sh") || "/bin/sh"
    script = ~S(exec "$@" 2>> "$PIKA_STDERR_LOG")

    port_env =
      env
      |> Map.put("PIKA_STDERR_LOG", stderr_path)
      |> Enum.map(fn {key, value} -> {to_charlist(key), to_charlist(value)} end)

    port_options = [
      :binary,
      :exit_status,
      :hide,
      args: ["-c", script, "pika-backend", command | args],
      env: port_env
    ]

    port_options =
      if is_binary(cwd),
        do: [{:cd, to_charlist(Path.expand(cwd))} | port_options],
        else: port_options

    port = Port.open({:spawn_executable, wrapper}, port_options)

    os_pid =
      case Port.info(port, :os_pid) do
        {:os_pid, pid} -> pid
        nil -> nil
      end

    {:ok,
     %{
       port: port,
       owner: owner,
       buffer: "",
       jsonl_path: jsonl_path,
       closed_by_client: false,
       os_pid: os_pid
     }}
  end

  @impl true
  def handle_call({:send, message}, _from, state) do
    JSONLWriter.append(state.jsonl_path, "out", message)
    result = Port.command(state.port, Jason.encode!(message) <> "\n")
    {:reply, if(result, do: :ok, else: {:error, :port_closed}), state}
  end

  def handle_call(:close, _from, %{port: nil} = state), do: {:reply, :ok, state}

  def handle_call(:close, _from, state) do
    stop_os_process(state.os_pid)
    safe_port_close(state.port)
    {:stop, :normal, :ok, %{state | closed_by_client: true, port: nil}}
  end

  def handle_call(:os_pid, _from, state), do: {:reply, state.os_pid, state}

  @impl true
  def handle_info({port, {:data, chunk}}, %{port: port} = state) do
    {lines, buffer} = split_lines(state.buffer <> chunk)

    Enum.each(lines, fn line ->
      case Jason.decode(line) do
        {:ok, message} ->
          JSONLWriter.append(state.jsonl_path, "in", message)
          send(state.owner, {:backend_wire, self(), message})

        {:error, error} ->
          JSONLWriter.append(state.jsonl_path, "decode_error", %{
            "line" => line,
            "error" => Exception.message(error)
          })

          send(state.owner, {:backend_wire_error, self(), line, error})
      end
    end)

    {:noreply, %{state | buffer: buffer}}
  end

  def handle_info({port, {:exit_status, status}}, %{port: port} = state) do
    if state.buffer != "" do
      JSONLWriter.append(state.jsonl_path, "trailing_partial", %{"line" => state.buffer})
    end

    send(state.owner, {:backend_process_exited, self(), status, state.closed_by_client})
    {:stop, :normal, %{state | port: nil}}
  end

  def handle_info({:EXIT, owner, reason}, %{owner: owner} = state), do: {:stop, reason, state}
  def handle_info({:EXIT, _port, _reason}, state), do: {:noreply, state}
  def handle_info(_message, state), do: {:noreply, state}

  @impl true
  def terminate(_reason, %{port: port, os_pid: os_pid}) when not is_nil(port) do
    stop_os_process(os_pid)
    safe_port_close(port)
    :ok
  end

  def terminate(_reason, _state), do: :ok

  defp split_lines(data) do
    parts = String.split(data, "\n")
    {Enum.drop(parts, -1), List.last(parts) || ""}
  end

  defp stop_os_process(os_pid) when is_integer(os_pid) do
    _ = System.cmd("kill", ["-TERM", Integer.to_string(os_pid)], stderr_to_stdout: true)
    wait_for_exit(os_pid, 10)
  end

  defp stop_os_process(nil), do: :ok

  defp wait_for_exit(os_pid, attempts_left) do
    case System.cmd("kill", ["-0", Integer.to_string(os_pid)], stderr_to_stdout: true) do
      {_output, status} when status != 0 ->
        :ok

      {_output, 0} when attempts_left > 0 ->
        Process.sleep(25)
        wait_for_exit(os_pid, attempts_left - 1)

      {_output, 0} ->
        _ = System.cmd("kill", ["-KILL", Integer.to_string(os_pid)], stderr_to_stdout: true)
        :ok
    end
  end

  defp safe_port_close(port) do
    Port.close(port)
  rescue
    ArgumentError -> :ok
  end
end
