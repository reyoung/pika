defmodule Pika.Preflight do
  @moduledoc false

  @timeout 5_000

  def run(backend) when is_map(backend), do: run([backend])

  def run(backends) when is_list(backends) do
    backend_types =
      backends
      |> Enum.map(fn
        backend when is_map(backend) -> backend["type"] || backend["backend"]
        backend -> backend
      end)
      |> MapSet.new()

    [
      probe("git", "Git", "git", ["--version"], true, :workspace),
      probe("python", "Python", "python3", ["--version"], false, :managed_repo_lock),
      probe(
        "gpu_driver",
        "GPU / Driver",
        "nvidia-smi",
        ["--query-gpu=name,driver_version", "--format=csv,noheader"],
        false,
        :gpu
      ),
      probe(
        "codex_app_server",
        "Codex App Server",
        "codex",
        ["app-server", "--help"],
        false,
        :codex
      ),
      probe("cursor_acp", "Cursor ACP", "cursor-agent", ["acp", "--help"], false, :cursor)
    ]
    |> Enum.map(fn check ->
      relevant =
        case check.id do
          "codex_app_server" -> MapSet.member?(backend_types, "codex_app_server")
          "cursor_acp" -> MapSet.member?(backend_types, "cursor_acp")
          _ -> true
        end

      Map.put(check, :relevant, relevant)
    end)
  end

  defp probe(id, label, executable, args, required, scope) do
    case System.find_executable(executable) do
      nil ->
        %{
          id: id,
          label: label,
          status: :missing,
          detail: "#{executable} not found",
          required: required,
          scope: scope
        }

      path ->
        task =
          Task.async(fn ->
            System.cmd(path, args, stderr_to_stdout: true, env: sanitized_env())
          end)

        case Task.yield(task, @timeout) || Task.shutdown(task, :brutal_kill) do
          {:ok, {output, 0}} ->
            %{
              id: id,
              label: label,
              status: :ok,
              detail: summarize(output),
              required: required,
              scope: scope
            }

          {:ok, {output, status}} ->
            %{
              id: id,
              label: label,
              status: :failed,
              detail: "exit #{status}: #{summarize(output)}",
              required: required,
              scope: scope
            }

          nil ->
            %{
              id: id,
              label: label,
              status: :failed,
              detail: "timed out after #{@timeout} ms",
              required: required,
              scope: scope
            }
        end
    end
  end

  defp sanitized_env do
    [
      {"NO_COLOR", "1"},
      {"TERM", "dumb"}
    ]
  end

  defp summarize(output) do
    output
    |> String.trim()
    |> String.split("\n")
    |> List.first()
    |> case do
      nil -> "available"
      "" -> "available"
      line -> String.slice(line, 0, 240)
    end
  end
end
