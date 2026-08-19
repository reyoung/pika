argv =
  case System.argv() do
    ["--" | rest] -> rest
    other -> other
  end

{opts, _args, invalid} =
  OptionParser.parse(argv,
    strict: [
      backend: :string,
      workspace: :string,
      artifact_dir: :string,
      skip_control_probes: :boolean
    ]
  )

if invalid != [] do
  Mix.raise("invalid arguments: #{inspect(invalid)}")
end

backend = Keyword.get(opts, :backend, "codex_app_server")

backends =
  case backend do
    "all" -> [:codex_app_server, :cursor_acp]
    "codex_app_server" -> [:codex_app_server]
    "cursor_acp" -> [:cursor_acp]
    other -> Mix.raise("unsupported backend: #{other}")
  end

run_opts = [
  backends: backends,
  workspace: Keyword.get(opts, :workspace, File.cwd!()),
  artifact_dir: Keyword.get(opts, :artifact_dir, "artifacts/backend-conformance"),
  control_probes: not Keyword.get(opts, :skip_control_probes, false)
]

case Pika.AgentBackend.Conformance.run(run_opts) do
  {:ok, result} -> IO.puts(Jason.encode!(Pika.JSONSafe.json_safe(result), pretty: true))
  {:error, reason} -> Mix.raise(reason)
end
