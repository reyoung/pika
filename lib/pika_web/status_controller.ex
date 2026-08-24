defmodule PikaWeb.StatusController do
  use PikaWeb, :controller

  def show(conn, _params), do: json(conn, snapshot())

  def events(conn, _params) do
    payload = Jason.encode!(%{event: "snapshot", data: snapshot()})

    conn =
      conn
      |> put_resp_header("cache-control", "no-cache")
      |> put_resp_content_type("text/event-stream")
      |> send_chunked(200)

    {:ok, conn} = chunk(conn, "event: snapshot\ndata: #{payload}\n\n")
    conn
  end

  defp snapshot do
    [[cursor]] = Pika.Repo.query!("SELECT COALESCE(MAX(id), 0) FROM conversation_turns").rows
    Pika.ProgressSummary.Snapshot.build(cursor, DateTime.utc_now())
  end
end
