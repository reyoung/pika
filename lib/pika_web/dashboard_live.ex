defmodule PikaWeb.DashboardLive do
  use PikaWeb, :live_view

  @impl true
  def mount(_params, session, socket) do
    if Pika.Auth.authenticated_marker?(session["pika_auth"]) do
      snapshot = Pika.Runtime.snapshot()

      if connected?(socket) do
        Phoenix.PubSub.subscribe(Pika.PubSub, Pika.Persistence.topic(snapshot.campaign.id))
      end

      {:ok, assign(socket, :snapshot, snapshot)}
    else
      {:ok, redirect(socket, to: "/")}
    end
  end

  @impl true
  def handle_info({:domain_event, _event}, socket),
    do: {:noreply, assign(socket, :snapshot, Pika.Runtime.snapshot())}

  @impl true
  def render(assigns) do
    ~H"""
    <main class="phase1-shell">
      <header class="topbar">
        <div class="brand">
          <div class="brand-mark">P</div>
          <div><strong>Pika Server</strong><small>Workspace persistence &amp; recovery</small></div>
        </div>
        <.pill kind={if @snapshot.campaign.status == "blocked", do: "error", else: "active"}>
          {@snapshot.campaign.status}
        </.pill>
      </header>

      <section class="diagnostic-hero panel">
        <div>
          <p class="eyebrow">Campaign singleton</p>
          <h1>{@snapshot.campaign.id}</h1>
          <p class="muted">{@snapshot.workspace.root}</p>
        </div>
        <dl class="identity-grid">
          <div><dt>Repo mode</dt><dd>{@snapshot.workspace.mode}</dd></div>
          <div><dt>Recovery</dt><dd>{recovery_label(@snapshot.recovery)}</dd></div>
          <div><dt>Base SHA</dt><dd><code>{short_sha(@snapshot.campaign.base_sha)}</code></dd></div>
          <div><dt>Best SHA</dt><dd><code>{short_sha(@snapshot.campaign.best_sha)}</code></dd></div>
          <div><dt>Best branch</dt><dd>{@snapshot.campaign.best_branch}</dd></div>
          <div><dt>Config SHA-256</dt><dd><code>{short_sha(@snapshot.workspace.config_hash)}</code></dd></div>
        </dl>
      </section>

      <div class="diagnostic-grid">
        <section class="panel diagnostic-panel">
          <div class="panel-heading"><div><p class="eyebrow">Startup diagnostics</p><h2>Preflight</h2></div></div>
          <div class="diagnostic-list">
            <article :for={check <- @snapshot.preflight} class="diagnostic-row">
              <span class={"diagnostic-dot diagnostic-#{check.status}"}></span>
              <div><strong>{check.label}</strong><small>{check.detail}</small></div>
              <span class="phase-label">{check.phase}</span>
            </article>
          </div>
        </section>

        <section class="panel diagnostic-panel">
          <div class="panel-heading"><div><p class="eyebrow">Durable state</p><h2>SQLite</h2></div></div>
          <dl class="sqlite-list">
            <div :for={{name, value} <- @snapshot.sqlite}><dt>{name}</dt><dd>{value}</dd></div>
          </dl>
          <p class="diagnostic-note">
            Workspace and database diagnostics remain available when a later Agent or GPU phase is unavailable.
          </p>
        </section>
      </div>
    </main>
    """
  end

  defp recovery_label(:initialized), do: "initialized"
  defp recovery_label(:recovered), do: "recovered"
  defp recovery_label("initialized"), do: "initialized"
  defp recovery_label("recovered"), do: "recovered"
  defp recovery_label(%{"status" => "blocked", "reason" => reason}), do: "blocked: #{reason}"
  defp recovery_label({:blocked, reason}), do: "blocked: #{inspect(reason)}"
  defp recovery_label(value), do: inspect(value)

  defp short_sha(nil), do: "—"
  defp short_sha(value) when byte_size(value) > 16, do: String.slice(value, 0, 16)
  defp short_sha(value), do: value
end
