defmodule Pika.Phase2.Profiler do
  @moduledoc false

  def validate_manifest(path, target_case_ids, measured_sha, skill_sha) do
    workspace_root = path |> Path.dirname() |> Path.dirname() |> Path.dirname()

    with {:ok, body} <- File.read(path),
         {:ok, manifest} <- Jason.decode(body),
         true <- manifest["schema_version"] == 1,
         true <- manifest["measured_sha"] == measured_sha,
         true <- manifest["case_id"] in target_case_ids,
         true <- manifest["skill_sha"] == skill_sha,
         true <- present?(manifest["tool"]),
         true <- present?(manifest["command"]),
         true <- present?(manifest["summary"]),
         directory when is_binary(directory) <- manifest["profile_directory"],
         true <- profile_directory?(directory),
         reports when is_list(reports) and reports != [] <- manifest["report_paths"],
         evidence when is_list(evidence) and evidence != [] <- manifest["remote_evidence_paths"],
         %{"skill" => "ncu-report-skill", "command" => parser_command, "output_paths" => outputs}
         when is_binary(parser_command) and is_list(outputs) and outputs != [] <-
           manifest["parser"],
         true <- present?(parser_command),
         true <- Enum.all?(reports, &under_directory?(&1, directory)),
         true <- Enum.all?(outputs, &(&1 in reports)),
         true <- Enum.any?(reports, &String.ends_with?(&1, ".ncu-rep")),
         :ok <- validate_parser_outputs(workspace_root, outputs) do
      {:ok, manifest}
    else
      _ -> {:error, :invalid_profiler_manifest}
    end
  end

  defp validate_parser_outputs(root, paths) do
    if Enum.all?(paths, &parsed_output?(Path.join(root, &1))),
      do: :ok,
      else: {:error, :invalid_profiler_parser_output}
  end

  defp parsed_output?(path) do
    case File.read(path) do
      {:ok, contents} when byte_size(contents) > 0 ->
        if String.ends_with?(path, ".json"),
          do: match?({:ok, _}, Jason.decode(contents)),
          else: true

      _ ->
        false
    end
  end

  defp profile_directory?(path) do
    Path.type(path) == :relative and
      (path == "artifacts/profiles" or String.starts_with?(path, "artifacts/profiles/")) and
      ".." not in Path.split(path)
  end

  defp under_directory?(path, directory) when is_binary(path) do
    Path.type(path) == :relative and
      (path == directory or String.starts_with?(path, String.trim_trailing(directory, "/") <> "/")) and
      ".." not in Path.split(path)
  end

  defp under_directory?(_path, _directory), do: false
  defp present?(value), do: is_binary(value) and String.trim(value) != ""
end
