argv =
  case System.argv() do
    ["--" | rest] -> rest
    other -> other
  end

{opts, args, invalid} =
  OptionParser.parse(argv,
    strict: [
      workspace: :string,
      artifact_dir: :string,
      skip_reference_resolution: :boolean,
      pair_count: :integer,
      min_valid_pairs: :integer
    ]
  )

pair_count = opts[:pair_count]
min_valid_pairs = opts[:min_valid_pairs]

if invalid != [] or length(args) != 1 or not (is_integer(pair_count) and pair_count > 0) or
     not (is_integer(min_valid_pairs) and min_valid_pairs in 1..pair_count),
   do:
     Mix.raise(
       "usage: mix run scripts/baseline_gpu_e2e.exs -- <repo> --pair-count N --min-valid-pairs N [--workspace DIR]"
     )

repo = hd(args)

case Pika.Baseline.GPUE2E.run(repo,
       workspace: Keyword.get(opts, :workspace),
       artifact_dir: Keyword.get(opts, :artifact_dir, "artifacts/alignment-preview"),
       resolve_references: not Keyword.get(opts, :skip_reference_resolution, false),
       pair_count: pair_count,
       min_valid_pairs: min_valid_pairs
     ) do
  {:ok, result} -> IO.puts(Jason.encode!(Pika.JSONSafe.json_safe(result), pretty: true))
  {:error, reason} -> Mix.raise(reason)
end
