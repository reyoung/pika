defmodule Pika.Repo do
  use Ecto.Repo,
    otp_app: :pika,
    adapter: Ecto.Adapters.SQLite3
end
