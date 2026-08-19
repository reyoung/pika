defmodule PikaWeb.MarkdownTest do
  use ExUnit.Case, async: true

  alias PikaWeb.Markdown

  test "renders conversational Markdown while blocking raw HTML and unsafe links" do
    rendered =
      """
      ## Result

      - `latency_us`: **38.2**
      - [unsafe](javascript:alert(1))

      <script>alert("xss")</script>
      """
      |> Markdown.render()
      |> Phoenix.HTML.Safe.to_iodata()
      |> IO.iodata_to_binary()

    assert rendered =~ "<h2>Result</h2>"
    assert rendered =~ "<strong>38.2</strong>"
    refute rendered =~ ~s(href="javascript:)
    refute rendered =~ "<script>"
  end
end
