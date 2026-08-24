defmodule Pika.Optimization.ArtifactStore do
  @moduledoc "Registers already-validated v2 files by Pika-computed identity."

  alias Pika.Optimization.{FileContract, Persistence}
  alias Pika.Repo

  @optimization_id "optimization"

  @spec register(String.t(), String.t(), String.t(), FileContract.receipt()) ::
          {:ok, String.t()} | {:error, term()}
  def register(owner_type, owner_id, kind, receipt) do
    optimization = Persistence.current()

    with :ok <- optimization_present(optimization),
         {:ok, relative_path} <-
           workspace_relative(optimization.workspace_canonical_path, receipt.absolute_path) do
      upsert(owner_type, owner_id, kind, relative_path, receipt)
    else
      {:error, _reason} = error -> error
    end
  end

  defp optimization_present(value) when is_map(value), do: :ok
  defp optimization_present(_value), do: {:error, :optimization_not_initialized}

  defp upsert(owner_type, owner_id, kind, relative_path, receipt) do
    now = System.system_time(:microsecond)

    case Repo.query!(
           """
           SELECT id, owner_type, owner_id FROM artifacts
           WHERE optimization_id = ? AND relative_path = ?
           """,
           [@optimization_id, relative_path]
         ).rows do
      [] ->
        id = Ecto.UUID.generate()

        Repo.query!(
          """
          INSERT INTO artifacts(
            id, optimization_id, owner_type, owner_id, kind, relative_path,
            sha256, byte_size, mime_type, metadata_json, created_at
          ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, '{}', ?)
          """,
          [
            id,
            @optimization_id,
            owner_type,
            owner_id,
            kind,
            relative_path,
            receipt.sha256,
            receipt.byte_size,
            MIME.from_path(receipt.absolute_path),
            now
          ]
        )

        {:ok, id}

      [[id, ^owner_type, ^owner_id]] ->
        Repo.query!(
          "UPDATE artifacts SET kind = ?, sha256 = ?, byte_size = ?, mime_type = ? WHERE id = ?",
          [kind, receipt.sha256, receipt.byte_size, MIME.from_path(receipt.absolute_path), id]
        )

        {:ok, id}

      [[_id, existing_type, existing_id]] ->
        {:error, {:artifact_owner_conflict, relative_path, existing_type, existing_id}}
    end
  end

  defp workspace_relative(workspace, absolute) do
    relative = Path.relative_to(Path.expand(absolute), Path.expand(workspace))

    if relative == "." or not String.starts_with?(relative, "..") do
      {:ok, relative}
    else
      {:error, {:path_outside_workspace, absolute}}
    end
  end
end
