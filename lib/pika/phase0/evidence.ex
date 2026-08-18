defmodule Pika.Phase0.Evidence do
  @moduledoc false

  @required_codex_methods [
    "initialize",
    "thread/start",
    "turn/start",
    "turn/steer",
    "turn/interrupt",
    "skills/extraRoots/set",
    "skills/list"
  ]

  def generate_codex_schema(artifact_dir) do
    artifact_dir = Path.expand(artifact_dir)
    File.mkdir_p!(artifact_dir)

    generated_dir =
      Path.join(System.tmp_dir!(), "pika-codex-schema-#{System.unique_integer([:positive])}")

    File.mkdir_p!(generated_dir)

    {generation_output, generation_status} =
      System.cmd("codex", ["app-server", "generate-json-schema", "--out", generated_dir],
        stderr_to_stdout: true
      )

    if generation_status != 0 do
      raise "Codex schema generation failed: #{generation_output}"
    end

    source = Path.join(generated_dir, "codex_app_server_protocol.v2.schemas.json")
    destination = Path.join(artifact_dir, "codex_app_server_protocol.v2.schemas.json")
    File.cp!(source, destination)
    contents = File.read!(destination)

    method_checks =
      Map.new(@required_codex_methods, &{&1, String.contains?(contents, ~s("#{&1}"))})

    if Enum.any?(method_checks, fn {_method, present} -> not present end) do
      raise "Codex schema is missing required methods: #{inspect(method_checks)}"
    end

    {version, 0} = System.cmd("codex", ["--version"], stderr_to_stdout: true)

    evidence = %{
      cli_version: String.trim(version),
      generated_at: DateTime.utc_now(),
      command: "codex app-server generate-json-schema --out <temporary-directory>",
      schema_file: Path.basename(destination),
      schema_sha256: sha256(contents),
      required_methods: method_checks
    }

    write_json(Path.join(artifact_dir, "codex-schema-evidence.json"), evidence)
    evidence
  end

  def write_json(path, value) do
    File.mkdir_p!(Path.dirname(path))
    File.write!(path, Jason.encode!(json_safe(value), pretty: true) <> "\n")
    path
  end

  def json_safe(%DateTime{} = value), do: DateTime.to_iso8601(value)
  def json_safe(%_{} = value), do: value |> Map.from_struct() |> json_safe()

  def json_safe(map) when is_map(map) do
    Map.new(map, fn {key, value} -> {to_string(key), json_safe(value)} end)
  end

  def json_safe(list) when is_list(list), do: Enum.map(list, &json_safe/1)
  def json_safe(tuple) when is_tuple(tuple), do: tuple |> Tuple.to_list() |> json_safe()
  def json_safe(value) when value in [true, false, nil], do: value
  def json_safe(value) when is_atom(value), do: Atom.to_string(value)
  def json_safe(value), do: value

  defp sha256(contents), do: :crypto.hash(:sha256, contents) |> Base.encode16(case: :lower)
end
