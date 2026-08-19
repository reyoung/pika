defmodule Pika.FileSystem do
  @moduledoc false

  def atomic_write(path, contents) when is_binary(path) and is_binary(contents) do
    directory = Path.dirname(path)
    temporary = Path.join(directory, ".#{Path.basename(path)}.tmp-#{random_suffix()}")

    with :ok <- File.mkdir_p(directory),
         {:ok, io} <- File.open(temporary, [:write, :binary, :exclusive]),
         :ok <- write_sync_close(io, contents),
         :ok <- File.rename(temporary, path) do
      sync_directory(directory)
      :ok
    else
      {:error, reason} = error ->
        _ = File.rm(temporary)
        if reason == :eexist, do: atomic_write(path, contents), else: error
    end
  end

  defp write_sync_close(io, contents) do
    result =
      with :ok <- IO.binwrite(io, contents),
           :ok <- :file.sync(io) do
        :ok
      end

    _ = File.close(io)
    result
  end

  defp sync_directory(path) do
    with {:ok, io} <- :file.open(String.to_charlist(path), [:read, :raw]),
         :ok <- :file.sync(io) do
      :file.close(io)
    else
      _ -> :ok
    end
  end

  defp random_suffix, do: :crypto.strong_rand_bytes(8) |> Base.url_encode64(padding: false)
end
