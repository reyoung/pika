defmodule Pika.Agent.Completion do
  @moduledoc "Evaluates the deliberately limited, read-only Agent Role completion graph."

  alias Pika.Agent.Role.Progress

  @outcomes [:completed, :accepted, :rejected, :failed, :blocked]

  def validate(%{terminals: terminals, suggestions: suggestions}, tool_names)
      when is_list(terminals) and is_list(suggestions) do
    with :ok <- validate_terminals(terminals),
         :ok <- validate_suggestions(suggestions, MapSet.new(tool_names)) do
      :ok
    end
  end

  def validate(_graph, _tool_names), do: {:error, :invalid_completion_graph}

  def evaluate(%{terminals: terminals, suggestions: suggestions}, facts) when is_map(facts) do
    state = terminal_state(terminals, facts)

    required =
      case state do
        :open ->
          suggestions
          |> Enum.filter(fn {_operation, expression} -> truthy?(expression, facts) end)
          |> Enum.map(&elem(&1, 0))

        {:terminal, _outcome} ->
          []
      end

    %Progress{
      state: state,
      required_operations: required,
      facts_revision: revision(facts)
    }
  end

  def truthy?(true, _facts), do: true
  def truthy?(false, _facts), do: false
  def truthy?({:fact, path}, facts), do: value(facts, path) not in [nil, false]
  def truthy?({:eq, path, expected}, facts), do: value(facts, path) == expected
  def truthy?({:not, expression}, facts), do: not truthy?(expression, facts)
  def truthy?({:all, expressions}, facts), do: Enum.all?(expressions, &truthy?(&1, facts))
  def truthy?({:any, expressions}, facts), do: Enum.any?(expressions, &truthy?(&1, facts))

  defp terminal_state(terminals, facts) do
    Enum.find_value(terminals, :open, fn {outcome, expression} ->
      if truthy?(expression, facts), do: {:terminal, outcome}
    end)
  end

  defp validate_terminals([]), do: {:error, :completion_has_no_terminal_path}

  defp validate_terminals(terminals) do
    if Enum.all?(terminals, fn
         {outcome, expression} when outcome in @outcomes -> valid_expression?(expression)
         _ -> false
       end),
      do: :ok,
      else: {:error, :invalid_completion_terminal}
  end

  defp validate_suggestions(suggestions, tools) do
    if Enum.all?(suggestions, fn
         {operation, expression} when is_binary(operation) ->
           MapSet.member?(tools, operation) and valid_expression?(expression)

         _ ->
           false
       end),
      do: :ok,
      else: {:error, :invalid_completion_suggestion}
  end

  defp valid_expression?(value) when is_boolean(value), do: true
  defp valid_expression?({:fact, path}), do: valid_path?(path)
  defp valid_expression?({:eq, path, _value}), do: valid_path?(path)
  defp valid_expression?({:not, expression}), do: valid_expression?(expression)

  defp valid_expression?({operator, expressions}) when operator in [:all, :any] and is_list(expressions),
    do: expressions != [] and Enum.all?(expressions, &valid_expression?/1)

  defp valid_expression?(_expression), do: false

  defp valid_path?(path) when is_atom(path) or is_binary(path), do: true
  defp valid_path?(path) when is_list(path), do: path != []
  defp valid_path?(_path), do: false

  defp value(facts, path) when is_list(path), do: get_in_flexible(facts, path)
  defp value(facts, key), do: fetch_flexible(facts, key)

  defp get_in_flexible(value, []), do: value

  defp get_in_flexible(value, [key | rest]) when is_map(value),
    do: get_in_flexible(fetch_flexible(value, key), rest)

  defp get_in_flexible(_value, _path), do: nil

  defp fetch_flexible(map, key) do
    case Map.fetch(map, key) do
      {:ok, value} -> value
      :error when is_atom(key) -> Map.get(map, Atom.to_string(key))
      :error -> Map.get(map, key)
    end
  end

  defp revision(facts) do
    case fetch_flexible(facts, :_revision) do
      value when is_integer(value) and value >= 0 -> value
      _ -> 0
    end
  end
end
