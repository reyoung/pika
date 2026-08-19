defmodule Pika.CampaignStoreTest do
  use ExUnit.Case, async: false

  test "safe snapshot decoding works in a fresh BEAM before Campaign atoms are loaded" do
    durable = %{
      status: :building_baseline,
      messages: [
        %{
          id: "message-1",
          role: :agent,
          content: "fixture",
          attachments: [],
          at: DateTime.utc_now(),
          kind: :question
        }
      ],
      required: MapSet.new(["submit_baseline"]),
      backend_workflow: :baseline
    }

    encoded = durable |> :erlang.term_to_binary(compressed: 6) |> Base.encode64()

    script = """
    blob = Base.decode64!(#{inspect(encoded)})

    cold_decode_rejected =
      try do
        :erlang.binary_to_term(blob, [:safe])
        false
      rescue
        ArgumentError -> true
      end

    unless cold_decode_rejected, do: System.halt(2)

    case Pika.CampaignStore.decode_snapshot(blob) do
      {:ok, durable} when is_map(durable) -> IO.write("decoded")
      other -> IO.inspect(other); System.halt(3)
    end
    """

    code_paths =
      Path.expand("../../_build/test/lib/*/ebin", __DIR__)
      |> Path.wildcard()
      |> Enum.flat_map(&["-pa", &1])

    assert {"decoded", 0} =
             System.cmd(System.find_executable("elixir"), code_paths ++ ["-e", script],
               stderr_to_stdout: true
             )
  end
end
