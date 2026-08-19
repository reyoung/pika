defmodule Pika.Stage0.SamplingTest do
  use ExUnit.Case, async: true

  alias Pika.Stage0.Sampling
  alias Pika.Test.Stage0Fixtures

  test "accepts a non-empty initial sample with a Target Case and cost evidence" do
    assert {:ok, sampling} =
             Sampling.initial(Stage0Fixtures.spec(), %{
               "case_ids" => ["target_case"],
               "reasons" => %{"target_case" => "highest weighted target"},
               "estimated_cost" => %{
                 "iteration_seconds" => 2.0,
                 "full_seconds" => 10.0,
                 "savings_ratio" => 0.8
               },
               "summary" => "representative initial sample"
             })

    assert sampling.revision == 1
    assert sampling.cause == "baseline"
    assert sampling.case_ids == ["target_case"]
  end

  test "rejects more than ten initial Cases" do
    cases =
      for index <- 1..11 do
        %{
          "id" => "case_#{index}",
          "name" => "Case #{index}",
          "kind" => "target",
          "shape" => %{"n" => index},
          "dtype" => "float16",
          "layout" => "contiguous"
        }
      end

    spec = %{Stage0Fixtures.spec() | "benchmark_cases" => cases}
    ids = Enum.map(cases, & &1["id"])

    assert {:error, {:too_many_initial_sample_cases, 10}} =
             Sampling.initial(spec, %{
               "case_ids" => ids,
               "reasons" => Map.new(ids, &{&1, "reason"}),
               "estimated_cost" => %{
                 "iteration_seconds" => 10.0,
                 "full_seconds" => 11.0,
                 "savings_ratio" => 0.09
               },
               "summary" => "too large"
             })
  end

  test "requires at least one Target Case and a reason for every selection" do
    [target] = Stage0Fixtures.spec()["benchmark_cases"]
    guard = %{target | "id" => "guard_case", "kind" => "guard"}
    spec = %{Stage0Fixtures.spec() | "benchmark_cases" => [guard]}

    assert {:error, :target_case_required} =
             Sampling.initial(spec, %{
               "case_ids" => ["guard_case"],
               "reasons" => %{"guard_case" => "guard"},
               "estimated_cost" => %{
                 "iteration_seconds" => 1,
                 "full_seconds" => 1,
                 "savings_ratio" => 0
               },
               "summary" => "guard only"
             })
  end
end
