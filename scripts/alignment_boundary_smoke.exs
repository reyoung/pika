argv =
  case System.argv() do
    ["--" | rest] -> rest
    other -> other
  end

{opts, _args, invalid} =
  OptionParser.parse(argv, strict: [backend: :string, artifact_dir: :string])

if invalid != [], do: Mix.raise("invalid arguments: #{inspect(invalid)}")

backend =
  case Keyword.get(opts, :backend, "codex") do
    "codex" -> :codex_app_server
    "cursor" -> :cursor_acp
    other -> Mix.raise("unknown backend: #{other}")
  end

case Pika.Alignment.BoundarySmoke.run(backend,
       artifact_dir: Keyword.get(opts, :artifact_dir, "artifacts/alignment-preview")
     ) do
  {:ok, result} -> IO.puts(Jason.encode!(Pika.JSONSafe.json_safe(result), pretty: true))
  {:error, reason} -> Mix.raise(reason)
end
