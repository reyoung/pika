defmodule Pika.AgentBackend.JSONLWriter do
  @moduledoc "Append-only, redacted provider wire log."

  @secret_keys ~w(authorization token access_token bearer api_key password passwd secret client_secret private_key)
  @secret_key_suffixes ~w(_token _password _passwd _secret _api_key _access_key)

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
  def redact(value) when is_binary(value), do: redact_string(value)
  def redact(value), do: value

  def scrub_file(path) do
    records = replay(path)
    temporary = path <> ".scrubbed"

    File.open!(temporary, [:write, :binary], fn io ->
      Enum.each(records, fn record -> IO.binwrite(io, Jason.encode!(redact(record)) <> "\n") end)
    end)

    File.rename!(temporary, path)
    :ok
  end

  defp redact_map(map) do
    Map.new(map, fn {key, value} ->
      normalized_key = key |> to_string() |> String.downcase()

      if normalized_key in @secret_keys or
           Enum.any?(@secret_key_suffixes, &String.ends_with?(normalized_key, &1)) do
        {key, "[REDACTED]"}
      else
        {key, redact(value)}
      end
    end)
  end

  defp redact_string(value) do
    value
    |> redact_replace(~r{\b([a-z][a-z0-9+.-]*://)[^\s/@:]+:[^\s/@]+@}i, "\\1[REDACTED]@")
    |> redact_replace(~r/\b(Bearer\s+)[A-Za-z0-9._~+\/-]+=*/i, "\\1[REDACTED]")
    |> redact_replace(
      ~r/(["']?(?:authorization|bearer|token|password|passwd|secret|api_key|access_key|client_secret|private_key|[A-Za-z][A-Za-z0-9_]*(?:_token|_password|_passwd|_secret|_api_key|_access_key))["']?\s*(?:=>|:|=)\s*)(?:"[^"]*"|'[^']*'|[^\s,}\]]+)/i,
      "\\1[REDACTED]"
    )
    |> redact_replace(
      ~r/\b([A-Z][A-Z0-9_]*(?:TOKEN|PASSWORD|PASSWD|SECRET|API_KEY|ACCESS_KEY))\s*=\s*([^\s"']+)/i,
      "\\1=[REDACTED]"
    )
    |> redact_replace(
      ~r/-----BEGIN [^-]*PRIVATE KEY-----.*?-----END [^-]*PRIVATE KEY-----/s,
      "[REDACTED PRIVATE KEY]"
    )
  end

  defp redact_replace(value, regex, replacement), do: Regex.replace(regex, value, replacement)
end
