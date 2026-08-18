defmodule Pika.AgentBackend.Replay do
  @moduledoc "Rebuild a provider-neutral event cursor from a redacted raw JSONL transcript."

  alias Pika.AgentBackend.JSONLWriter

  def cursor(path, backend) do
    events =
      path
      |> JSONLWriter.replay()
      |> Enum.flat_map(&event_types(&1, backend))

    %{
      backend: backend,
      record_count: length(JSONLWriter.replay(path)),
      event_count: length(events),
      event_types: events,
      last_event_type: List.last(events)
    }
  end

  defp event_types(%{"direction" => "in", "payload" => payload}, :codex_app_server) do
    case payload["method"] do
      "thread/started" ->
        [:session_started]

      "turn/started" ->
        [:turn_started]

      "item/agentMessage/delta" ->
        [:message_delta]

      method when method in ["item/plan/delta", "turn/plan/updated"] ->
        [:plan_updated]

      "item/started" ->
        [item_event(payload, :started)]

      "item/completed" ->
        [item_event(payload, :completed)]

      method when method in ["item/commandExecution/outputDelta", "command/exec/outputDelta"] ->
        [:command_output]

      method
      when method in [
             "item/fileChange/outputDelta",
             "item/fileChange/patchUpdated",
             "turn/diff/updated"
           ] ->
        [:file_changed]

      "item/mcpToolCall/progress" ->
        [:tool_updated]

      "thread/tokenUsage/updated" ->
        [:usage_updated]

      "turn/completed" ->
        [:turn_completed]

      "error" ->
        [:backend_error]

      _ ->
        []
    end
  end

  defp event_types(
         %{"direction" => "in", "payload" => %{"method" => "session/update", "params" => params}},
         :cursor_acp
       ) do
    case get_in(params, ["update", "sessionUpdate"]) do
      "agent_message_chunk" -> [:message_delta]
      "plan" -> [:plan_updated]
      "tool_call" -> [:tool_started]
      "tool_call_update" -> [:tool_updated]
      "usage_update" -> [:usage_updated]
      _ -> []
    end
  end

  defp event_types(
         %{"direction" => "in", "payload" => %{"id" => _, "result" => %{"stopReason" => _}}},
         :cursor_acp
       ),
       do: [:turn_completed]

  defp event_types(_record, _backend), do: []

  defp item_event(payload, stage) do
    case get_in(payload, ["params", "item", "type"]) do
      "fileChange" -> :file_changed
      _ when stage == :started -> :tool_started
      _ -> :tool_completed
    end
  end
end
