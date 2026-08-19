defmodule PikaWeb.Markdown do
  @moduledoc false

  import Phoenix.HTML, only: [raw: 1]

  def render(markdown) when is_binary(markdown) do
    markdown
    |> MDEx.to_html!(
      extension: [autolink: true, strikethrough: true, table: true],
      render: [escape: true],
      sanitize: MDEx.Document.default_sanitize_options()
    )
    |> raw()
  end

  def render(_value), do: raw("")
end
