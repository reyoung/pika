defmodule PikaWeb.Layouts do
  use PikaWeb, :html

  attr(:inner_content, :any, required: true)

  def root(assigns) do
    ~H"""
    <!doctype html>
    <html lang="zh-CN">
      <head>
        <meta charset="utf-8" />
        <meta name="viewport" content="width=device-width, initial-scale=1" />
        <meta name="csrf-token" content={Plug.CSRFProtection.get_csrf_token()} />
        <title>Pika</title>
        <link rel="icon" href="/favicon.svg" type="image/svg+xml" />
        <link phx-track-static rel="stylesheet" href="/assets/app.css" />
        <script defer phx-track-static type="text/javascript" src="/assets/app.js"></script>
        <script
          defer
          phx-track-static
          type="text/javascript"
          src="/assets/keyboard_shortcuts.js"
        >
        </script>
        <script
          :if={Application.get_env(:pika, :dev_reload, false)}
          defer
          type="text/javascript"
          src="/assets/dev_reload.js"
        >
        </script>
      </head>
      <body>
        {@inner_content}
      </body>
    </html>
    """
  end

  attr(:inner_content, :any, required: true)

  def app(assigns) do
    ~H"""
    {@inner_content}
    """
  end
end
