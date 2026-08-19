ExUnit.start(
  exclude: if(System.get_env("PIKA_EXTERNAL_CONFORMANCE") == "1", do: [], else: [:external])
)

Code.require_file("support/stage0_fixtures.ex", __DIR__)
Code.require_file("support/conn_case.ex", __DIR__)
Code.require_file("support/stage0_agent_backend.ex", __DIR__)
