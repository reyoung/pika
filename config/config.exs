import Config

config :logger, :console,
  format: "$time $metadata[$level] $message\n",
  metadata: [:request_id]

config :phoenix, :json_library, Jason

config :esbuild,
  version: "0.25.9",
  default: [
    args:
      ~w(js/app.js --bundle --target=es2022 --outdir=../priv/static/assets --external:/fonts/* --external:/images/*),
    cd: Path.expand("../assets", __DIR__),
    env: %{"NODE_PATH" => [Path.expand("../deps", __DIR__)]}
  ]

config :pika, PikaWeb.Endpoint,
  adapter: Bandit.PhoenixAdapter,
  http: [ip: {127, 0, 0, 1}, port: 0],
  live_view: [signing_salt: "pika-stage0-live"],
  pubsub_server: Pika.PubSub,
  render_errors: [formats: [html: PikaWeb.ErrorHTML, json: PikaWeb.ErrorJSON], layout: false],
  secret_key_base: String.duplicate("pika-stage0-demo-", 8),
  server: false

config :pika, Pika.Stage0.PromptCatalog,
  alignment: {:priv, "prompts/stage0/alignment.md.eex"},
  setup_merge: {:priv, "prompts/stage0/setup_merge.md.eex"},
  baseline: {:priv, "prompts/stage0/baseline.md.eex"}

import_config "#{config_env()}.exs"
