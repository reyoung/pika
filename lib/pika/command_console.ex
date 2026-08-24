defmodule Pika.CommandConsole do
  @moduledoc "Persistent, provider-neutral command output captured under Backend Session artifacts."

  alias Pika.AgentBackend.{Event, JSONLWriter}

  @ref_pattern ~r/\A[0-9a-f]{64}\z/

  def capture(%Event{} = event, artifact_dir, cwd) do
    with {:ok, record} <- normalize(event, cwd),
         ref when is_binary(ref) <- ref(event) do
      path = Path.join([Path.expand(artifact_dir), "commands", ref <> ".jsonl"])
      :ok = JSONLWriter.append(path, "command", record)

      if Process.whereis(Pika.PubSub),
        do:
          Phoenix.PubSub.broadcast(Pika.PubSub, topic(ref), {:command_console_event, ref, record})

      {:ok, ref}
    else
      _other -> :ignore
    end
  rescue
    error -> {:error, error}
  end

  def ref(%Event{} = event), do: ref(event.session_id, event.turn_id, command_id(event))

  def ref(session_id, turn_id, command_id) when not is_nil(command_id) do
    Jason.encode!([session_id, turn_id, command_id])
    |> then(&:crypto.hash(:sha256, &1))
    |> Base.encode16(case: :lower)
  end

  def ref(_session_id, _turn_id, nil), do: nil
  def command?(%Event{} = event), do: command_id(event) != nil and command_event?(event)
  def topic(ref), do: "command_console:" <> ref

  def load(ref, opts \\ []) when is_binary(ref) do
    if Regex.match?(@ref_pattern, ref) do
      roots = Keyword.get(opts, :roots, artifact_roots())

      roots
      |> Enum.flat_map(
        &Path.wildcard(Path.join([Path.expand(&1), "**", "commands", ref <> ".jsonl"]))
      )
      |> Enum.uniq()
      |> Enum.sort()
      |> Enum.flat_map(&JSONLWriter.replay/1)
      |> Enum.reduce(empty(ref), fn record, console ->
        apply_record(console, record["payload"] || record[:payload] || %{})
      end)
      |> then(&{:ok, &1})
    else
      {:error, :invalid_ref}
    end
  end

  def empty(ref),
    do: %{
      ref: ref,
      command: nil,
      cwd: nil,
      backend: nil,
      status: "running",
      exit_code: nil,
      duration_ms: nil,
      raw_output: "",
      output: ""
    }

  def apply_record(console, record) do
    record = stringify_keys(record)

    console =
      console
      |> put_present(:command, record["command"])
      |> put_present(:cwd, record["cwd"])
      |> put_present(:backend, record["backend"])
      |> put_present(:status, record["status"])
      |> put_present(:exit_code, record["exit_code"])
      |> put_present(:duration_ms, record["duration_ms"])

    case {record["kind"], record["mode"], record["output"]} do
      {"output", "replace", output} when is_binary(output) ->
        set_output(console, output)

      {"output", _, output} when is_binary(output) ->
        set_output(console, console.raw_output <> output)

      {"completed", _, output} when is_binary(output) and console.output == "" ->
        set_output(console, output)

      _other ->
        console
    end
  end

  defp normalize(%Event{type: :command_output} = event, cwd) do
    data = stringify_keys(event.data)

    {:ok,
     base_record(event, cwd)
     |> Map.merge(%{
       "kind" => "output",
       "mode" => data["update_mode"] || "append",
       "output" => first_binary(data, ~w(delta output text content)) || "",
       "status" => data["status"] || "running"
     })}
  end

  defp normalize(%Event{type: type} = event, cwd) when type in [:tool_started, :tool_completed] do
    data = stringify_keys(event.data)
    item = data["item"] || data

    if command_event?(event) do
      {:ok,
       base_record(event, cwd)
       |> Map.merge(%{
         "kind" => if(type == :tool_started, do: "started", else: "completed"),
         "command" =>
           get_in(item, ["rawInput", "command"]) || first_binary(item, ~w(command title name)),
         "cwd" => item["cwd"] || cwd,
         "status" =>
           item["status"] || if(type == :tool_started, do: "running", else: "completed"),
         "exit_code" => item["exitCode"] || item["exit_code"],
         "duration_ms" => item["durationMs"] || item["duration_ms"],
         "output" => item["aggregatedOutput"]
       })}
    else
      :ignore
    end
  end

  defp normalize(_event, _cwd), do: :ignore

  defp base_record(event, cwd) do
    %{
      "session_id" => event.session_id,
      "turn_id" => event.turn_id,
      "command_id" => command_id(event),
      "backend" => to_string(event.backend),
      "cwd" => cwd,
      "status" => "running"
    }
  end

  defp command_event?(%Event{type: :command_output}), do: true
  defp command_event?(%Event{data: %{item: %{"type" => "commandExecution"}}}), do: true

  defp command_event?(%Event{backend: :cursor_acp, type: :tool_started, data: data}) do
    data = stringify_keys(data)
    data["kind"] == "execute" or is_binary(get_in(data, ["rawInput", "command"]))
  end

  defp command_event?(_event), do: false

  defp command_id(%Event{data: data, backend: :cursor_acp}) do
    data = stringify_keys(data)
    data["command_id"] || data["toolCallId"] || data["id"] || data["terminalId"]
  end

  defp command_id(%Event{data: data}) do
    data = stringify_keys(data)
    item = data["item"] || %{}
    data["command_id"] || item["id"] || data["itemId"] || data["item_id"]
  end

  defp artifact_roots do
    case Pika.Optimization.Persistence.current() do
      %{workspace_canonical_path: workspace} -> [Path.join(workspace, "agent-sessions")]
      _other -> []
    end
  rescue
    _error -> []
  end

  defp first_binary(map, keys), do: Enum.find_value(keys, &if(is_binary(map[&1]), do: map[&1]))
  defp put_present(map, _key, nil), do: map
  defp put_present(map, key, value), do: Map.put(map, key, value)
  defp set_output(console, raw), do: %{console | raw_output: raw, output: terminal_text(raw)}

  defp terminal_text(output) do
    output
    |> String.replace(~r/\e\[[0-?]*[ -\/]*[@-~]/, "")
    |> String.replace("\r\n", "\n")
    |> String.split("\n", trim: false)
    |> Enum.map_join("\n", fn line -> line |> String.split("\r") |> List.last() end)
  end

  defp stringify_keys(map) when is_map(map),
    do: Map.new(map, fn {key, value} -> {to_string(key), stringify_keys(value)} end)

  defp stringify_keys(list) when is_list(list), do: Enum.map(list, &stringify_keys/1)
  defp stringify_keys(value), do: value
end
