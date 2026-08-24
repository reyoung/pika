defmodule PikaWeb.ControlController do
  use PikaWeb, :controller

  alias Pika.Attempt.Scheduler
  alias Pika.Optimization.Runtime
  alias Pika.ProgressSummary.Snapshot
  alias Pika.Repo

  def show(conn, _params), do: json(conn, snapshot())
  def attempts(conn, _params), do: json(conn, %{attempts: snapshot().attempts})

  def attempt(conn, %{"id" => id}) do
    with {attempt_id, ""} <- Integer.parse(id),
         attempt when is_map(attempt) <- Enum.find(snapshot().attempts, &(&1.id == attempt_id)) do
      json(conn, attempt)
    else
      _other -> error(conn, 404, :attempt_not_found)
    end
  end

  def metrics(conn, _params), do: json(conn, %{best: snapshot().best})

  def events(conn, params) do
    after_sequence = integer(params["after"], 0)
    limit = integer(params["limit"], 500) |> min(2_000) |> max(1)

    events =
      Repo.query!(
        """
        SELECT sequence, event_id, aggregate_type, aggregate_id, event_type, payload_json, created_at
        FROM domain_events WHERE optimization_id = 'optimization' AND sequence > ?
        ORDER BY sequence LIMIT ?
        """,
        [after_sequence, limit]
      ).rows
      |> Enum.map(fn [
                       sequence,
                       event_id,
                       aggregate_type,
                       aggregate_id,
                       event_type,
                       payload,
                       created_at
                     ] ->
        %{
          sequence: sequence,
          event_id: event_id,
          aggregate_type: aggregate_type,
          aggregate_id: aggregate_id,
          event_type: event_type,
          payload: Jason.decode!(payload),
          created_at: created_at
        }
      end)

    json(conn, %{events: events})
  end

  def pause(conn, params), do: control(conn, params, &Runtime.pause/0)
  def resume(conn, params), do: control(conn, params, &Runtime.resume/0)
  def stop(conn, params), do: control(conn, params, fn -> Runtime.stop_now("api_requested") end)

  def create_btw(conn, _params_with_attempt_id = params) do
    with {:ok, _key} <- idempotency_key(conn, params),
         {:ok, guidance} <- Scheduler.add_guidance(params["body"] || "") do
      conn |> put_status(201) |> json(guidance)
    else
      {:error, reason} -> error(conn, 409, reason)
    end
  end

  defp control(conn, params, callback) do
    with {:ok, _key} <- idempotency_key(conn, params),
         {:ok, optimization} <- callback.() do
      json(conn, optimization)
    else
      {:error, reason} -> error(conn, 409, reason)
    end
  end

  defp snapshot do
    [[cursor]] = Repo.query!("SELECT COALESCE(MAX(id), 0) FROM conversation_turns").rows
    Snapshot.build(cursor, DateTime.utc_now())
  end

  defp idempotency_key(conn, params) do
    key = get_req_header(conn, "idempotency-key") |> List.first() || params["idempotency_key"]
    if is_binary(key) and key != "", do: {:ok, key}, else: {:error, :idempotency_key_required}
  end

  defp integer(nil, default), do: default

  defp integer(value, default) do
    case Integer.parse(to_string(value)) do
      {number, ""} -> number
      _other -> default
    end
  end

  defp error(conn, status, reason),
    do: conn |> put_status(status) |> json(%{error: Pika.JSONSafe.json_safe(reason)})
end
