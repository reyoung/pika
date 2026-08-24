ExUnit.start(
  exclude: if(System.get_env("PIKA_EXTERNAL_CONFORMANCE") == "1", do: [], else: [:external])
)

Code.require_file("support/conn_case.ex", __DIR__)
