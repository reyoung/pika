defmodule Pika.Optimization.FileContract do
  @moduledoc "Validates Agent-owned local files before domain commands consume them."

  alias Pika.Optimization.SchemaValidator
  alias Pika.Paths

  @default_max_bytes 16 * 1024 * 1024

  @type receipt :: %{
          relative_path: Path.t(),
          absolute_path: Path.t(),
          sha256: String.t(),
          byte_size: non_neg_integer()
        }

  @spec read(Path.t(), Path.t(), keyword()) :: {:ok, binary(), receipt()} | {:error, term()}
  def read(root, relative_path, opts \\ []) when is_binary(root) and is_binary(relative_path) do
    max_bytes = Keyword.get(opts, :max_bytes, @default_max_bytes)

    with {:ok, absolute, normalized_root} <- resolve(root, relative_path),
         {:ok, %{type: :regular, size: size}} <- File.lstat(absolute),
         :ok <- bounded(size, max_bytes),
         {:ok, contents} <- File.read(absolute) do
      {:ok, contents,
       %{
         relative_path: Path.relative_to(absolute, normalized_root),
         absolute_path: absolute,
         sha256: sha256(contents),
         byte_size: size
       }}
    else
      {:ok, stat} -> {:error, {:not_regular_file, stat.type}}
      {:error, reason} -> {:error, reason}
    end
  end

  @spec validate_json(Path.t(), Path.t(), Path.t(), keyword()) ::
          {:ok, term(), receipt()} | {:error, term()}
  def validate_json(root, relative_path, schema_path, opts \\ []) do
    with {:ok, contents, receipt} <- read(root, relative_path, opts),
         {:ok, value} <- decode_json(contents),
         {:ok, schema_contents} <- File.read(schema_path),
         {:ok, schema} <- decode_json(schema_contents),
         :ok <- SchemaValidator.validate(value, schema) do
      {:ok, value, receipt}
    else
      {:error, errors} when is_list(errors) -> {:error, {:schema_validation_failed, errors}}
      {:error, reason} -> {:error, reason}
    end
  end

  @spec validate_jsonl(Path.t(), Path.t(), Path.t(), keyword()) ::
          {:ok, [term()], receipt()} | {:error, term()}
  def validate_jsonl(root, relative_path, record_schema_path, opts \\ []) do
    with {:ok, contents, receipt} <- read(root, relative_path, opts),
         :ok <- nonempty_jsonl(contents),
         {:ok, schema_contents} <- File.read(record_schema_path),
         {:ok, schema} <- decode_json(schema_contents),
         {:ok, records} <- validate_lines(contents, schema) do
      {:ok, records, receipt}
    else
      {:error, reason} -> {:error, reason}
    end
  end

  @spec resolve(Path.t(), Path.t()) :: {:ok, Path.t(), Path.t()} | {:error, term()}
  def resolve(root, relative_path) when is_binary(root) and is_binary(relative_path) do
    with {:ok, canonical_root} <- Paths.canonical(root),
         :ok <- validate_relative(relative_path),
         :ok <- reject_symlink_components(canonical_root, Path.split(relative_path)) do
      {:ok, Path.join(canonical_root, relative_path), canonical_root}
    end
  end

  def resolve(_root, _relative_path), do: {:error, :invalid_file_path}

  defp validate_lines(contents, schema) do
    contents
    |> String.split("\n", trim: false)
    |> drop_final_empty()
    |> Enum.with_index(1)
    |> Enum.reduce_while({:ok, []}, fn {line, line_number}, {:ok, records} ->
      with :ok <- nonempty_line(line, line_number),
           {:ok, value} <- decode_json(line),
           :ok <- validate_record(value, schema, line_number) do
        {:cont, {:ok, [value | records]}}
      else
        {:error, reason} -> {:halt, {:error, reason}}
      end
    end)
    |> case do
      {:ok, records} -> {:ok, Enum.reverse(records)}
      error -> error
    end
  end

  defp validate_record(value, schema, line_number) do
    case SchemaValidator.validate(value, schema) do
      :ok -> :ok
      {:error, errors} -> {:error, {:jsonl_schema_validation_failed, line_number, errors}}
    end
  end

  defp drop_final_empty(lines) do
    case Enum.reverse(lines) do
      ["" | rest] -> Enum.reverse(rest)
      _other -> lines
    end
  end

  defp nonempty_jsonl(""), do: {:error, :empty_jsonl}
  defp nonempty_jsonl(_contents), do: :ok

  defp nonempty_line("", line_number), do: {:error, {:empty_jsonl_line, line_number}}
  defp nonempty_line(_line, _line_number), do: :ok

  defp validate_relative(""), do: {:error, :empty_file_path}

  defp validate_relative(relative_path) do
    normalized = relative_path |> Path.split() |> Path.join()

    cond do
      Path.type(relative_path) == :absolute ->
        {:error, :absolute_file_path}

      normalized != relative_path ->
        {:error, :noncanonical_file_path}

      Enum.any?(Path.split(relative_path), &(&1 in ["", ".", ".."])) ->
        {:error, :file_path_escape}

      true ->
        :ok
    end
  end

  defp reject_symlink_components(root, components) do
    components
    |> Enum.reduce_while(root, fn component, current ->
      next = Path.join(current, component)

      case File.lstat(next) do
        {:ok, %{type: :symlink}} -> {:halt, {:error, {:symlink_component, next}}}
        {:ok, _stat} -> {:cont, next}
        {:error, reason} -> {:halt, {:error, {:file_stat_failed, next, reason}}}
      end
    end)
    |> case do
      {:error, _reason} = error -> error
      _path -> :ok
    end
  end

  defp bounded(size, max_bytes)
       when is_integer(size) and is_integer(max_bytes) and max_bytes >= 0 and size <= max_bytes,
       do: :ok

  defp bounded(size, max_bytes), do: {:error, {:file_too_large, size, max_bytes}}

  defp decode_json(contents) do
    case Jason.decode(contents) do
      {:ok, value} -> {:ok, value}
      {:error, reason} -> {:error, {:invalid_json, reason}}
    end
  end

  defp sha256(contents), do: :crypto.hash(:sha256, contents) |> Base.encode16(case: :lower)
end
