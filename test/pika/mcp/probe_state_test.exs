defmodule Pika.MCP.ProbeStateTest do
  use ExUnit.Case, async: false

  setup do
    Pika.MCP.ProbeState.reset()
    :ok
  end

  test "stores only a token hash and enforces nonce plus idempotency" do
    registration = Pika.MCP.ProbeState.register_session(%{backend: :test})
    refute registration.token == registration.token_hash

    assert {:ok, %{identity: %{backend: :test}, nonce: nonce}} =
             Pika.MCP.ProbeState.get_context(registration.token)

    assert {:ok, first} =
             Pika.MCP.ProbeState.complete(registration.token, "same", nonce, "summary")

    assert {:ok, ^first} =
             Pika.MCP.ProbeState.complete(registration.token, "same", nonce, "summary")

    assert {:error, :idempotency_conflict} =
             Pika.MCP.ProbeState.complete(registration.token, "same", nonce, "different")

    assert {:error, :invalid_nonce} =
             Pika.MCP.ProbeState.complete(registration.token, "other", "wrong", "summary")

    assert {:error, :unauthorized} = Pika.MCP.ProbeState.get_context("not-a-token")
  end
end
