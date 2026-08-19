defmodule Pika.WorkspaceLock do
  @moduledoc false

  use GenServer

  def start_link({plan, opts}),
    do: GenServer.start_link(__MODULE__, plan, Keyword.put_new(opts, :name, __MODULE__))

  def start_link(plan), do: GenServer.start_link(__MODULE__, plan, name: __MODULE__)

  def start({plan, opts}),
    do: GenServer.start(__MODULE__, plan, Keyword.put_new(opts, :name, __MODULE__))

  def workspace(server \\ __MODULE__), do: GenServer.call(server, :workspace)

  @impl true
  def init(plan) do
    Process.flag(:trap_exit, true)

    with {:ok, port} <- acquire(plan),
         {:ok, workspace} <- Pika.Workspace.activate(plan) do
      {:ok, %{port: port, workspace: workspace}}
    else
      {:error, reason} -> {:stop, reason}
    end
  end

  @impl true
  def handle_call(:workspace, _from, state), do: {:reply, state.workspace, state}

  @impl true
  def handle_info({port, {:exit_status, status}}, %{port: port} = state),
    do: {:stop, {:workspace_lock_process_exited, status}, state}

  def handle_info(_message, state), do: {:noreply, state}

  @impl true
  def terminate(_reason, %{port: port}) when is_port(port) do
    Port.close(port)
    :ok
  rescue
    ArgumentError -> :ok
  end

  def terminate(_reason, _state), do: :ok

  defp acquire(%{mode: :owned_repo}), do: {:ok, nil}

  defp acquire(%{lock_path: path, config: config}) do
    with python when is_binary(python) <- System.find_executable("python3") do
      diagnostics = %{
        server_uuid: Ecto.UUID.generate(),
        pid: System.pid(),
        started_at: DateTime.utc_now() |> DateTime.to_iso8601(),
        workspace: config.workspace
      }

      encoded = Jason.encode!(diagnostics) <> "\n"

      script = """
      import fcntl, os, sys
      path, payload = sys.argv[1], sys.argv[2].encode('utf-8')
      fd = os.open(path, os.O_RDWR | os.O_CREAT, 0o600)
      try:
          fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
      except BlockingIOError:
          print('BUSY', flush=True)
          sys.exit(73)
      os.ftruncate(fd, 0)
      os.write(fd, payload)
      os.fsync(fd)
      print('LOCKED', flush=True)
      while os.read(0, 1):
          pass
      """

      port =
        Port.open({:spawn_executable, python}, [
          :binary,
          :exit_status,
          {:line, 1024},
          args: ["-u", "-c", script, path, encoded]
        ])

      await_lock(port, path)
    else
      nil -> {:error, {:missing_dependency, "python3", :managed_repo_lock}}
    end
  end

  defp await_lock(port, path) do
    receive do
      {^port, {:data, {:eol, "LOCKED"}}} ->
        {:ok, port}

      {^port, {:data, {:eol, "BUSY"}}} ->
        diagnostic =
          case File.read(path) do
            {:ok, value} -> String.trim(value)
            _ -> "unavailable"
          end

        Port.close(port)
        {:error, {:managed_repo_locked, path, diagnostic}}

      {^port, {:exit_status, status}} ->
        {:error, {:managed_repo_lock_failed, path, status}}
    after
      5_000 ->
        Port.close(port)
        {:error, {:managed_repo_lock_timeout, path}}
    end
  end
end
