ExUnit.start(
  exclude: if(System.get_env("PIKA_EXTERNAL_CONFORMANCE") == "1", do: [], else: [:external])
)

Code.require_file("support/alignment_fixtures.ex", __DIR__)
Code.require_file("support/campaign_fixtures.ex", __DIR__)
Code.require_file("support/conn_case.ex", __DIR__)
Code.require_file("support/alignment_agent_backend.ex", __DIR__)
Code.require_file("support/optimization_fixtures.ex", __DIR__)
Code.require_file("support/attempt_agent_backend.ex", __DIR__)
Code.require_file("support/integration_agent_backend.ex", __DIR__)
Code.require_file("support/sync_agent_backend.ex", __DIR__)
