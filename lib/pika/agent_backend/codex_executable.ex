defmodule Pika.AgentBackend.CodexExecutable do
  @moduledoc false

  def resolve(command) when is_binary(command), do: command

  def resolve(nil) do
    entrypoint = System.find_executable("codex") || raise "codex executable was not found"

    case native_binary(entrypoint) do
      {:ok, binary} -> binary
      :error -> entrypoint
    end
  end

  defp native_binary(entrypoint) do
    with {:ok, real_entrypoint} <- realpath(entrypoint),
         true <- Path.extname(real_entrypoint) == ".js" do
      package_root = real_entrypoint |> Path.dirname() |> Path.join("..") |> Path.expand()

      candidates =
        Path.wildcard(
          Path.join([
            package_root,
            "node_modules",
            "@openai",
            "codex-*",
            "vendor",
            "*",
            "bin",
            if(match?({:win32, _}, :os.type()), do: "codex.exe", else: "codex")
          ])
        )

      preferred_target = native_target()

      preferred =
        Enum.find(candidates, &(String.contains?(&1, preferred_target) and executable?(&1)))

      case preferred || Enum.find(candidates, &executable?/1) do
        nil -> :error
        binary -> {:ok, binary}
      end
    else
      _ -> :error
    end
  end

  defp realpath(path) do
    case :file.read_link_all(String.to_charlist(path)) do
      {:ok, resolved} ->
        resolved = List.to_string(resolved)

        absolute =
          if Path.type(resolved) == :absolute do
            resolved
          else
            path |> Path.dirname() |> Path.join(resolved) |> Path.expand()
          end

        {:ok, absolute}

      {:error, :einval} ->
        {:ok, path}

      _ ->
        :error
    end
  end

  defp executable?(path) do
    case File.stat(path) do
      {:ok, %{type: :regular, mode: mode}} -> Bitwise.band(mode, 0o111) != 0
      _ -> false
    end
  end

  defp native_target do
    architecture = :erlang.system_info(:system_architecture) |> List.to_string()

    cond do
      String.contains?(architecture, "aarch64-apple") -> "aarch64-apple-darwin"
      String.contains?(architecture, "x86_64-apple") -> "x86_64-apple-darwin"
      String.contains?(architecture, "aarch64") -> "aarch64-unknown-linux-musl"
      true -> "x86_64-unknown-linux-musl"
    end
  end
end
