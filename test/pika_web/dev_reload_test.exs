defmodule PikaWeb.DevReloadTest do
  use PikaWeb.ConnCase, async: false

  alias Pika.Auth

  setup do
    root = Path.join(System.tmp_dir!(), "pika-web-reload-#{System.unique_integer([:positive])}")
    source = Path.join(root, "lib/example.ex")
    File.mkdir_p!(Path.dirname(source))
    File.write!(source, "defmodule Example, do: :ok\n")

    previous_reload = Application.get_env(:pika, :dev_reload)
    previous_root = Application.get_env(:pika, :dev_reload_root)

    Auth.clear()
    %{token: token} = Auth.generate()
    Application.put_env(:pika, :dev_reload, false)
    Application.put_env(:pika, :dev_reload_root, root)

    on_exit(fn ->
      Auth.clear()
      restore_env(:dev_reload, previous_reload)
      restore_env(:dev_reload_root, previous_root)
      File.rm_rf!(root)
    end)

    %{root: root, token: token}
  end

  test "serves an authenticated, uncached source fingerprint", %{
    conn: conn,
    root: root,
    token: token
  } do
    conn = get(conn, "/?token=#{token}")

    conn =
      conn
      |> recycle()
      |> put_req_header("accept", "application/json")
      |> get("/__pika_reload")

    assert %{"fingerprint" => fingerprint} = json_response(conn, 200)
    assert fingerprint == Pika.DevReload.fingerprint(root)
    assert get_resp_header(conn, "cache-control") == ["no-store"]
  end

  test "loads the polling client only when reload is enabled" do
    Application.put_env(:pika, :dev_reload, true)
    html = render_component(&PikaWeb.Layouts.root/1, inner_content: "content")
    assert html =~ ~s(src="/assets/dev_reload.js")

    Application.put_env(:pika, :dev_reload, false)
    html = render_component(&PikaWeb.Layouts.root/1, inner_content: "content")
    refute html =~ ~s(src="/assets/dev_reload.js")
  end

  defp restore_env(key, nil), do: Application.delete_env(:pika, key)
  defp restore_env(key, value), do: Application.put_env(:pika, key, value)
end
