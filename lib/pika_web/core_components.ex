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

  attr :console, :map, default: nil

  def command_console(assigns) do
    ~H"""
    <section
      :if={@console}
      id="command-console"
      class="command-console"
      phx-hook="CommandConsole"
      role="region"
      aria-label="命令输出 Console"
    >
      <div class="command-console-resize" data-console-resize role="separator" aria-orientation="horizontal" tabindex="0"></div>
      <header>
        <div class="command-console-title">
          <span class={"command-console-state state-#{@console.status}"}></span>
          <div>
            <strong>Console</strong>
            <code title={@console.command || ""}>{@console.command || "等待命令信息…"}</code>
          </div>
        </div>
        <div class="command-console-meta">
          <span>{@console.backend}</span>
          <span :if={@console.cwd} title={@console.cwd}>{@console.cwd}</span>
          <span :if={not is_nil(@console.exit_code)}>exit {@console.exit_code}</span>
          <span :if={not is_nil(@console.duration_ms)}>{@console.duration_ms} ms</span>
        </div>
        <div class="command-console-actions">
          <button type="button" data-console-latest title="滚动到底部">↓</button>
          <button type="button" data-console-collapse title="收起 Console">—</button>
          <button type="button" phx-click="close_command_console" title="关闭 Console" aria-label="关闭 Console">×</button>
        </div>
      </header>
      <pre data-console-output tabindex="0">{@console.output}</pre>
    </section>
    """
  end
end
