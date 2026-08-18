defmodule Pika.Phase0.Skill do
  @moduledoc false

  @url "https://github.com/mit-han-lab/ncu-report-skill.git"

  def ensure_latest(path) do
    path = Path.expand(path)
    File.mkdir_p!(Path.dirname(path))
    sha = remote_head!()

    if File.dir?(Path.join(path, ".git")) do
      current = git!(path, ["rev-parse", "HEAD"])

      if current != sha do
        _ = git!(path, ["fetch", "--depth", "1", "origin", sha])
        _ = git!(path, ["checkout", "--detach", "FETCH_HEAD"])
      end
    else
      {output, 0} =
        System.cmd("git", ["clone", "--depth", "1", @url, path], stderr_to_stdout: true)

      _ = output
    end

    actual = git!(path, ["rev-parse", "HEAD"])

    if actual != sha do
      raise "ncu-report-skill checkout mismatch: expected #{sha}, got #{actual}"
    end

    %{name: "ncu-report-skill", url: @url, path: path, sha: sha}
  end

  defp remote_head! do
    case System.cmd("git", ["ls-remote", @url, "HEAD"], stderr_to_stdout: true) do
      {output, 0} -> output |> String.split() |> hd()
      {output, status} -> raise "git ls-remote failed (#{status}): #{output}"
    end
  end

  defp git!(cwd, args) do
    case System.cmd("git", args, cd: cwd, stderr_to_stdout: true) do
      {output, 0} -> String.trim(output)
      {output, status} -> raise "git #{Enum.join(args, " ")} failed (#{status}): #{output}"
    end
  end
end
