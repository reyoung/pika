defmodule Pika.Auth do
  @moduledoc false

  @key {__MODULE__, :token_hash}

  def generate do
    random_token() |> configure()
  end

  def random_token do
    :crypto.strong_rand_bytes(32) |> Base.url_encode64(padding: false)
  end

  def configure(token) when is_binary(token) and token != "" do
    marker = hash(token)
    :persistent_term.put(@key, marker)
    %{token: token, marker: marker}
  end

  def configure(nil), do: generate()

  def authenticate(token) when is_binary(token) do
    marker = hash(token)
    if authenticated_marker?(marker), do: {:ok, marker}, else: {:error, :unauthorized}
  end

  def authenticate(_token), do: {:error, :unauthorized}

  def authenticated_marker?(marker) when is_binary(marker) do
    case :persistent_term.get(@key, nil) do
      ^marker -> true
      _ -> false
    end
  end

  def authenticated_marker?(_marker), do: false

  def bearer_authenticated?(authorization) when is_binary(authorization) do
    case authorization do
      "Bearer " <> token when token != "" -> match?({:ok, _}, authenticate(token))
      _ -> false
    end
  end

  def bearer_authenticated?(_authorization), do: false

  def clear do
    :persistent_term.erase(@key)
    :ok
  end

  defp hash(token), do: :crypto.hash(:sha256, token) |> Base.encode16(case: :lower)
end
