import Config

config :logger, level: :warning

config :pika, PikaWeb.Endpoint,
  server: false,
  secret_key_base: String.duplicate("pika-stage0-test-", 8)
