defmodule PikaWeb.CoreComponents do
  @moduledoc false

  use Phoenix.Component

  attr :kind, :string, default: "neutral"
  slot :inner_block, required: true

  def pill(assigns) do
    ~H"""
    <span class={"pill pill-#{@kind}"}>{render_slot(@inner_block)}</span>
    """
  end

  attr :field, Phoenix.HTML.FormField, required: true
  attr :type, :string, default: "text"
  attr :placeholder, :string, default: nil

  def input(%{type: "textarea"} = assigns) do
    ~H"""
    <textarea id={@field.id} name={@field.name} placeholder={@placeholder}>{Phoenix.HTML.Form.normalize_value("textarea", @field.value)}</textarea>
    """
  end

  def input(assigns) do
    ~H"""
    <input id={@field.id} name={@field.name} type={@type} value={Phoenix.HTML.Form.normalize_value(@type, @field.value)} placeholder={@placeholder} />
    """
  end
end
