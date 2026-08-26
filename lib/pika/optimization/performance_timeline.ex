defmodule Pika.Optimization.PerformanceTimeline do
  @moduledoc "Builds the normalized per-Case performance history shown in the live banner."

  alias Pika.Repo

  @optimization_id "optimization"

  @spec version() :: tuple()
  def version do
    case Repo.query!(
           """
           SELECT
             (SELECT COUNT(*) FROM attempts WHERE optimization_id = ?),
             (SELECT COALESCE(MAX(updated_at), 0) FROM attempts WHERE optimization_id = ?),
             (SELECT COUNT(*) FROM attempt_metrics am
                JOIN attempts a ON a.id = am.attempt_id WHERE a.optimization_id = ?),
             (SELECT COUNT(*) FROM best_revisions WHERE optimization_id = ?),
             (SELECT COALESCE(MAX(created_at), 0) FROM best_revisions WHERE optimization_id = ?)
           """,
           List.duplicate(@optimization_id, 5)
         ).rows do
      [[attempts, attempts_updated_at, attempt_metrics, best_revisions, best_created_at]] ->
        {attempts, attempts_updated_at, attempt_metrics, best_revisions, best_created_at}
    end
  end

  @spec load() :: %{points: [map()], cases: [map()], metrics: [map()]}
  def load do
    points = (best_points() ++ attempt_points()) |> Enum.sort_by(&point_order/1)

    %{
      points: points,
      cases: catalog(points, :case_id, :case_name),
      metrics: catalog(points, :metric_id, :unit)
    }
  end

  defp best_points do
    Repo.query!(
      """
      SELECT br.sequence, br.source_kind, br.source_attempt_id, br.summary, br.created_at,
             bm.case_id, bc.name, bm.metric_id, md.unit, md.direction,
             bm.target_value, bm.development_value, bm.noise_tolerance
      FROM best_metrics bm
      JOIN best_revisions br ON br.id = bm.best_revision_id
      JOIN benchmark_cases bc
        ON bc.baseline_revision_id = br.baseline_revision_id AND bc.case_id = bm.case_id
      JOIN metric_definitions md
        ON md.baseline_revision_id = br.baseline_revision_id AND md.metric_id = bm.metric_id
      WHERE br.optimization_id = ?
      ORDER BY br.sequence, bm.case_id, bm.metric_id
      """,
      [@optimization_id]
    ).rows
    |> Enum.map(fn [
                     revision,
                     source_kind,
                     attempt_id,
                     summary,
                     measured_at,
                     case_id,
                     case_name,
                     metric_id,
                     unit,
                     direction,
                     target_value,
                     value,
                     noise_tolerance
                   ] ->
      point(
        case_id,
        case_name,
        metric_id,
        unit,
        direction,
        target_value,
        value,
        noise_tolerance,
        measured_at,
        attempt_id,
        if(source_kind == "baseline", do: "baseline", else: "best"),
        if(source_kind == "baseline", do: "baseline", else: "accepted"),
        summary,
        revision
      )
    end)
  end

  defp attempt_points do
    Repo.query!(
      """
      SELECT a.id, a.status, a.summary, a.updated_at,
             am.case_id, bc.name, am.metric_id, md.unit, md.direction,
             am.target_value, am.candidate_value, am.noise_tolerance
      FROM attempt_metrics am
      JOIN attempts a ON a.id = am.attempt_id
      JOIN sampling_revisions sr ON sr.id = a.sampling_revision_id
      JOIN benchmark_cases bc
        ON bc.baseline_revision_id = sr.baseline_revision_id AND bc.case_id = am.case_id
      JOIN metric_definitions md
        ON md.baseline_revision_id = sr.baseline_revision_id AND md.metric_id = am.metric_id
      WHERE a.optimization_id = ?
      ORDER BY a.id, am.case_id, am.metric_id
      """,
      [@optimization_id]
    ).rows
    |> Enum.map(fn [
                     attempt_id,
                     status,
                     summary,
                     measured_at,
                     case_id,
                     case_name,
                     metric_id,
                     unit,
                     direction,
                     target_value,
                     value,
                     noise_tolerance
                   ] ->
      point(
        case_id,
        case_name,
        metric_id,
        unit,
        direction,
        target_value,
        value,
        noise_tolerance,
        measured_at,
        attempt_id,
        "iteration",
        status,
        summary,
        nil
      )
    end)
  end

  defp point(
         case_id,
         case_name,
         metric_id,
         unit,
         direction,
         target_value,
         value,
         noise_tolerance,
         measured_at,
         attempt_id,
         source,
         status,
         summary,
         best_revision
       ) do
    %{
      case_id: case_id,
      case_name: case_name,
      metric_id: metric_id,
      unit: unit,
      direction: direction,
      target_value: target_value,
      value: value,
      improvement_ratio: relative_improvement(direction, target_value, value),
      noise_tolerance: noise_tolerance,
      measured_at: measured_at,
      attempt_id: attempt_id,
      source: source,
      status: status,
      summary: summary || "No summary yet",
      best_revision: best_revision
    }
  end

  defp relative_improvement(_direction, target, _value) when target == 0 or is_nil(target),
    do: nil

  defp relative_improvement("maximize", target, value), do: (value - target) / target
  defp relative_improvement(_minimize, target, value), do: (target - value) / target

  defp catalog(points, id_key, label_key) do
    points
    |> Enum.map(&%{id: Map.fetch!(&1, id_key), label: Map.fetch!(&1, label_key)})
    |> Enum.uniq_by(& &1.id)
    |> Enum.sort_by(& &1.id)
  end

  defp point_order(point) do
    source_order = %{"baseline" => 0, "iteration" => 1, "best" => 2}
    {point.measured_at, Map.fetch!(source_order, point.source), point.case_id, point.metric_id}
  end
end
