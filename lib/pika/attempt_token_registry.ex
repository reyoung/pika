defmodule Pika.AttemptTokenRegistry do
  @moduledoc false

  @table __MODULE__

  def init_owner do
    case :ets.whereis(@table) do
      :undefined ->
        _table =
          :ets.new(@table, [
            :named_table,
            :set,
            :public,
            read_concurrency: true,
            write_concurrency: true
          ])

        :ok

      table ->
        case :ets.info(table, :owner) do
          owner when owner == self() ->
            :ets.delete_all_objects(table)
            :ok

          owner ->
            {:error, {:attempt_token_registry_owned, owner}}
        end
    end
  end

  def put(token, identity) when is_binary(token) and is_map(identity) do
    true = :ets.insert(@table, {token_hash(token), identity})
    :ok
  rescue
    ArgumentError -> {:error, :attempt_token_registry_not_started}
  end

  def lookup(token) when is_binary(token) do
    case safe_lookup(token_hash(token)) do
      [{_hash, identity}] -> {:ok, identity}
      [] -> {:error, :unauthorized}
    end
  end

  def lookup(_token), do: {:error, :unauthorized}

  def authorize(token) do
    case lookup(token) do
      {:ok, _identity} -> :ok
      {:error, _reason} = error -> error
    end
  end

  def delete(token) when is_binary(token), do: delete_hash(token_hash(token))
  def delete(_token), do: :ok

  def delete_hash(hash) when is_binary(hash) do
    :ets.delete(@table, hash)
    :ok
  rescue
    ArgumentError -> :ok
  end

  defp safe_lookup(hash) do
    :ets.lookup(@table, hash)
  rescue
    ArgumentError -> []
  end

  defp token_hash(token),
    do: :crypto.hash(:sha256, token) |> Base.encode16(case: :lower)
end
