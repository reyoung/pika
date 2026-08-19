defmodule Pika.MCP.ProbeState do
  @moduledoc "In-memory backend-conformance MCP state. Raw bearer tokens are never retained."

  use GenServer

  alias Pika.AgentBackend.Id

  def start_link(opts \\ []),
    do: GenServer.start_link(__MODULE__, %{}, Keyword.put_new(opts, :name, __MODULE__))

  def register_session(identity, server \\ __MODULE__),
    do: GenServer.call(server, {:register, identity})

  def get_context(token, server \\ __MODULE__), do: GenServer.call(server, {:context, token})

  def complete(token, idempotency_key, nonce, summary, server \\ __MODULE__) do
    GenServer.call(server, {:complete, token, idempotency_key, nonce, summary})
  end

  def completions(server \\ __MODULE__), do: GenServer.call(server, :completions)
  def reset(server \\ __MODULE__), do: GenServer.call(server, :reset)

  @impl true
  def init(_), do: {:ok, %{sessions: %{}, completions: %{}}}

  @impl true
  def handle_call({:register, identity}, _from, state) do
    token = :crypto.strong_rand_bytes(32) |> Base.url_encode64(padding: false)
    token_hash = hash(token)
    nonce = Id.new("nonce")
    session = %{identity: identity, nonce: nonce, token_hash: token_hash}

    reply = %{token: token, nonce: nonce, token_hash: Base.encode16(token_hash, case: :lower)}
    {:reply, reply, put_in(state, [:sessions, token_hash], session)}
  end

  def handle_call({:context, token}, _from, state) do
    case Map.fetch(state.sessions, hash(token)) do
      {:ok, session} -> {:reply, {:ok, Map.take(session, [:identity, :nonce])}, state}
      :error -> {:reply, {:error, :unauthorized}, state}
    end
  end

  def handle_call({:complete, token, key, nonce, summary}, _from, state) do
    token_hash = hash(token)

    with {:ok, session} <- Map.fetch(state.sessions, token_hash),
         true <- nonce == session.nonce do
      request = %{nonce: nonce, summary: summary}
      request_hash = :crypto.hash(:sha256, :erlang.term_to_binary(request))
      completion_key = {token_hash, key}

      case Map.get(state.completions, completion_key) do
        nil ->
          response = %{
            completion_id: Id.new("completion"),
            session_identity: session.identity,
            summary: summary,
            idempotency_key: key
          }

          entry = %{request_hash: request_hash, response: response}
          {:reply, {:ok, response}, put_in(state, [:completions, completion_key], entry)}

        %{request_hash: ^request_hash, response: response} ->
          {:reply, {:ok, response}, state}

        _other ->
          {:reply, {:error, :idempotency_conflict}, state}
      end
    else
      :error -> {:reply, {:error, :unauthorized}, state}
      false -> {:reply, {:error, :invalid_nonce}, state}
    end
  end

  def handle_call(:completions, _from, state) do
    completions = Enum.map(state.completions, fn {_key, entry} -> entry.response end)
    {:reply, completions, state}
  end

  def handle_call(:reset, _from, _state), do: {:reply, :ok, %{sessions: %{}, completions: %{}}}

  defp hash(token) when is_binary(token), do: :crypto.hash(:sha256, token)
end
