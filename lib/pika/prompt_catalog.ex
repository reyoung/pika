defmodule Pika.PromptCatalog do
  @moduledoc false

  @alignment_kinds [:alignment, :setup_merge, :baseline]
  @kinds @alignment_kinds ++ [:plan, :iteration, :integration, :sync]

  def validate(kinds \\ @alignment_kinds) do
    Enum.reduce_while(kinds, :ok, fn kind, :ok ->
      with {:ok, path} <- configured_path(kind),
           {:ok, source} <- File.read(path),
           {:ok, _quoted} <- compile(source) do
        {:cont, :ok}
      else
        {:error, reason} -> {:halt, {:error, {:prompt_resource_invalid, kind, reason}}}
      end
    end)
  end

  def render(kind, assigns) when kind in @kinds and is_map(assigns) do
    with {:ok, path} <- configured_path(kind),
         {:ok, source} <- File.read(path) do
      try do
        {:ok, EEx.eval_string(source, assigns: Map.to_list(assigns), file: path)}
      rescue
        error -> {:error, {:render_failed, Exception.message(error)}}
      end
    end
  end

  defp configured_path(kind) do
    case Application.get_env(:pika, __MODULE__, []) |> Keyword.fetch(kind) do
      {:ok, {:priv, relative}} when is_binary(relative) ->
        {:ok, Application.app_dir(:pika, Path.join("priv", relative))}

      {:ok, path} when is_binary(path) ->
        {:ok, Path.expand(path)}

      {:ok, value} ->
        {:error, {:invalid_path, value}}

      :error ->
        {:error, :not_configured}
    end
  end

  defp compile(source) do
    {:ok, EEx.compile_string(source)}
  rescue
    error -> {:error, {:compile_failed, Exception.message(error)}}
  end
end
