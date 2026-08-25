defmodule Pika.Agent.ToolActivity do
  @moduledoc "Normalizes provider tool events for the durable conversation timeline."

  alias Pika.AgentBackend.Event
  alias Pika.CommandConsole

  @codex_tool_types ~w(commandExecution dynamicToolCall fileChange imageView mcpToolCall webSearch)

  @spec from_event(Event.t()) :: {:ok, map()} | :ignore
  def from_event(%Event{type: type} = event)
      when type in [:tool_started, :tool_updated, :tool_completed, :file_changed] do
    data = stringify_keys(event.data)
    item = data["item"] || data
    item_type = item["type"]

    cond do
      item_type == "mcpToolCall" and item["server"] == "pika" ->
        :ignore

      item_type in @codex_tool_types ->
        build(event, data, item, codex_kind(item_type))

      cursor_tool?(data) ->
        build(event, data, item, cursor_kind(item))

      type == :tool_updated and tool_id(item) ->
        build(event, data, item, "tool")

      true ->
        :ignore
    end
  end

  def from_event(_event), do: :ignore

  defp build(event, data, item, kind) do
    case tool_id(item) do
      nil ->
        :ignore

      id ->
        activity = %{
          "id" => id,
          "kind" => kind,
          "status" => tool_status(event.type, data, item),
          "updated_at" => DateTime.to_iso8601(event.at)
        }

        activity =
          activity
          |> put_present("name", tool_name(kind, item))
          |> put_present("summary", tool_summary(kind, item))
          |> put_present("command_ref", command_ref(event))

        {:ok, activity}
    end
  end

  defp command_ref(event) do
    if CommandConsole.command?(event), do: CommandConsole.ref(event)
  end

  defp tool_id(item),
    do:
      item["id"] || item["itemId"] || item["item_id"] || item["toolCallId"] ||
        item["command_id"] || item["terminalId"]

  defp tool_status(event_type, data, item) do
    status = item["status"] || data["status"]
    stage = data["stage"]

    case normalize(status) do
      value when value in ["completed", "failed", "cancelled"] -> value
      "in_progress" -> "running"
      _ when event_type == :tool_completed -> "completed"
      _ when stage in ["completed", :completed] -> "completed"
      _ -> "running"
    end
  end

  defp tool_name("command", item), do: command_name(item["command"])

  defp tool_name("mcp", item) do
    [item["server"], item["tool"] || item["name"]]
    |> Enum.reject(&is_nil/1)
    |> Enum.join(" · ")
    |> empty_to_nil()
  end

  defp tool_name("file_change", _item), do: "Changed files"
  defp tool_name("web_search", _item), do: "Searched the web"
  defp tool_name("image_view", _item), do: "Viewed an image"

  defp tool_name(_kind, item),
    do: item["title"] || item["name"] || item["kind"] || "Used a tool"

  defp tool_summary("command", item), do: compact(item["command"])

  defp tool_summary("mcp", item) do
    error = item["error"]

    cond do
      is_binary(error) -> compact(error)
      is_map(error) -> compact(error["message"] || inspect(error))
      true -> nil
    end
  end

  defp tool_summary("file_change", item) do
    item["changes"]
    |> List.wrap()
    |> Enum.map(&(&1["path"] || &1["file"] || &1["name"]))
    |> Enum.reject(&is_nil/1)
    |> Enum.join(", ")
    |> compact()
  end

  defp tool_summary("web_search", item), do: compact(item["query"])

  defp tool_summary(_kind, item) do
    compact(item["title"] || item["description"] || cursor_input(item["rawInput"]))
  end

  defp command_name(command) when is_binary(command) do
    command = String.trim(command)

    cond do
      Regex.match?(~r/\b(gpu-lease|nvidia-smi)\b/, command) -> "Ran a GPU command"
      Regex.match?(~r/^\s*(cat|head|less|sed|tail)\b/, command) -> "Read files"
      Regex.match?(~r/^\s*(grep|rg)\b/, command) -> "Searched files"
      Regex.match?(~r/^\s*git\b/, command) -> "Ran Git"
      true -> "Ran a command"
    end
  end

  defp command_name(_command), do: "Ran a command"

  defp cursor_input(nil), do: nil
  defp cursor_input(value) when is_binary(value), do: value
  defp cursor_input(value), do: Jason.encode!(value)

  defp cursor_tool?(data),
    do: data["sessionUpdate"] in ["tool_call", "tool_call_update"]

  defp cursor_kind(item) do
    case normalize(item["kind"]) do
      value when value in ["execute", "terminal"] -> "command"
      value when value in ["edit", "delete", "move"] -> "file_change"
      value when value in ["search", "web_search"] -> "web_search"
      _ -> "tool"
    end
  end

  defp codex_kind("commandExecution"), do: "command"
  defp codex_kind("fileChange"), do: "file_change"
  defp codex_kind("mcpToolCall"), do: "mcp"
  defp codex_kind("webSearch"), do: "web_search"
  defp codex_kind("imageView"), do: "image_view"
  defp codex_kind(_type), do: "tool"

  defp compact(nil), do: nil

  defp compact(value) when is_binary(value) do
    value
    |> String.replace(~r/\s+/, " ")
    |> String.trim()
    |> String.slice(0, 180)
    |> empty_to_nil()
  end

  defp compact(value), do: value |> inspect(limit: 8) |> compact()

  defp empty_to_nil(""), do: nil
  defp empty_to_nil(value), do: value

  defp normalize(nil), do: nil

  defp normalize(value) do
    value
    |> to_string()
    |> Macro.underscore()
  end

  defp put_present(map, _key, nil), do: map
  defp put_present(map, key, value), do: Map.put(map, key, value)

  defp stringify_keys(map) when is_map(map),
    do: Map.new(map, fn {key, value} -> {to_string(key), stringify_keys(value)} end)

  defp stringify_keys(list) when is_list(list), do: Enum.map(list, &stringify_keys/1)
  defp stringify_keys(value), do: value
end
