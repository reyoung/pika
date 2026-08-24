defmodule Pika.Optimization.SchemaValidator do
  @moduledoc "Validates v2 Agent JSON against the Pika-owned local JSON Schema subset."

  @type validation_error :: %{path: String.t(), message: String.t()}

  @spec validate(term(), map()) :: :ok | {:error, [validation_error()]}
  def validate(value, schema) when is_map(schema) do
    case errors(value, schema, schema, "") do
      [] -> :ok
      errors -> {:error, Enum.sort_by(errors, &{&1.path, &1.message})}
    end
  end

  @spec validate_json_file(Path.t(), Path.t()) ::
          {:ok, term()}
          | {:error, {:invalid_json, term()} | {:schema_validation_failed, [validation_error()]}}
  def validate_json_file(path, schema_path) do
    with {:ok, contents} <- File.read(path),
         {:ok, value} <- decode(contents),
         {:ok, schema_contents} <- File.read(schema_path),
         {:ok, schema} <- decode(schema_contents),
         :ok <- validate(value, schema) do
      {:ok, value}
    else
      {:error, errors} when is_list(errors) -> {:error, {:schema_validation_failed, errors}}
      {:error, {:invalid_json, _reason}} = error -> error
      {:error, reason} -> {:error, reason}
    end
  end

  defp decode(contents) do
    case Jason.decode(contents) do
      {:ok, value} -> {:ok, value}
      {:error, reason} -> {:error, {:invalid_json, reason}}
    end
  end

  defp errors(value, schema, root, path) do
    reference_errors(value, schema, root, path) ++
      conditional_errors(value, schema, root, path) ++
      combined_errors(value, schema, root, path) ++
      value_errors(value, schema, root, path)
  end

  defp reference_errors(value, %{"$ref" => reference}, root, path) do
    case resolve_reference(root, reference) do
      {:ok, referenced} -> errors(value, referenced, root, path)
      :error -> [error(path, "unresolved schema reference #{reference}")]
    end
  end

  defp reference_errors(_value, _schema, _root, _path), do: []

  defp conditional_errors(value, %{"if" => condition} = schema, root, path) do
    branch =
      if errors(value, condition, root, path) == [], do: schema["then"], else: schema["else"]

    if is_map(branch), do: errors(value, branch, root, path), else: []
  end

  defp conditional_errors(_value, _schema, _root, _path), do: []

  defp combined_errors(value, %{"allOf" => schemas}, root, path) when is_list(schemas) do
    Enum.flat_map(schemas, &errors(value, &1, root, path))
  end

  defp combined_errors(_value, _schema, _root, _path), do: []

  defp value_errors(value, schema, root, path) do
    type_errors = type_errors(value, schema, path)

    if type_errors == [] do
      const_errors(value, schema, path) ++
        enum_errors(value, schema, path) ++
        string_errors(value, schema, path) ++
        number_errors(value, schema, path) ++
        array_errors(value, schema, root, path) ++
        object_errors(value, schema, root, path)
    else
      type_errors
    end
  end

  defp type_errors(value, %{"type" => types}, path) when is_list(types) do
    if Enum.any?(types, &matches_type?(value, &1)),
      do: [],
      else: [error(path, "expected type #{Enum.join(types, " or ")}")]
  end

  defp type_errors(value, %{"type" => type}, path) when is_binary(type) do
    if matches_type?(value, type), do: [], else: [error(path, "expected type #{type}")]
  end

  defp type_errors(_value, _schema, _path), do: []

  defp const_errors(value, %{"const" => expected}, path) do
    if value == expected, do: [], else: [error(path, "expected constant #{inspect(expected)}")]
  end

  defp const_errors(_value, _schema, _path), do: []

  defp enum_errors(value, %{"enum" => allowed}, path) when is_list(allowed) do
    if value in allowed, do: [], else: [error(path, "expected one of #{inspect(allowed)}")]
  end

  defp enum_errors(_value, _schema, _path), do: []

  defp string_errors(value, schema, path) when is_binary(value) do
    []
    |> maybe_error(
      is_integer(schema["minLength"]) and String.length(value) < schema["minLength"],
      path,
      "string is shorter than #{schema["minLength"]}"
    )
    |> maybe_error(
      is_integer(schema["maxLength"]) and String.length(value) > schema["maxLength"],
      path,
      "string is longer than #{schema["maxLength"]}"
    )
    |> pattern_error(value, schema, path)
  end

  defp string_errors(_value, _schema, _path), do: []

  defp pattern_error(errors, value, %{"pattern" => pattern}, path) do
    case Regex.compile(pattern) do
      {:ok, regex} ->
        maybe_error(errors, not Regex.match?(regex, value), path, "does not match pattern")

      {:error, _reason} ->
        [error(path, "schema contains invalid pattern") | errors]
    end
  end

  defp pattern_error(errors, _value, _schema, _path), do: errors

  defp number_errors(value, schema, path) when is_number(value) do
    []
    |> maybe_error(
      is_number(schema["minimum"]) and value < schema["minimum"],
      path,
      "number is below minimum #{schema["minimum"]}"
    )
    |> maybe_error(
      is_number(schema["exclusiveMinimum"]) and value <= schema["exclusiveMinimum"],
      path,
      "number must be greater than #{schema["exclusiveMinimum"]}"
    )
  end

  defp number_errors(_value, _schema, _path), do: []

  defp array_errors(value, schema, root, path) when is_list(value) do
    cardinality =
      []
      |> maybe_error(
        is_integer(schema["minItems"]) and length(value) < schema["minItems"],
        path,
        "array has fewer than #{schema["minItems"]} items"
      )
      |> maybe_error(
        is_integer(schema["maxItems"]) and length(value) > schema["maxItems"],
        path,
        "array has more than #{schema["maxItems"]} items"
      )
      |> maybe_error(
        schema["uniqueItems"] == true and Enum.uniq(value) != value,
        path,
        "array items are not unique"
      )

    item_errors =
      case schema["items"] do
        item_schema when is_map(item_schema) ->
          value
          |> Enum.with_index()
          |> Enum.flat_map(fn {item, index} ->
            errors(item, item_schema, root, pointer(path, Integer.to_string(index)))
          end)

        _other ->
          []
      end

    cardinality ++ item_errors
  end

  defp array_errors(_value, _schema, _root, _path), do: []

  defp object_errors(value, schema, root, path) when is_map(value) do
    properties = schema["properties"] || %{}
    required = schema["required"] || []

    required_errors =
      for key <- required, not Map.has_key?(value, key) do
        error(pointer(path, key), "required property is missing")
      end

    property_errors =
      Enum.flat_map(properties, fn {key, property_schema} ->
        if Map.has_key?(value, key) do
          errors(value[key], property_schema, root, pointer(path, key))
        else
          []
        end
      end)

    additional_errors =
      if schema["additionalProperties"] == false do
        value
        |> Map.keys()
        |> Kernel.--(Map.keys(properties))
        |> Enum.map(&error(pointer(path, &1), "additional property is not allowed"))
      else
        []
      end

    required_errors ++ property_errors ++ additional_errors
  end

  defp object_errors(_value, _schema, _root, _path), do: []

  defp resolve_reference(root, "#/" <> path) do
    keys = String.split(path, "/", trim: true) |> Enum.map(&unescape_pointer/1)

    case get_in_map(root, keys) do
      nil -> :error
      value -> {:ok, value}
    end
  end

  defp resolve_reference(_root, _reference), do: :error

  defp get_in_map(value, []), do: value
  defp get_in_map(value, [key | rest]) when is_map(value), do: get_in_map(value[key], rest)
  defp get_in_map(_value, _keys), do: nil

  defp matches_type?(value, "object"), do: is_map(value)
  defp matches_type?(value, "array"), do: is_list(value)
  defp matches_type?(value, "string"), do: is_binary(value)
  defp matches_type?(value, "integer"), do: is_integer(value)
  defp matches_type?(value, "number"), do: is_number(value)
  defp matches_type?(value, "boolean"), do: is_boolean(value)
  defp matches_type?(value, "null"), do: is_nil(value)
  defp matches_type?(_value, _type), do: false

  defp pointer("", token), do: "/" <> escape_pointer(token)
  defp pointer(path, token), do: path <> "/" <> escape_pointer(token)
  defp escape_pointer(token), do: token |> String.replace("~", "~0") |> String.replace("/", "~1")

  defp unescape_pointer(token),
    do: token |> String.replace("~1", "/") |> String.replace("~0", "~")

  defp maybe_error(errors, true, path, message), do: [error(path, message) | errors]
  defp maybe_error(errors, false, _path, _message), do: errors
  defp error("", message), do: %{path: "/", message: message}
  defp error(path, message), do: %{path: path, message: message}
end
