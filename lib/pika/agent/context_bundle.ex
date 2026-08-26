defmodule Pika.Agent.ContextBundle do
  @moduledoc "Creates one immutable, file-first v2 Context Bundle for a fresh Backend Session."

  alias Pika.FileSystem
  alias Pika.Optimization.{Config, Persistence}
  alias Pika.Repo

  @optimization_id "optimization"

  @spec build(Config.t(), String.t(), String.t(), atom() | String.t(), String.t()) ::
          {:ok, map()} | {:error, term()}
  def build(%Config{} = config, session_id, role, work_kind, work_id)
      when is_binary(session_id) and is_binary(role) and is_binary(work_id) do
    work_kind = to_string(work_kind)
    directory = Path.join([config.workspace, "agent-sessions", session_id, "context"])

    with :ok <- require_new_directory(directory),
         {:ok, baseline} <- baseline_context(config.workspace, role),
         {:ok, facts} <- work_facts(role, work_kind, work_id, baseline),
         {:ok, files} <- materialize_files(directory, baseline, facts),
         context <- context(config, role, work_kind, work_id, baseline, facts, files),
         contents <- Jason.encode!(context, pretty: true),
         context_file = Path.join(directory, "context.json"),
         :ok <- FileSystem.atomic_write(context_file, contents),
         :ok <- FileSystem.freeze_files([context_file | Map.values(files)]) do
      {:ok,
       %{
         directory: directory,
         context_file: context_file,
         context: context,
         contents: contents,
         sha256: sha256(contents)
       }}
    end
  end

  defp require_new_directory(directory) do
    cond do
      File.exists?(directory) -> {:error, {:context_bundle_already_exists, directory}}
      true -> File.mkdir_p(directory)
    end
  end

  defp baseline_context(workspace, role) do
    {where, params} =
      if role in [
           "iteration",
           "integration",
           "iteration_followup",
           "integration_followup",
           "progress_summary"
         ] do
        {"AND status = 'accepted'", [@optimization_id]}
      else
        {"", [@optimization_id]}
      end

    case Repo.query!(
           """
           SELECT id, revision, status, work_relative_path, development_sha
           FROM baseline_revisions
           WHERE optimization_id = ? #{where}
           ORDER BY revision DESC LIMIT 1
           """,
           params
         ).rows do
      [[id, revision, status, relative_path, development_sha]] ->
        root = Path.join(workspace, relative_path)

        candidates = %{
          baseline_definition: Path.join(root, "baseline-definition.json"),
          cases: Path.join(root, "cases.json"),
          metrics: Path.join(root, "metrics.json")
        }

        sources = Map.filter(candidates, fn {_key, path} -> File.regular?(path) end)

        cases =
          case sources[:cases] do
            nil -> []
            path -> path |> File.read!() |> Jason.decode!() |> Map.fetch!("cases")
          end

        {:ok,
         %{
           id: id,
           revision: revision,
           status: status,
           development_sha: development_sha,
           root: root,
           sources: sources,
           cases: cases
         }}

      [] ->
        {:error, :accepted_baseline_missing}
    end
  end

  defp work_facts("iteration", "attempt", work_id, baseline),
    do: attempt_facts(work_id, baseline, :sampling)

  defp work_facts("integration", "attempt", work_id, baseline),
    do: attempt_facts(work_id, baseline, :full)

  defp work_facts(role, work_kind, work_id, baseline)
       when role in ["baseline_alignment", "baseline_verify"] do
    {:ok,
     %{
       role: role,
       work_kind: work_kind,
       work_id: work_id,
       baseline_revision: baseline.revision,
       guidance: nil
     }}
  end

  defp work_facts(role, work_kind, work_id, baseline)
       when role in [
              "baseline_verify_followup",
              "iteration_followup",
              "integration_followup",
              "progress_summary"
            ] do
    {:ok,
     %{
       role: role,
       work_kind: work_kind,
       work_id: work_id,
       baseline_revision: baseline.revision,
       guidance: nil
     }}
  end

  defp work_facts(role, work_kind, _work_id, _baseline),
    do: {:error, {:unsupported_context_work, role, work_kind}}

  defp attempt_facts(work_id, baseline, case_mode) do
    with :ok <- require_accepted_baseline(baseline),
         {:ok, attempt_id} <- integer_id(work_id),
         [row] <-
           Repo.query!(
             """
             SELECT current_iteration_round, base_sha, sampling_revision_id,
                    guidance_revision_id, candidate_sha, status, work_relative_path
             FROM attempts WHERE optimization_id = ? AND id = ?
             """,
             [@optimization_id, attempt_id]
           ).rows do
      [round, base_sha, sampling_id, guidance_id, candidate_sha, status, work_relative_path] = row

      [[sampling_revision]] =
        Repo.query!("SELECT sequence FROM sampling_revisions WHERE id = ?", [sampling_id]).rows

      case_ids =
        case case_mode do
          :sampling -> sampling_case_ids(sampling_id)
          :full -> Enum.map(baseline.cases, & &1["id"])
        end

      {:ok,
       %{
         attempt_id: attempt_id,
         iteration_round: round,
         base_sha: base_sha,
         candidate_sha: candidate_sha,
         status: status,
         work_relative_path: work_relative_path,
         sampling_revision: sampling_revision,
         sampling_case_ids: case_ids,
         guidance: guidance(guidance_id)
       }}
    else
      [] -> {:error, {:attempt_not_found, work_id}}
      {:error, _reason} = error -> error
    end
  end

  defp materialize_files(directory, baseline, facts) do
    copied =
      Enum.reduce_while(baseline.sources, {:ok, %{}}, fn {key, source}, {:ok, files} ->
        destination = Path.join(directory, Path.basename(source))

        case copy(source, destination) do
          :ok -> {:cont, {:ok, Map.put(files, key, destination)}}
          {:error, reason} -> {:halt, {:error, reason}}
        end
      end)

    guidance = Path.join(directory, "guidance.md")
    events = Path.join(directory, "events.jsonl")

    with {:ok, files} <- copied,
         :ok <- FileSystem.atomic_write(guidance, guidance_contents(facts.guidance)),
         :ok <- FileSystem.atomic_write(events, events_jsonl()) do
      {:ok, files |> Map.put(:guidance, guidance) |> Map.put(:events, events)}
    end
  end

  defp context(config, role, work_kind, work_id, baseline, facts, files) do
    optimization = Persistence.current()
    best = latest_best()
    {work_root, execution_cwd} = context_paths(config, role, baseline, facts)

    %{
      schema_version: 1,
      role: role,
      work_kind: work_kind,
      work_id: work_id,
      attempt_id: facts[:attempt_id],
      iteration_round: facts[:iteration_round],
      base_sha: facts[:base_sha],
      candidate_sha: facts[:candidate_sha],
      best_sha_at_session_start: best && best.sha,
      best_revision_at_session_start: best && best.sequence,
      baseline_revision: baseline.revision,
      development_sha: baseline.development_sha,
      sampling_revision: facts[:sampling_revision],
      sampling_case_ids: facts[:sampling_case_ids],
      optimization_status: optimization.status,
      workspace_root: config.workspace,
      work_root: work_root,
      execution_cwd: execution_cwd,
      files: Map.new(files, fn {key, path} -> {key, path} end)
    }
  end

  defp context_paths(_config, role, baseline, _facts)
       when role in ["baseline_alignment", "baseline_verify"] do
    nested = Path.join(baseline.root, "repo")
    {baseline.root, if(File.dir?(nested), do: nested, else: baseline.root)}
  end

  defp context_paths(config, "iteration", _baseline, facts) do
    root = Path.join(config.workspace, facts.work_relative_path)
    {root, Path.join(root, "repo")}
  end

  defp context_paths(config, "integration", _baseline, facts) do
    {Path.join(config.workspace, facts.work_relative_path), config.repo}
  end

  defp context_paths(config, _role, _baseline, _facts), do: {config.workspace, config.workspace}

  defp latest_best do
    case Repo.query!(
           "SELECT sequence, sha FROM best_revisions WHERE optimization_id = ? ORDER BY sequence DESC LIMIT 1",
           [@optimization_id]
         ).rows do
      [[sequence, sha]] -> %{sequence: sequence, sha: sha}
      [] -> nil
    end
  end

  defp sampling_case_ids(sampling_id) do
    Repo.query!(
      "SELECT case_id FROM sampling_revision_cases WHERE sampling_revision_id = ? ORDER BY case_id",
      [sampling_id]
    ).rows
    |> List.flatten()
  end

  defp guidance(nil), do: nil

  defp guidance(id) do
    case Repo.query!("SELECT sequence, body FROM guidance_revisions WHERE id = ?", [id]).rows do
      [[sequence, body]] -> %{sequence: sequence, body: body}
      [] -> nil
    end
  end

  defp guidance_contents(nil), do: ""
  defp guidance_contents(guidance), do: guidance.body <> "\n"

  defp events_jsonl do
    Repo.query!(
      """
      SELECT sequence, aggregate_type, aggregate_id, event_type, payload_json, created_at
      FROM domain_events WHERE optimization_id = ? ORDER BY sequence
      """,
      [@optimization_id]
    ).rows
    |> Enum.map_join("", fn [
                              sequence,
                              aggregate_type,
                              aggregate_id,
                              event_type,
                              payload,
                              created_at
                            ] ->
      Jason.encode!(%{
        sequence: sequence,
        aggregate_type: aggregate_type,
        aggregate_id: aggregate_id,
        event_type: event_type,
        payload: Jason.decode!(payload),
        created_at: created_at
      }) <> "\n"
    end)
  end

  defp copy(source, destination) do
    with {:ok, contents} <- File.read(source), do: FileSystem.atomic_write(destination, contents)
  end

  defp integer_id(value) do
    case Integer.parse(value) do
      {id, ""} when id > 0 -> {:ok, id}
      _other -> {:error, {:invalid_attempt_id, value}}
    end
  end

  defp require_accepted_baseline(%{status: "accepted", cases: [_ | _]}), do: :ok

  defp require_accepted_baseline(baseline),
    do: {:error, {:accepted_baseline_context_missing, baseline.revision, baseline.status}}

  defp sha256(contents), do: :crypto.hash(:sha256, contents) |> Base.encode16(case: :lower)
end
