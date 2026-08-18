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
end
