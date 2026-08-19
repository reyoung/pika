defmodule Pika.Stage0.Git do
  @moduledoc false

  def run(cwd, args) do
    case System.cmd("git", args, cd: cwd, stderr_to_stdout: true) do
      {output, 0} -> {:ok, String.trim(output)}
      {output, status} -> {:error, %{status: status, output: String.trim(output), args: args}}
    end
  end

  def run!(cwd, args) do
    case run(cwd, args) do
      {:ok, output} -> output
      {:error, error} -> raise "git #{Enum.join(args, " ")} failed: #{inspect(error)}"
    end
  end

  def clean?(repo) do
    case run(repo, ["status", "--porcelain=v1", "--untracked-files=normal"]) do
      {:ok, ""} -> true
      _ -> false
    end
  end

  def head(repo), do: run(repo, ["rev-parse", "HEAD"])
end
