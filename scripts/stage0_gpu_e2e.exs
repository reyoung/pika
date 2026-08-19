argv =
  case System.argv() do
    ["--" | rest] -> rest
    other -> other
  end

{opts, args, invalid} =
  OptionParser.parse(argv,
    strict: [workspace: :string, artifact_dir: :string, skip_reference_resolution: :boolean]
  )

if invalid != [] or length(args) != 1,
  do: Mix.raise("usage: mix run scripts/stage0_gpu_e2e.exs -- <repo> [--workspace DIR]")

repo = hd(args)

case Pika.Stage0.GPUE2E.run(repo,
       workspace: Keyword.get(opts, :workspace),
       artifact_dir: Keyword.get(opts, :artifact_dir, "artifacts/stage0-demo"),
       resolve_references: not Keyword.get(opts, :skip_reference_resolution, false)
     ) do
  {:ok, result} -> IO.puts(Jason.encode!(Pika.Phase0.Evidence.json_safe(result), pretty: true))
  {:error, reason} -> Mix.raise(reason)
end
