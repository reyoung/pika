defmodule Pika.ReferenceCatalog do
  @moduledoc false

  @entries [
    {"cutlass", "https://github.com/NVIDIA/cutlass.git", "NVIDIA CUTLASS"},
    {"cutex", "https://github.com/deciding/cutex.git", "CUDA Template Extensions"},
    {"cuLA", "https://github.com/inclusionAI/cuLA.git", "CUDA Linear Algebra"},
    {"flash-attention", "https://github.com/Dao-AILab/flash-attention.git", "Flash Attention"},
    {"flashinfer", "https://github.com/flashinfer-ai/flashinfer.git",
     "LLM Serving Kernel Library"},
    {"FlyDSL", "https://github.com/ROCm/FlyDSL.git", "ROCm FlyDSL"},
    {"triton", "https://github.com/triton-lang/triton.git", "Triton"},
    {"DeepGEMM", "https://github.com/deepseek-ai/DeepGEMM.git", "DeepGEMM"},
    {"LeetCUDA", "https://github.com/xlite-dev/LeetCUDA.git", "CUDA Learning"},
    {"FlashMLA", "https://github.com/deepseek-ai/FlashMLA.git", "FlashMLA"},
    {"composable_kernel", "https://github.com/ROCm/composable_kernel.git", "Composable Kernel"},
    {"cute-gemm", "https://github.com/reed-lau/cute-gemm.git", "CuTe GEMM Examples"},
    {"hpc-ops", "https://github.com/Tencent/hpc-ops.git", "Tencent HPC Ops"},
    {"aiter", "https://github.com/ROCm/aiter.git", "ROCm AIter"},
    {"quack", "https://github.com/Dao-AILab/quack.git", "Dao-AILab Quack"},
    {"tilelang", "https://github.com/tile-ai/tilelang.git", "TileLang"}
  ]

  def entries do
    Enum.map(@entries, fn {id, url, description} ->
      %{
        id: id,
        url: url,
        description: description,
        selected: true,
        status: :unresolved,
        sha: nil,
        branch: nil
      }
    end)
  end

  def resolve_selected(entries) do
    results =
      Task.async_stream(
        entries,
        fn
          %{selected: false} = entry -> entry
          entry -> resolve(entry)
        end,
        ordered: true,
        max_concurrency: 8,
        timeout: 60_000,
        on_timeout: :kill_task
      )
      |> Enum.to_list()

    entries
    |> Enum.zip(results)
    |> Enum.map(fn
      {_original, {:ok, entry}} ->
        entry

      {original, {:exit, _reason}} ->
        %{original | status: :error, description: original.description <> " (resolution timeout)"}
    end)
    |> then(fn resolved ->
      if Enum.any?(resolved, &(&1.selected && &1.status == :error)) do
        {:error, resolved}
      else
        {:ok, resolved}
      end
    end)
  end

  def materialize_selected(setup_root, entries, opts \\ []) do
    on_progress = Keyword.get(opts, :on_progress, fn _progress -> :ok end)
    total = Enum.count(entries, & &1.selected)

    {resolved, _completed} =
      Enum.map_reduce(entries, 0, fn
        %{selected: false} = entry, completed ->
          {entry, completed}

        entry, completed ->
          notify_progress(on_progress, entry.id, completed, total, :materializing)
          materialized = materialize(setup_root, entry)
          completed = completed + 1
          notify_progress(on_progress, entry.id, completed, total, materialized.status)
          {materialized, completed}
      end)

    if Enum.any?(resolved, &(&1.selected && &1.status == :error)),
      do: {:error, resolved},
      else: {:ok, resolved}
  end

  defp notify_progress(callback, id, completed, total, status) do
    callback.(%{id: id, completed: completed, total: total, status: status})
  rescue
    _error -> :ok
  end

  defp materialize(setup_root, entry) do
    path = Path.join([setup_root, "ref", entry.id])

    result =
      if File.dir?(Path.join(path, ".git")) or File.regular?(Path.join(path, ".git")) do
        checkout_pinned(path, entry.sha)
      else
        with {:ok, _} <-
               Pika.Git.run(setup_root, [
                 "-c",
                 "protocol.file.allow=always",
                 "submodule",
                 "add",
                 "--depth",
                 "1",
                 "--force",
                 entry.url,
                 "ref/#{entry.id}"
               ]),
             :ok <- checkout_pinned(path, entry.sha) do
          :ok
        end
      end

    case result do
      :ok ->
        entry

      {:error, reason} ->
        %{entry | status: :error, description: entry.description <> " (#{inspect(reason)})"}
    end
  end

  defp checkout_pinned(path, sha) do
    with {:ok, actual} <- Pika.Git.run(path, ["rev-parse", "HEAD"]),
         :ok <- fetch_if_needed(path, actual, sha),
         {:ok, _} <- Pika.Git.run(path, ["checkout", "--detach", sha]),
         {:ok, ^sha} <- Pika.Git.run(path, ["rev-parse", "HEAD"]) do
      :ok
    else
      {:error, reason} -> {:error, reason}
      {:ok, actual} -> {:error, {:reference_sha_mismatch, actual, sha}}
    end
  end

  defp fetch_if_needed(_path, sha, sha), do: :ok

  defp fetch_if_needed(path, _actual, sha) do
    case Pika.Git.run(path, ["fetch", "--depth", "1", "origin", sha]) do
      {:ok, _} -> :ok
      {:error, reason} -> {:error, reason}
    end
  end

  defp resolve(entry) do
    case System.cmd("git", ["ls-remote", "--symref", entry.url, "HEAD"], stderr_to_stdout: true) do
      {output, 0} ->
        branch =
          case Regex.run(~r/ref: refs\/heads\/([^\s]+)\s+HEAD/, output) do
            [_, value] -> value
            _ -> nil
          end

        sha =
          output
          |> String.split("\n")
          |> Enum.find_value(fn line ->
            case String.split(line) do
              [value, "HEAD"] when byte_size(value) == 40 -> value
              _ -> nil
            end
          end)

        if sha,
          do: %{entry | status: :resolved, sha: sha, branch: branch},
          else: %{entry | status: :error}

      {_output, _status} ->
        %{entry | status: :error}
    end
  end
end
