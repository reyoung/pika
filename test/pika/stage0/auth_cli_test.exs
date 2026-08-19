defmodule Pika.Stage0.AuthCLITest do
  use ExUnit.Case, async: false

  alias Pika.CLI
  alias Pika.Stage0.Auth

  setup do
    Auth.clear()
    on_exit(&Auth.clear/0)
    :ok
  end

  test "stores only a token hash marker and invalidates the old startup token" do
    first = Auth.generate()
    refute first.token == first.marker
    assert {:ok, first.marker} == Auth.authenticate(first.token)
    assert Auth.authenticated_marker?(first.marker)

    second = Auth.generate()
    assert {:error, :unauthorized} = Auth.authenticate(first.token)
    assert {:ok, second.marker} == Auth.authenticate(second.token)
  end

  test "parses the Stage0 public CLI options with safe defaults" do
    assert {:ok, opts} =
             CLI.parse_stage0([
               "--backend",
               "cursor",
               "--effort",
               "xhigh",
               "--port",
               "8080",
               "--skill-root",
               "/tmp/a",
               "--skill-root",
               "/tmp/b"
             ])

    assert opts[:backend] == "cursor"
    assert opts[:effort] == "xhigh"
    assert opts[:port] == 8080
    assert Keyword.get_values(opts, :skill_root) == ["/tmp/a", "/tmp/b"]
    assert {:error, _} = CLI.parse_stage0(["--backend", "unknown"])
    assert {:error, _} = CLI.parse_stage0(["--effort", "infinite"])
  end
end
