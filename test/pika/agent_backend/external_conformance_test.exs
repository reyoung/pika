defmodule Pika.AgentBackend.ExternalConformanceTest do
  use ExUnit.Case, async: false

  @moduletag :external
  @moduletag timeout: 600_000

  test "real Codex App Server and Cursor ACP satisfy the backend contract" do
    artifact_dir = Path.expand("artifacts/backend-conformance")

    assert {:ok, %{status: "passed", backends: backends}} =
             Pika.AgentBackend.Conformance.run(
               backends: [:codex_app_server, :cursor_acp],
               workspace: File.cwd!(),
               artifact_dir: artifact_dir,
               control_probes: true
             )

    assert Map.keys(backends) |> Enum.sort() == [:codex_app_server, :cursor_acp]
  end
end
