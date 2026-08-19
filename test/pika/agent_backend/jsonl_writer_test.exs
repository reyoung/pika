defmodule Pika.AgentBackend.JSONLWriterTest do
  use ExUnit.Case, async: true

  alias Pika.AgentBackend.JSONLWriter

  test "redacts authorization values and replays only complete records" do
    path = Path.join(System.tmp_dir!(), "pika-jsonl-#{System.unique_integer([:positive])}.jsonl")

    JSONLWriter.append(path, "out", %{
      "headers" => [%{"name" => "Authorization", "value" => "Bearer secret"}],
      "token" => "also-secret"
    })

    File.write!(path, "{partial", [:append])
    [record] = JSONLWriter.replay(path)
    assert get_in(record, ["payload", "headers", Access.at(0), "value"]) == "[REDACTED]"
    assert get_in(record, ["payload", "token"]) == "[REDACTED]"
    refute File.read!(path) =~ "Bearer secret"
  end

  test "redacts secrets embedded in command output strings and can scrub an existing log" do
    path =
      Path.join(
        System.tmp_dir!(),
        "pika-jsonl-secret-#{System.unique_integer([:positive])}.jsonl"
      )

    JSONLWriter.append(path, "in", %{
      "output" =>
        "DB_URL=postgres://alice:password@example.test/db API_TOKEN=top-secret Bearer abc.def.ghi"
    })

    contents = File.read!(path)
    refute contents =~ "password"
    refute contents =~ "top-secret"
    refute contents =~ "abc.def.ghi"
    assert contents =~ "[REDACTED]"

    File.write!(
      path,
      Jason.encode!(%{
        "direction" => "in",
        "payload" => %{"output" => "https://user:secret@example.test"}
      }) <> "\n"
    )

    assert :ok = JSONLWriter.scrub_file(path)
    refute File.read!(path) =~ "user:secret"
  end
end
