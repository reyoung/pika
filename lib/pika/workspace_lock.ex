defmodule Pika.WorkspaceLock do
  @moduledoc "Cross-process ownership lock for exactly one v2 process per Workspace."

  use GenServer

  def start_link(opts) do
    case Keyword.get(opts, :name, __MODULE__) do
      nil -> GenServer.start_link(__MODULE__, opts)
      name -> GenServer.start_link(__MODULE__, opts, name: name)
    end
  end

  def snapshot(server \\ __MODULE__), do: GenServer.call(server, :snapshot)

  @impl true
  def init(opts) do
    workspace = opts |> Keyword.fetch!(:workspace) |> Pika.Paths.canonical!()
    path = Path.join(workspace, ".pika.lock")
    token = Ecto.UUID.generate()

    case acquire(path, token, 0) do
      {:ok, metadata} -> {:ok, %{path: path, token: token, metadata: metadata}}
      {:error, reason} -> {:stop, reason}
    end
  end

  @impl true
  def handle_call(:snapshot, _from, state), do: {:reply, state.metadata, state}

  @impl true
  def terminate(_reason, state) do
    case File.read(state.path) do
      {:ok, contents} ->
        case Jason.decode(contents) do
          {:ok, %{"token" => token}} when token == state.token -> File.rm(state.path)
          _other -> :ok
        end

      {:error, _reason} ->
        :ok
    end

    :ok
  end

  defp acquire(path, token, retries) when retries <= 1 do
    metadata = metadata(token)

    case File.open(path, [:write, :exclusive, :binary]) do
      {:ok, io} ->
        result =
          with :ok <- IO.binwrite(io, Jason.encode!(metadata)),
               :ok <- :file.sync(io) do
            :ok
          end

        File.close(io)

        case result do
          :ok -> {:ok, metadata}
          {:error, reason} -> {:error, {:workspace_lock_write_failed, reason}}
        end

      {:error, :eexist} ->
        with {:ok, existing} <- read_lock(path),
             false <- owner_alive?(existing) do
          case File.rm(path) do
            :ok -> acquire(path, token, retries + 1)
            {:error, reason} -> {:error, {:stale_workspace_lock_remove_failed, reason}}
          end
        else
          true -> {:error, {:workspace_already_locked, existing_identity(path)}}
          {:error, reason} -> {:error, reason}
        end

      {:error, reason} ->
        {:error, {:workspace_lock_create_failed, reason}}
    end
  end

  defp acquire(path, _token, _retries), do: {:error, {:workspace_lock_race, path}}

  defp metadata(token) do
    {:ok, hostname} = :inet.gethostname()

    %{
      "schema_version" => 1,
      "token" => token,
      "hostname" => to_string(hostname),
      "os_pid" => System.pid(),
      "started_at" => DateTime.utc_now() |> DateTime.to_iso8601()
    }
  end

  defp read_lock(path) do
    with {:ok, contents} <- File.read(path),
         {:ok, value} when is_map(value) <- Jason.decode(contents) do
      {:ok, value}
    else
      {:error, reason} -> {:error, {:invalid_workspace_lock, reason}}
      _other -> {:error, :invalid_workspace_lock}
    end
  end

  defp owner_alive?(%{"hostname" => hostname, "os_pid" => pid}) do
    {:ok, current_hostname} = :inet.gethostname()

    if hostname == to_string(current_hostname) and Regex.match?(~r/^\d+$/, to_string(pid)) do
      case System.cmd("kill", ["-0", to_string(pid)], stderr_to_stdout: true) do
        {_output, 0} -> true
        {_output, _status} -> false
      end
    else
      true
    end
  end

  defp owner_alive?(_metadata), do: true

  defp existing_identity(path) do
    case read_lock(path) do
      {:ok, metadata} -> metadata
      {:error, reason} -> %{path: path, error: inspect(reason)}
    end
  end
end
