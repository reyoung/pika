argv =
  case System.argv() do
    ["--" | rest] -> rest
    other -> other
  end

{opts, _args, invalid} =
  OptionParser.parse(argv,
    strict: [
      backend: :string,
      artifact_dir: :string,
      pair_count: :integer,
      min_valid_pairs: :integer
    ]
  )

pair_count = opts[:pair_count]
min_valid_pairs = opts[:min_valid_pairs]

if invalid != [] or not (is_integer(pair_count) and pair_count > 0) or
     not (is_integer(min_valid_pairs) and min_valid_pairs in 1..pair_count),
   do:
     Mix.raise(
       "usage: mix run scripts/alignment_boundary_smoke.exs -- --pair-count N --min-valid-pairs N [--backend codex|cursor]"
     )

backend =
  case Keyword.get(opts, :backend, "codex") do
    "codex" -> :codex_app_server
    "cursor" -> :cursor_acp
    other -> Mix.raise("unknown backend: #{other}")
  end

case Pika.Alignment.BoundarySmoke.run(backend,
       artifact_dir: Keyword.get(opts, :artifact_dir, "artifacts/alignment-preview"),
       pair_count: pair_count,
       min_valid_pairs: min_valid_pairs
     ) do
  {:ok, result} -> IO.puts(Jason.encode!(Pika.JSONSafe.json_safe(result), pretty: true))
  {:error, reason} -> Mix.raise(reason)
end
