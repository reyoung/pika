defmodule Pika.AgentBackend.Id do
  @moduledoc false

  def new(prefix) do
    suffix = :crypto.strong_rand_bytes(12) |> Base.url_encode64(padding: false)
    "#{prefix}_#{suffix}"
  end
end
