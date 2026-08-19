defmodule Pika.Stage0.Auth do
  @moduledoc false

  @key {__MODULE__, :token_hash}

  def generate do
    token = :crypto.strong_rand_bytes(32) |> Base.url_encode64(padding: false)
    marker = hash(token)
    :persistent_term.put(@key, marker)
    %{token: token, marker: marker}
  end

  def authenticate(token) when is_binary(token) do
    marker = hash(token)
    if authenticated_marker?(marker), do: {:ok, marker}, else: {:error, :unauthorized}
  end

  def authenticate(_), do: {:error, :unauthorized}

  def authenticated_marker?(marker) when is_binary(marker) do
    case :persistent_term.get(@key, nil) do
      ^marker -> true
      _ -> false
    end
  end

  def authenticated_marker?(_), do: false

  def clear do
    :persistent_term.erase(@key)
    :ok
  end

  defp hash(token), do: :crypto.hash(:sha256, token) |> Base.encode16(case: :lower)
end
