ExUnit.start(
  exclude: if(System.get_env("PIKA_EXTERNAL_CONFORMANCE") == "1", do: [], else: [:external])
)
