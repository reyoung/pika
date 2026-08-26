defmodule Pika.ModelCatalogTest do
  use ExUnit.Case, async: true

  alias Pika.ModelCatalog

  test "parses Cursor's provider-reported model list" do
    output = """
    Available models

    auto - Auto (default)
    gpt-test - GPT Test
    claude-test-thinking - Claude Test Thinking
    """

    assert {:ok, models} =
             ModelCatalog.list("cursor_acp", cursor_runner: fn -> {:ok, output} end)

    assert models == [
             %{id: "gpt-test", label: "GPT Test", description: nil},
             %{
               id: "claude-test-thinking",
               label: "Claude Test Thinking",
               description: nil
             }
           ]

    assert {:ok, ^models} =
             ModelCatalog.list("cursor_headless", cursor_runner: fn -> {:ok, output} end)
  end

  test "normalizes Codex models and places the provider default first" do
    provider_models = [
      %{
        "id" => "hidden",
        "model" => "hidden",
        "displayName" => "Hidden",
        "description" => "hidden",
        "hidden" => true,
        "isDefault" => false
      },
      %{
        "id" => "other",
        "model" => "other",
        "displayName" => "Other",
        "description" => "other model",
        "hidden" => false,
        "isDefault" => false
      },
      %{
        "id" => "default",
        "model" => "default",
        "displayName" => "Default",
        "description" => "default model",
        "hidden" => false,
        "isDefault" => true
      }
    ]

    assert {:ok, [default, other]} =
             ModelCatalog.list("codex_app_server",
               codex_lister: fn -> {:ok, provider_models} end
             )

    assert default.id == "default"
    assert default.default
    assert other.id == "other"
  end
end
