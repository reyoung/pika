defmodule Pika.Agent.Template do
  @moduledoc "Loads and renders non-executable Workspace Role guidance."

  @placeholder ~r/\{\{\s*([a-zA-Z_][a-zA-Z0-9_.-]*)\s*\}\}/

  def resolve(workspace, %{relative_path: relative, builtin: builtin}, assigns)
      when is_binary(relative) and is_binary(builtin) and is_map(assigns) do
    path = Path.join(workspace.root, relative)

    with {:ok, source, origin} <- source(path, builtin),
         {:ok, rendered} <- render(source, assigns) do
      {:ok,
       %{
         text: rendered,
         source: origin,
         sha256: sha256(source)
       }}
    end
  end

  def resolve(_workspace, _template, _assigns), do: {:error, :invalid_template_spec}

  def render(source, assigns) when is_binary(source) and is_map(assigns) do
    placeholders = Regex.scan(@placeholder, source, capture: :all_but_first) |> List.flatten()

    Enum.reduce_while(placeholders, {:ok, source}, fn placeholder, {:ok, rendered} ->
      case lookup(assigns, String.split(placeholder, ".")) do
        {:ok, value} ->
          replacement = printable(value)
          pattern = ~r/\{\{\s*#{Regex.escape(placeholder)}\s*\}\}/
          {:cont, {:ok, Regex.replace(pattern, rendered, replacement)}}

        :error ->
          {:halt, {:error, {:unknown_template_variable, placeholder}}}
      end
    end)
  end

  defp source(path, builtin) do
    case File.read(path) do
      {:ok, source} -> {:ok, source, {:workspace, path}}
      {:error, :enoent} -> {:ok, builtin, :builtin}
      {:error, reason} -> {:error, {:template_read_failed, path, reason}}
    end
  end

  defp lookup(value, []), do: {:ok, value}

  defp lookup(map, [key | rest]) when is_map(map) do
    atom_key = existing_atom(key)

    cond do
      Map.has_key?(map, key) -> lookup(Map.fetch!(map, key), rest)
      atom_key && Map.has_key?(map, atom_key) -> lookup(Map.fetch!(map, atom_key), rest)
      true -> :error
    end
  end

  defp lookup(_value, _path), do: :error

  defp existing_atom(key) do
    String.to_existing_atom(key)
  rescue
    ArgumentError -> nil
  end

  defp printable(value) when is_binary(value), do: value
  defp printable(value) when is_atom(value) or is_number(value), do: to_string(value)
  defp printable(value), do: Jason.encode!(Pika.JSONSafe.json_safe(value), pretty: true)

  defp sha256(value),
    do: :crypto.hash(:sha256, value) |> Base.encode16(case: :lower)
end
