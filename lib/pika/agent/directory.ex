defmodule Pika.Agent.Directory do
  @moduledoc "In-memory authority for v2 Actor Work and MCP Session token bindings."

  use GenServer

  alias Pika.Agent.{ToolCatalog, SessionBinding}

  def start_link(opts \\ []) do
    case Keyword.get(opts, :name, __MODULE__) do
      nil -> GenServer.start_link(__MODULE__, %{})
      name -> GenServer.start_link(__MODULE__, %{}, name: name)
    end
  end

  @spec issue(SessionBinding.t(), GenServer.server()) :: {:ok, String.t()} | {:error, term()}
  def issue(%SessionBinding{} = binding, server \\ __MODULE__),
    do: GenServer.call(server, {:issue, binding})

  def lookup(token, server \\ __MODULE__)

  def lookup(token, server) when is_binary(token),
    do: GenServer.call(server, {:lookup, token_hash(token)})

  def lookup(_token, _server), do: {:error, :unauthorized}

  def authorize(token, server \\ __MODULE__) do
    case lookup(token, server) do
      {:ok, _binding} -> :ok
      {:error, _reason} = error -> error
    end
  catch
    :exit, _reason -> {:error, :unauthorized}
  end

  def lookup_work(role_id, work_kind, work_id, server \\ __MODULE__) do
    GenServer.call(server, {:lookup_work, {role_id, to_string(work_kind), work_id}})
  end

  def revoke(token, server \\ __MODULE__) when is_binary(token),
    do: GenServer.call(server, {:revoke, token_hash(token)})

  def active(server \\ __MODULE__), do: GenServer.call(server, :active)

  @impl true
  def init(_state), do: {:ok, %{tokens: %{}, works: %{}, monitors: %{}}}

  @impl true
  def handle_call({:issue, binding}, _from, state) do
    key = work_key(binding)
    state = discard_dead_owner(state, key)

    case state.works[key] do
      nil ->
        {:ok, catalog} = ToolCatalog.for_role(binding.role_id)
        token = :crypto.strong_rand_bytes(32) |> Base.url_encode64(padding: false)
        hash = token_hash(token)
        {state, monitor} = ensure_monitor(state, binding.actor)
        binding = Map.put(binding, :catalog, catalog)

        state = %{
          state
          | tokens: Map.put(state.tokens, hash, binding),
            works: Map.put(state.works, key, %{actor: binding.actor, token_hash: hash}),
            monitors: Map.put(state.monitors, monitor, binding.actor)
        }

        {:reply, {:ok, token}, state}

      %{actor: actor} when actor == binding.actor ->
        {:reply, {:error, :actor_already_has_work_token}, state}

      _other ->
        {:reply, {:error, :work_already_active}, state}
    end
  end

  def handle_call({:lookup, hash}, _from, state) do
    case state.tokens[hash] do
      %{actor: actor} = binding when is_pid(actor) ->
        if Process.alive?(actor),
          do: {:reply, {:ok, binding}, state},
          else: {:reply, {:error, :unauthorized}, remove_actor(state, actor)}

      _other ->
        {:reply, {:error, :unauthorized}, state}
    end
  end

  def handle_call({:lookup_work, key}, _from, state) do
    state = discard_dead_owner(state, key)

    case state.works[key] do
      %{actor: actor} -> {:reply, {:ok, actor}, state}
      nil -> {:reply, {:error, :not_found}, state}
    end
  end

  def handle_call({:revoke, hash}, _from, state),
    do: {:reply, :ok, remove_hash(state, hash)}

  def handle_call(:active, _from, state) do
    state = discard_dead_owners(state)

    values =
      Enum.map(state.tokens, fn {_hash, binding} ->
        {{binding.role_id, binding.work_kind, binding.work_id}, binding.actor}
      end)

    {:reply, values, state}
  end

  @impl true
  def handle_info({:DOWN, monitor, :process, actor, _reason}, state) do
    if state.monitors[monitor] == actor,
      do: {:noreply, remove_actor(state, actor)},
      else: {:noreply, state}
  end

  defp ensure_monitor(state, actor) do
    case Enum.find(state.monitors, fn {_monitor, owner} -> owner == actor end) do
      {monitor, ^actor} -> {state, monitor}
      nil -> {state, Process.monitor(actor)}
    end
  end

  defp discard_dead_owner(state, key) do
    case state.works[key] do
      %{actor: actor} -> if(Process.alive?(actor), do: state, else: remove_actor(state, actor))
      nil -> state
    end
  end

  defp discard_dead_owners(state) do
    state.works
    |> Map.values()
    |> Enum.map(& &1.actor)
    |> Enum.uniq()
    |> Enum.reduce(state, fn actor, acc ->
      if Process.alive?(actor), do: acc, else: remove_actor(acc, actor)
    end)
  end

  defp remove_actor(state, actor) do
    hashes = for {hash, %{actor: ^actor}} <- state.tokens, do: hash
    Enum.reduce(hashes, state, &remove_hash(&2, &1))
  end

  defp remove_hash(state, hash) do
    case Map.pop(state.tokens, hash) do
      {nil, _tokens} ->
        state

      {%{actor: actor} = binding, tokens} ->
        key = work_key(binding)
        works = Map.delete(state.works, key)

        other_binding? = Enum.any?(tokens, fn {_hash, value} -> value.actor == actor end)

        monitors =
          if other_binding? do
            state.monitors
          else
            Enum.reduce(state.monitors, state.monitors, fn {monitor, owner}, acc ->
              if owner == actor do
                Process.demonitor(monitor, [:flush])
                Map.delete(acc, monitor)
              else
                acc
              end
            end)
          end

        %{state | tokens: tokens, works: works, monitors: monitors}
    end
  end

  defp work_key(binding),
    do: {binding.role_id, to_string(binding.work_kind), binding.work_id}

  defp token_hash(token),
    do: :crypto.hash(:sha256, token) |> Base.encode16(case: :lower)
end
