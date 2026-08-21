defmodule Pika.Agent.Directory do
  @moduledoc "In-memory authority for active Actor, Work, Session, and MCP token bindings."

  use GenServer

  alias Pika.Agent.Role.Work

  def start_link(opts \\ []) do
    case Keyword.get(opts, :name, __MODULE__) do
      nil -> GenServer.start_link(__MODULE__, %{})
      name -> GenServer.start_link(__MODULE__, %{}, name: name)
    end
  end

  def issue(actor, %Work{} = work, role_id, server \\ __MODULE__) when is_pid(actor) do
    GenServer.call(server, {:issue, actor, work, role_id, nil})
  end

  def issue(actor, %Work{} = work, role_id, catalog, server)
      when is_pid(actor) and is_list(catalog),
      do: GenServer.call(server, {:issue, actor, work, role_id, catalog})

  def bind_session(token, session_id, server \\ __MODULE__),
    do: GenServer.call(server, {:bind_session, token, session_id})

  def lookup(token, server \\ __MODULE__)

  def lookup(token, server) when is_binary(token),
    do: GenServer.call(server, {:lookup, token})

  def lookup(_token, _server), do: {:error, :unauthorized}

  def authorize(token, server \\ __MODULE__) do
    case lookup(token, server) do
      {:ok, _binding} -> :ok
      {:error, _reason} = error -> error
    end
  catch
    :exit, _reason -> {:error, :unauthorized}
  end

  def lookup_work(%Work{} = work, server \\ __MODULE__),
    do: GenServer.call(server, {:lookup_work, work_key(work)})

  def revoke(token, server \\ __MODULE__) when is_binary(token),
    do: GenServer.call(server, {:revoke, token_hash(token)})

  def active_count(server \\ __MODULE__), do: GenServer.call(server, :active_count)
  def active(server \\ __MODULE__), do: GenServer.call(server, :active)

  @impl true
  def init(_opts), do: {:ok, %{tokens: %{}, works: %{}, monitors: %{}, actors: %{}}}

  @impl true
  def handle_call({:issue, actor, work, role_id, catalog}, _from, state) do
    key = work_key(work)
    state = discard_dead_owner(state, key)

    case Map.get(state.works, key) do
      nil ->
        token = :crypto.strong_rand_bytes(32) |> Base.url_encode64(padding: false)
        hash = token_hash(token)
        {state, monitor} = ensure_monitor(state, actor)

        binding = %{
          actor: actor,
          work: work,
          role_id: role_id,
          catalog: catalog,
          session_id: nil,
          token_hash: hash,
          issued_at: System.system_time(:microsecond)
        }

        state = %{
          state
          | tokens: Map.put(state.tokens, hash, binding),
            works: Map.put(state.works, key, actor),
            actors:
              Map.update(
                state.actors,
                actor,
                %{monitor: monitor, hashes: MapSet.new([hash])},
                fn entry ->
                  %{entry | hashes: MapSet.put(entry.hashes, hash)}
                end
              )
        }

        {:reply, {:ok, token}, state}

      ^actor ->
        {:reply, {:error, :actor_already_has_work_token}, state}

      _other ->
        {:reply, {:error, :work_already_active}, state}
    end
  end

  def handle_call({:bind_session, token, session_id}, _from, state) do
    hash = token_hash(token)

    case Map.fetch(state.tokens, hash) do
      {:ok, binding} ->
        next = put_in(state.tokens[hash], %{binding | session_id: session_id})
        {:reply, :ok, next}

      :error ->
        {:reply, {:error, :unauthorized}, state}
    end
  end

  def handle_call({:lookup, token}, _from, state) do
    hash = token_hash(token)

    case Map.get(state.tokens, hash) do
      %{actor: actor} = binding when is_pid(actor) ->
        if Process.alive?(actor),
          do: {:reply, {:ok, binding}, state},
          else: {:reply, {:error, :unauthorized}, remove_actor(state, actor)}

      _ ->
        {:reply, {:error, :unauthorized}, state}
    end
  end

  def handle_call({:lookup_work, key}, _from, state) do
    state = discard_dead_owner(state, key)

    case Map.get(state.works, key) do
      actor when is_pid(actor) -> {:reply, {:ok, actor}, state}
      nil -> {:reply, {:error, :not_found}, state}
    end
  end

  def handle_call({:revoke, hash}, _from, state) do
    {:reply, :ok, remove_hash(state, hash)}
  end

  def handle_call(:active_count, _from, state), do: {:reply, map_size(state.works), state}

  def handle_call(:active, _from, state) do
    state = discard_dead_owners(state)

    active =
      Enum.map(state.works, fn {{role_id, kind, id, campaign_id}, actor} ->
        {%Work{role_id: role_id, kind: kind, id: id, campaign_id: campaign_id}, actor}
      end)

    {:reply, active, state}
  end

  @impl true
  def handle_info({:DOWN, monitor, :process, actor, _reason}, state) do
    case Map.get(state.monitors, monitor) do
      ^actor -> {:noreply, remove_actor(state, actor)}
      _ -> {:noreply, state}
    end
  end

  defp ensure_monitor(state, actor) do
    case Map.get(state.actors, actor) do
      %{monitor: monitor} ->
        {state, monitor}

      nil ->
        monitor = Process.monitor(actor)
        {%{state | monitors: Map.put(state.monitors, monitor, actor)}, monitor}
    end
  end

  defp discard_dead_owner(state, key) do
    case Map.get(state.works, key) do
      actor when is_pid(actor) ->
        if(Process.alive?(actor), do: state, else: remove_actor(state, actor))

      _ ->
        state
    end
  end

  defp discard_dead_owners(state) do
    Enum.reduce(Map.values(state.works), state, fn actor, acc ->
      if Process.alive?(actor), do: acc, else: remove_actor(acc, actor)
    end)
  end

  defp remove_hash(state, hash) do
    case Map.pop(state.tokens, hash) do
      {nil, _tokens} ->
        state

      {%{actor: actor, work: work}, tokens} ->
        actors =
          case Map.get(state.actors, actor) do
            nil ->
              state.actors

            entry ->
              Map.put(state.actors, actor, %{entry | hashes: MapSet.delete(entry.hashes, hash)})
          end

        %{state | tokens: tokens, works: Map.delete(state.works, work_key(work)), actors: actors}
    end
  end

  defp remove_actor(state, actor) do
    case Map.pop(state.actors, actor) do
      {nil, _actors} ->
        state

      {%{monitor: monitor, hashes: hashes}, actors} ->
        Process.demonitor(monitor, [:flush])

        Enum.reduce(
          hashes,
          %{state | actors: actors, monitors: Map.delete(state.monitors, monitor)},
          fn hash, acc ->
            remove_hash(acc, hash)
          end
        )
    end
  end

  defp work_key(work), do: {work.role_id, work.kind, work.id, work.campaign_id}

  defp token_hash(token),
    do: :crypto.hash(:sha256, token) |> Base.encode16(case: :lower)
end
