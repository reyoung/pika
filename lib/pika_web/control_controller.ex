defmodule PikaWeb.ControlController do
  use PikaWeb, :controller

  def show(conn, _params), do: json(conn, Pika.Dashboard.snapshot())

  def attempts(conn, _params) do
    dashboard = Pika.Dashboard.snapshot()
    json(conn, %{attempts: dashboard.attempts})
  end

  def attempt(conn, %{"id" => id}) do
    campaign_id = Pika.Persistence.current_campaign().id

    case Pika.Dashboard.attempt(campaign_id, id) do
      {:ok, attempt} -> json(conn, attempt)
      {:error, reason} -> error(conn, 404, reason)
    end
  end

  def metrics(conn, _params) do
    dashboard = Pika.Dashboard.snapshot()
    json(conn, %{metrics: dashboard.metrics, spec: dashboard.spec})
  end

  def events(conn, params) do
    after_sequence = integer(params["after"], 0)
    limit = integer(params["limit"], 500) |> min(2_000) |> max(1)
    campaign_id = Pika.Persistence.current_campaign().id
    json(conn, %{events: Pika.AttemptStore.events(campaign_id, after_sequence, limit)})
  end

  def sync_preview(conn, params) do
    with remote when is_binary(remote) <- params["remote"],
         branch when is_binary(branch) <- params["branch"],
         {:ok, preview} <- Pika.SyncCoordinator.preview(remote, branch) do
      json(conn, preview)
    else
      nil -> error(conn, 422, :missing_sync_config)
      {:error, reason} -> error(conn, 409, reason)
    end
  end

  def pause(conn, params), do: control(conn, params, &Pika.Control.pause/1)
  def stop(conn, params), do: control(conn, params, &Pika.Control.stop_now/1)
  def resume(conn, params), do: control(conn, params, &Pika.Control.resume/1)

  def request_sync(conn, params) do
    with {:ok, key} <- idempotency_key(conn, params),
         remote when is_binary(remote) and remote != "" <- params["remote"],
         branch when is_binary(branch) and branch != "" <- params["branch"],
         {:ok, run} <- Pika.SyncCoordinator.request(remote, branch, key) do
      conn |> put_status(202) |> json(run)
    else
      nil -> error(conn, 422, :missing_sync_config)
      {:error, reason} -> error(conn, 409, reason)
    end
  end

  def confirm_sync_spec(conn, %{"id" => run_id} = params) do
    with {:ok, key} <- idempotency_key(conn, params),
         approved when is_boolean(approved) <- params["approved"],
         {:ok, run} <- Pika.SyncCoordinator.confirm_spec(run_id, approved, key) do
      json(conn, run)
    else
      nil -> error(conn, 422, :approved_required)
      {:error, reason} -> error(conn, 409, reason)
    end
  end

  def create_btw(conn, %{"id" => attempt_id} = params) do
    with {:ok, key} <- idempotency_key(conn, params),
         {:ok, guidance} <-
           Pika.AttemptCoordinator.create_btw(
             attempt_id,
             params["body"],
             params["mode"] || "chat",
             key
           ) do
      conn |> put_status(201) |> json(guidance)
    else
      {:error, reason} -> error(conn, 409, reason)
    end
  end

  defp control(conn, params, fun) do
    with {:ok, key} <- idempotency_key(conn, params),
         {:ok, campaign} <- fun.(key) do
      json(conn, campaign)
    else
      {:error, reason} -> error(conn, 409, reason)
    end
  end

  defp idempotency_key(conn, params) do
    key = get_req_header(conn, "idempotency-key") |> List.first() || params["idempotency_key"]
    if is_binary(key) and key != "", do: {:ok, key}, else: {:error, :idempotency_key_required}
  end

  defp integer(nil, default), do: default

  defp integer(value, default) do
    case Integer.parse(to_string(value)) do
      {number, ""} -> number
      _ -> default
    end
  end

  defp error(conn, status, reason),
    do: conn |> put_status(status) |> json(%{error: Pika.JSONSafe.json_safe(reason)})
end
