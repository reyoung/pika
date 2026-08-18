defmodule Pika.AgentBackend.JSONLWriter do
  @moduledoc "Append-only, redacted provider wire log."

  @secret_keys ~w(authorization token access_token bearer api_key)

  def append(path, direction, payload) do
    File.mkdir_p!(Path.dirname(path))

    record = %{
      at: DateTime.utc_now() |> DateTime.to_iso8601(),
      direction: direction,
      payload: redact(payload)
    }

    File.write!(path, Jason.encode!(record) <> "\n", [:append])
    :ok
  end

  def replay(path) do
    path
    |> File.stream!(:line, [])
    |> Enum.reduce_while([], fn line, acc ->
      case Jason.decode(line) do
        {:ok, record} -> {:cont, [record | acc]}
        {:error, _} -> {:halt, acc}
      end
    end)
    |> Enum.reverse()
  end

  def redact(%{"name" => name} = map) when is_binary(name) do
    if String.downcase(name) in ["authorization", "proxy-authorization"] do
      Map.put(map, "value", "[REDACTED]")
    else
      redact_map(map)
    end
  end

  def redact(map) when is_map(map), do: redact_map(map)
  def redact(list) when is_list(list), do: Enum.map(list, &redact/1)
  def redact(value), do: value

  defp redact_map(map) do
    Map.new(map, fn {key, value} ->
      normalized_key = key |> to_string() |> String.downcase()

      if normalized_key in @secret_keys or String.ends_with?(normalized_key, "_token") do
        {key, "[REDACTED]"}
      else
        {key, redact(value)}
      end
    end)
  end
end
