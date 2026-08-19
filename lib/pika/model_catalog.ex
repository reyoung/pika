defmodule Pika.ModelCatalog do
  @moduledoc false

  alias Pika.AgentBackend.JSONLPort

  @timeout 10_000

  def list(backend, opts \\ [])

  def list("cursor_acp", opts) do
    runner = Keyword.get(opts, :cursor_runner, &cursor_models/0)

    with {:ok, output} <- runner.() do
      models =
        output
        |> String.split("\n", trim: true)
        |> Enum.flat_map(fn line ->
          case Regex.run(~r/^(\S+)\s+-\s+(.+)$/, String.trim(line)) do
            [_, "auto", _label] -> []
            [_, id, label] -> [%{id: id, label: label, description: nil}]
            _ -> []
          end
        end)

      non_empty(models)
    end
  end

  def list("codex_app_server", opts) do
    lister = Keyword.get(opts, :codex_lister, &codex_models/0)

    with {:ok, models} <- lister.() do
      models =
        models
        |> Enum.reject(&(&1["hidden"] == true))
        |> Enum.map(fn model ->
          %{
            id: model["model"] || model["id"],
            label: model["displayName"] || model["model"] || model["id"],
            description: model["description"],
            default: model["isDefault"] == true
          }
        end)
        |> Enum.filter(&(is_binary(&1.id) and &1.id != ""))
        |> Enum.sort_by(&if(&1.default, do: 0, else: 1))

      non_empty(models)
    end
  end

  def list(_backend, _opts), do: {:error, :unsupported_backend}

  defp cursor_models do
    case System.find_executable("cursor-agent") do
      nil ->
        {:error, :cursor_agent_not_found}

      command ->
        task =
          Task.async(fn -> System.cmd(command, ["--list-models"], stderr_to_stdout: true) end)

        case Task.yield(task, @timeout) || Task.shutdown(task, :brutal_kill) do
          {:ok, {output, 0}} -> {:ok, output}
          {:ok, {output, status}} -> {:error, {:cursor_model_list_failed, status, output}}
          {:exit, reason} -> {:error, {:cursor_model_list_failed, reason}}
          nil -> {:error, :cursor_model_list_timeout}
        end
    end
  end

  defp codex_models do
    case System.find_executable("codex") do
      nil ->
        {:error, :codex_not_found}

      command ->
        with_temporary_transport(command, fn transport ->
          with :ok <- rpc_send(transport, 1, "initialize", initialize_params()),
               {:ok, _result} <- rpc_receive(transport, 1),
               :ok <-
                 JSONLPort.send_message(transport, %{"method" => "initialized", "params" => %{}}),
               :ok <- rpc_send(transport, 2, "model/list", %{"includeHidden" => false}),
               {:ok, result} <- rpc_receive(transport, 2),
               models when is_list(models) <- result["data"] do
            {:ok, models}
          else
            {:error, _reason} = error -> error
            other -> {:error, {:invalid_codex_model_list, other}}
          end
        end)
    end
  end

  defp with_temporary_transport(command, fun) do
    directory =
      Path.join(
        System.tmp_dir!(),
        "pika-model-catalog-#{Base.url_encode64(:crypto.strong_rand_bytes(12), padding: false)}"
      )

    File.mkdir_p!(directory)

    result =
      JSONLPort.start(
        owner: self(),
        command: command,
        args: ["app-server", "--listen", "stdio://"],
        env: %{},
        stderr_path: Path.join(directory, "stderr.log"),
        jsonl_path: Path.join(directory, "protocol.jsonl")
      )

    try do
      case result do
        {:ok, transport} ->
          try do
            fun.(transport)
          after
            if Process.alive?(transport), do: JSONLPort.close(transport)
          end

        {:error, reason} ->
          {:error, {:codex_model_list_start_failed, reason}}
      end
    after
      File.rm_rf(directory)
    end
  end

  defp rpc_send(transport, id, method, params),
    do: JSONLPort.send_message(transport, %{"id" => id, "method" => method, "params" => params})

  defp rpc_receive(transport, id) do
    receive do
      {:backend_wire, ^transport, %{"id" => ^id, "result" => result}} ->
        {:ok, result}

      {:backend_wire, ^transport, %{"id" => ^id, "error" => error}} ->
        {:error, {:codex_rpc_error, error}}

      {:backend_wire, ^transport, _message} ->
        rpc_receive(transport, id)

      {:backend_wire_error, ^transport, _line, error} ->
        {:error, {:codex_protocol_error, Exception.message(error)}}

      {:backend_process_exited, ^transport, status, _closed_by_client} ->
        {:error, {:codex_process_exited, status}}
    after
      @timeout -> {:error, :codex_model_list_timeout}
    end
  end

  defp initialize_params do
    %{
      "clientInfo" => %{"name" => "pika", "title" => "Pika", "version" => "0.0.1"},
      "capabilities" => %{"experimentalApi" => false}
    }
  end

  defp non_empty([]), do: {:error, :empty_model_catalog}
  defp non_empty(models), do: {:ok, models}
end
