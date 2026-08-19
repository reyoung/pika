defmodule Pika.Phase2.Metrics do
  @moduledoc false

  alias Pika.Repo

  def baseline_series(campaign_id) do
    Repo.query!(
      """
      SELECT sr.revision, sr.id, bc.name, md.name, br.sequence, br.sha,
             bm.value, bm.mad, bm.noise_tolerance, bm.valid_pair_count
      FROM best_metrics bm
      JOIN best_revisions br ON br.id = bm.best_revision_id
      JOIN spec_revisions sr ON sr.id = br.spec_revision_id
      JOIN benchmark_cases bc ON bc.id = bm.benchmark_case_id
      JOIN metric_definitions md ON md.id = bm.metric_definition_id
      WHERE br.campaign_id = ?
      ORDER BY sr.revision, bc.ordinal, md.name, br.sequence
      """,
      [campaign_id]
    ).rows
    |> Enum.group_by(fn [_revision, spec_id, case_name, metric_name | _] ->
      {spec_id, case_name, metric_name}
    end)
    |> Enum.map(fn {{spec_id, case_name, metric_name}, rows} ->
      %{
        spec_revision_id: spec_id,
        spec_revision: rows |> hd() |> hd(),
        case_id: case_name,
        metric_id: metric_name,
        points:
          Enum.map(rows, fn [
                              _revision,
                              _spec_id,
                              _case,
                              _metric,
                              sequence,
                              sha,
                              value,
                              mad,
                              noise,
                              valid
                            ] ->
            %{
              best_sequence: sequence,
              sha: sha,
              value: value,
              mad: mad,
              noise_tolerance: noise,
              valid_pair_count: valid
            }
          end)
      }
    end)
    |> Enum.sort_by(&{&1.spec_revision, &1.case_id, &1.metric_id})
  end
end
