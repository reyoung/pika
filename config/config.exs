import Config

config :logger, :console,
  format: "$time $metadata[$level] $message\n",
  metadata: [:request_id]

config :phoenix, :json_library, Jason

config :pika, PikaWeb.Endpoint,
  adapter: Bandit.PhoenixAdapter,
  http: [ip: {127, 0, 0, 1}, port: 0],
  secret_key_base: String.duplicate("pika-phase-0-", 8),
  server: false

import_config "#{config_env()}.exs"
