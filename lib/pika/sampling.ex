defmodule Pika.Sampling do
  @moduledoc false

  def initial(spec, args) when is_map(spec) and is_map(args) do
    case_ids = args["case_ids"]
    reasons = args["reasons"]
    estimated_cost = args["estimated_cost"]
    summary = args["summary"]
    known_cases = Map.new(spec["benchmark_cases"] || [], &{&1["id"], &1})
    max_cases = get_in(spec, ["iteration_sampling", "max_initial_cases"]) || 10

    cond do
      not is_list(case_ids) or case_ids == [] ->
        {:error, :empty_iteration_sample}

      case_ids != Enum.uniq(case_ids) ->
        {:error, :duplicate_iteration_sample_case}

      length(case_ids) > max_cases ->
        {:error, {:too_many_initial_sample_cases, max_cases}}

      Enum.any?(case_ids, &(not Map.has_key?(known_cases, &1))) ->
        {:error, :unknown_iteration_sample_case}

      not Enum.any?(case_ids, &(get_in(known_cases, [&1, "kind"]) == "target")) ->
        {:error, :target_case_required}

      not valid_reasons?(reasons, case_ids) ->
        {:error, :invalid_iteration_sample_reasons}

      not valid_cost?(estimated_cost) ->
        {:error, :invalid_iteration_sample_cost}

      not present?(summary) ->
        {:error, :iteration_sample_summary_required}

      true ->
        {:ok,
         %{
           revision: 1,
           cause: "baseline",
           case_ids: case_ids,
           reasons: Map.take(reasons, case_ids),
           estimated_cost: estimated_cost,
           summary: summary,
           created_at: DateTime.utc_now()
         }}
    end
  end

  defp valid_reasons?(reasons, case_ids) when is_map(reasons) do
    Enum.all?(case_ids, &present?(reasons[&1]))
  end

  defp valid_reasons?(_reasons, _case_ids), do: false

  defp valid_cost?(cost) when is_map(cost) do
    nonnegative_number?(cost["iteration_seconds"]) and
      positive_number?(cost["full_seconds"]) and
      is_number(cost["savings_ratio"]) and cost["savings_ratio"] >= 0 and
      cost["savings_ratio"] <= 1
  end

  defp valid_cost?(_cost), do: false

  defp present?(value), do: is_binary(value) and String.trim(value) != ""
  defp nonnegative_number?(value), do: is_number(value) and value >= 0
  defp positive_number?(value), do: is_number(value) and value > 0
end
