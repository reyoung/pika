defmodule Pika.ReferenceCatalog do
  @moduledoc false

  @max_id_bytes 64
  @max_url_bytes 2_048
  @max_description_bytes 240
  @supported_schemes ~w(http https ssh git file)

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
        origin: :builtin,
        selected: true,
        status: :unresolved,
        sha: nil,
        branch: nil
      }
    end)
  end

  def new_user_entry(attrs, existing_entries \\ [])

  def new_user_entry(attrs, existing_entries) when is_map(attrs) do
    url = attrs |> value(:url) |> normalize_text()
    requested_id = attrs |> value(:id) |> normalize_text()
    description = attrs |> value(:description) |> normalize_text()

    with :ok <- validate_url(url),
         {:ok, id} <- normalize_id(requested_id, url),
         :ok <- validate_id(id),
         :ok <- validate_description(description),
         :ok <- ensure_unique(id, url, existing_entries) do
      {:ok,
       %{
         id: id,
         url: url,
         description:
           if(description == "", do: "User-provided Git repository", else: description),
         origin: :user,
         selected: true,
         status: :unresolved,
         sha: nil,
         branch: nil
       }}
    end
  end

  def new_user_entry(_attrs, _existing_entries),
    do: {:error, {:invalid_reference_project, :repository, :invalid_format}}

  def frozen?(%{status: :resolved, sha: sha}) when is_binary(sha),
    do: Regex.match?(~r/\A[0-9a-f]{40}\z/i, sha)

  def frozen?(_entry), do: false

  def resolve_selected(entries) do
    results =
      Task.async_stream(
        entries,
        fn
          %{selected: false} = entry -> entry
          entry when is_map(entry) -> if(frozen?(entry), do: entry, else: resolve(entry))
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

  def materialize_selected(workspace_root, entries, opts \\ []) do
    on_progress = Keyword.get(opts, :on_progress, fn _progress -> :ok end)
    total = Enum.count(entries, & &1.selected)
    max_concurrency = materialization_concurrency(opts, total)
    completed = :atomics.new(1, [])

    checkout_root = Path.join(workspace_root, "refs")

    case ensure_real_directory(checkout_root) do
      :ok -> :ok
      {:error, reason} -> raise "Reference checkout root is unavailable: #{inspect(reason)}"
    end

    results =
      Task.async_stream(
        entries,
        fn
          %{selected: false} = entry ->
            entry

          entry ->
            notify_progress(
              on_progress,
              entry.id,
              :atomics.get(completed, 1),
              total,
              :materializing
            )

            safely_prepare_materialization(workspace_root, entry)
        end,
        ordered: true,
        max_concurrency: max_concurrency,
        timeout: Keyword.get(opts, :timeout, :infinity),
        on_timeout: :kill_task
      )

    resolved =
      entries
      |> Stream.zip(results)
      |> Enum.map(fn
        {_original, {:ok, %{selected: false} = entry}} ->
          entry

        {_original, {:ok, entry}} ->
          count = :atomics.add_get(completed, 1, 1)
          notify_progress(on_progress, entry.id, count, total, entry.status)
          entry

        {original, {:exit, reason}} ->
          materialized = materialization_error(original, {:task_exit, reason})

          if original.selected do
            count = :atomics.add_get(completed, 1, 1)
            notify_progress(on_progress, original.id, count, total, materialized.status)
          end

          materialized
      end)

    cond do
      Enum.any?(resolved, &(&1.selected && &1.status == :error)) ->
        {:error, resolved}

      link_into = Keyword.get(opts, :link_into) ->
        case link_selected(workspace_root, link_into, resolved) do
          :ok -> {:ok, resolved}
          {:error, reason} -> {:error, mark_selected_error(resolved, reason)}
        end

      true ->
        {:ok, resolved}
    end
  rescue
    error -> {:error, mark_selected_error(entries, {:exception, Exception.message(error)})}
  end

  def link_selected(workspace_root, worktree, entries) do
    refs_root = workspace_root |> Path.join("refs") |> Path.expand()
    selected = Enum.filter(entries, & &1.selected)

    with :ok <- ensure_reference_exclude(worktree),
         :ok <- ensure_real_directory(Path.join(worktree, "ref")),
         :ok <- remove_stale_links(refs_root, worktree, selected) do
      Enum.reduce_while(selected, :ok, fn entry, :ok ->
        case ensure_reference_link(refs_root, worktree, entry) do
          :ok -> {:cont, :ok}
          {:error, reason} -> {:halt, {:error, reason}}
        end
      end)
    end
  end

  defp notify_progress(callback, id, completed, total, status) do
    callback.(%{id: id, completed: completed, total: total, status: status})
  rescue
    _error -> :ok
  end

  defp safely_prepare_materialization(workspace_root, entry) do
    prepare_materialization(workspace_root, entry)
  rescue
    error -> materialization_error(entry, {:exception, Exception.message(error)})
  catch
    kind, reason -> materialization_error(entry, {kind, reason})
  end

  defp prepare_materialization(workspace_root, entry) do
    path = Path.join([workspace_root, "refs", entry.id])

    result =
      :global.trans({{__MODULE__, :reference_checkout, path}, self()}, fn ->
        cond do
          File.dir?(Path.join(path, ".git")) ->
            with :ok <- verify_origin(path, entry.url),
                 :ok <- checkout_pinned(path, entry.sha) do
              :ok
            end

          File.exists?(path) ->
            {:error, {:reference_checkout_conflict, entry.id, path}}

          true ->
            clone_pinned(workspace_root, path, entry)
        end
      end)

    case result do
      :ok -> entry
      {:error, reason} -> materialization_error(entry, reason)
      other -> materialization_error(entry, {:reference_checkout_failed, other})
    end
  end

  defp materialization_error(entry, reason) do
    %{entry | status: :error, description: entry.description <> " (#{inspect(reason)})"}
  end

  defp mark_selected_error(entries, reason) do
    Enum.map(entries, fn
      %{selected: true} = entry -> materialization_error(entry, reason)
      entry -> entry
    end)
  end

  defp materialization_concurrency(opts, total) do
    opts
    |> Keyword.get(:max_concurrency, 8)
    |> max(1)
    |> min(max(total, 1))
  end

  defp checkout_pinned(path, sha) do
    with {:ok, actual} <- Pika.Git.run(path, ["rev-parse", "HEAD"]),
         :ok <- fetch_if_needed(path, actual, sha),
         {:ok, _} <- Pika.Git.run(path, ["checkout", "--detach", sha]),
         {:ok, ^sha} <- Pika.Git.run(path, ["rev-parse", "HEAD"]),
         true <- Pika.Git.clean?(path) do
      :ok
    else
      false -> {:error, :reference_checkout_dirty}
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

  defp clone_pinned(workspace_root, path, entry) do
    refs_root = Path.dirname(path)

    temp =
      Path.join(
        refs_root,
        ".#{entry.id}.clone-#{System.unique_integer([:positive, :monotonic])}"
      )

    result =
      with {:ok, _} <-
             Pika.Git.run(workspace_root, [
               "-c",
               "protocol.file.allow=always",
               "clone",
               "--depth",
               "1",
               "--no-checkout",
               entry.url,
               temp
             ]),
           :ok <- checkout_pinned(temp, entry.sha),
           :ok <- File.rename(temp, path) do
        :ok
      end

    if File.exists?(temp), do: File.rm_rf(temp)
    result
  end

  defp verify_origin(path, expected) do
    case Pika.Git.run(path, ["remote", "get-url", "origin"]) do
      {:ok, actual} ->
        if canonical_url(actual) == canonical_url(expected),
          do: :ok,
          else: {:error, {:reference_origin_mismatch, actual, expected}}

      {:error, reason} ->
        {:error, reason}
    end
  end

  defp ensure_reference_exclude(worktree) do
    with {:ok, exclude_path} <-
           Pika.Git.run(worktree, ["rev-parse", "--git-path", "info/exclude"]) do
      exclude_path = Path.expand(exclude_path, worktree)

      :global.trans({{__MODULE__, :reference_exclude, exclude_path}, self()}, fn ->
        with :ok <- File.mkdir_p(Path.dirname(exclude_path)),
             {:ok, contents} <- read_optional(exclude_path) do
          patterns = String.split(contents, "\n", trim: true)

          if "/ref/" in patterns do
            :ok
          else
            separator = if contents == "" or String.ends_with?(contents, "\n"), do: "", else: "\n"
            File.write(exclude_path, contents <> separator <> "/ref/\n")
          end
        end
      end)
    end
  end

  defp read_optional(path) do
    case File.read(path) do
      {:error, :enoent} -> {:ok, ""}
      result -> result
    end
  end

  defp ensure_real_directory(path) do
    case File.lstat(path) do
      {:ok, %{type: :directory}} ->
        :ok

      {:ok, %{type: type}} ->
        {:error, {:path_conflict, path, type}}

      {:error, :enoent} ->
        case File.mkdir_p(path) do
          :ok ->
            case File.lstat(path) do
              {:ok, %{type: :directory}} -> :ok
              {:ok, %{type: type}} -> {:error, {:path_conflict, path, type}}
              {:error, reason} -> {:error, {:path_unavailable, path, reason}}
            end

          {:error, reason} ->
            {:error, {:path_create_failed, path, reason}}
        end

      {:error, reason} ->
        {:error, {:path_unavailable, path, reason}}
    end
  end

  defp remove_stale_links(refs_root, worktree, selected) do
    link_root = Path.join(worktree, "ref")
    selected_ids = MapSet.new(selected, & &1.id)

    link_root
    |> File.ls()
    |> case do
      {:ok, names} ->
        Enum.reduce_while(names, :ok, fn name, :ok ->
          path = Path.join(link_root, name)

          if not MapSet.member?(selected_ids, name) and managed_reference_link?(path, refs_root) do
            case File.rm(path) do
              :ok -> {:cont, :ok}
              {:error, reason} -> {:halt, {:error, {:reference_link_remove_failed, name, reason}}}
            end
          else
            {:cont, :ok}
          end
        end)

      {:error, :enoent} ->
        :ok

      {:error, reason} ->
        {:error, {:reference_link_directory_failed, reason}}
    end
  end

  defp ensure_reference_link(refs_root, worktree, entry) do
    source = Path.join(refs_root, entry.id)
    link = Path.join([worktree, "ref", entry.id])

    with :ok <- verify_checkout(source, entry),
         :ok <- ensure_link_target(link, source, refs_root) do
      :ok
    end
  end

  defp verify_checkout(path, entry) do
    with true <- File.dir?(Path.join(path, ".git")),
         {:ok, sha} <- Pika.Git.run(path, ["rev-parse", "HEAD"]),
         true <- sha == entry.sha,
         true <- Pika.Git.clean?(path) do
      :ok
    else
      false -> {:error, {:reference_checkout_invalid, entry.id}}
      {:error, reason} -> {:error, {:reference_checkout_invalid, entry.id, reason}}
      {:ok, actual} -> {:error, {:reference_sha_mismatch, entry.id, actual, entry.sha}}
    end
  end

  defp ensure_link_target(link, source, refs_root) do
    case File.lstat(link) do
      {:error, :enoent} ->
        case File.ln_s(source, link) do
          :ok -> :ok
          {:error, reason} -> {:error, {:reference_link_failed, link, reason}}
        end

      {:ok, %{type: :symlink}} ->
        with {:ok, target} <- File.read_link(link) do
          actual = Path.expand(target, Path.dirname(link))

          cond do
            actual == source ->
              :ok

            managed_reference_target?(actual, refs_root) ->
              with :ok <- File.rm(link), :ok <- File.ln_s(source, link), do: :ok

            true ->
              {:error, {:reference_link_conflict, link}}
          end
        end

      {:ok, _stat} ->
        {:error, {:reference_link_conflict, link}}

      {:error, reason} ->
        {:error, {:reference_link_failed, link, reason}}
    end
  end

  defp managed_reference_link?(path, refs_root) do
    case File.lstat(path) do
      {:ok, %{type: :symlink}} ->
        case File.read_link(path) do
          {:ok, target} ->
            target |> Path.expand(Path.dirname(path)) |> managed_reference_target?(refs_root)

          {:error, _reason} ->
            false
        end

      _other ->
        false
    end
  end

  defp managed_reference_target?(target, refs_root),
    do: Path.dirname(Path.expand(target)) == Path.expand(refs_root)

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

  defp normalize_id("", url) do
    id =
      url
      |> String.trim_trailing("/")
      |> String.split(["/", ":"], trim: true)
      |> List.last()
      |> case do
        nil -> ""
        value -> String.replace(value, ~r/\.git\z/i, "")
      end
      |> String.replace(~r/[^A-Za-z0-9._-]+/u, "-")
      |> String.trim("-._")

    if id == "", do: {:error, {:invalid_reference_project, :id, :cannot_derive}}, else: {:ok, id}
  end

  defp normalize_id(id, _url), do: {:ok, id}

  defp validate_id(""), do: {:error, {:invalid_reference_project, :id, :required}}

  defp validate_id(id) when byte_size(id) > @max_id_bytes,
    do: {:error, {:invalid_reference_project, :id, :too_long}}

  defp validate_id(id) do
    if Regex.match?(~r/\A[A-Za-z0-9][A-Za-z0-9._-]*\z/, id) and id not in [".", ".."] do
      :ok
    else
      {:error, {:invalid_reference_project, :id, :invalid_format}}
    end
  end

  defp validate_url(""), do: {:error, {:invalid_reference_project, :url, :required}}

  defp validate_url(url) when byte_size(url) > @max_url_bytes,
    do: {:error, {:invalid_reference_project, :url, :too_long}}

  defp validate_url("-" <> _rest),
    do: {:error, {:invalid_reference_project, :url, :invalid_format}}

  defp validate_url(url) do
    valid_location? =
      absolute_path?(url) or scp_location?(url) or supported_uri?(URI.parse(url))

    if valid_location? and not Regex.match?(~r/[\x00-\x20\x7f]/, url),
      do: :ok,
      else: {:error, {:invalid_reference_project, :url, :invalid_format}}
  end

  defp validate_description(description) when byte_size(description) > @max_description_bytes,
    do: {:error, {:invalid_reference_project, :description, :too_long}}

  defp validate_description(_description), do: :ok

  defp ensure_unique(id, url, entries) do
    cond do
      Enum.any?(entries, &(String.downcase(to_string(&1.id)) == String.downcase(id))) ->
        {:error, {:duplicate_reference_project, :id, id}}

      Enum.any?(entries, &(canonical_url(&1.url) == canonical_url(url))) ->
        {:error, {:duplicate_reference_project, :url, url}}

      true ->
        :ok
    end
  end

  defp absolute_path?(url), do: Path.type(url) == :absolute

  defp scp_location?(url),
    do: Regex.match?(~r/\A(?:[^@:\/\s]+@)?[^:\/\s]+:[^\/\s][^\s]*\z/, url)

  defp supported_uri?(%URI{scheme: scheme, path: path, host: host, userinfo: userinfo})
       when scheme in @supported_schemes do
    userinfo in [nil, ""] and is_binary(path) and path not in ["", "/"] and
      (scheme == "file" or is_binary(host))
  end

  defp supported_uri?(_uri), do: false

  defp canonical_url(url), do: url |> to_string() |> String.trim() |> String.trim_trailing("/")

  defp value(attrs, key), do: Map.get(attrs, key) || Map.get(attrs, to_string(key)) || ""

  defp normalize_text(value) when is_binary(value), do: String.trim(value)
  defp normalize_text(_value), do: ""
end
