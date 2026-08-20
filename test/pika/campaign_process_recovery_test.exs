defmodule Pika.CampaignProcessRecoveryTest do
  use ExUnit.Case, async: false

  alias Pika.Test.{AlignmentFixtures, CampaignFixtures}

  @startup_timeout 30_000

  test "kill -9 recovery preserves the singleton and invalidates every old HTTP token path" do
    root = CampaignFixtures.workspace()
    File.rmdir!(root)
    port_number = Pika.MCP.ProbeServer.available_port()
    first = start_server(root, port_number)
    on_exit(fn -> stop_server(first) end)

    assert first.output =~ "(initialized)"
    assert {:ok, 302, first_headers, _body} = http_get(first.url)
    cookie = response_cookie(first_headers)
    assert is_binary(cookie)

    assert {:ok, 200, _headers, body} = bearer_get(port_number, "/api/status", first.token)
    first_status = Jason.decode!(body)
    campaign_id = first_status["campaign"]["id"]
    refute sqlite_storage_bytes(root) =~ first.token

    kill_9(first)
    first_port = first.port
    assert_receive {^first_port, {:exit_status, _status}}, 10_000

    second = start_server(root, port_number)
    on_exit(fn -> stop_server(second) end)
    assert second.output =~ "(recovered)"

    assert {:ok, 401, _headers, _body} = http_get(first.url)
    assert {:ok, 401, _headers, _body} = cookie_get(port_number, "/", cookie)
    assert {:ok, 401, _headers, _body} = bearer_get(port_number, "/api/status", first.token)
    assert {:ok, 401, _headers, _body} = mcp_initialize(port_number, first.token)

    assert {:ok, 200, _headers, second_body} =
             bearer_get(port_number, "/api/status", second.token)

    assert {:ok, 200, _headers, sse_body} = bearer_get(port_number, "/events", second.token)
    assert sse_body =~ "event: snapshot"

    second_status = Jason.decode!(second_body)
    assert second_status["campaign"]["id"] == campaign_id
    assert second_status["recovery"] == "recovered"
    assert second_status["workspace"]["mode"] == "owned_repo"
    refute sqlite_storage_bytes(root) =~ second.token

    assert {:ok, 200, _headers, _body} = mcp_initialize(port_number, second.token)
    assert {:ok, 302, second_headers, _body} = http_get(second.url)
    second_cookie = response_cookie(second_headers)
    assert {:ok, 200, _headers, html} = cookie_get(port_number, "/alignment", second_cookie)
    assert html =~ "Alignment → GPU Baseline"
    assert html =~ campaign_id

    assert singleton_count(root) == 1
  end

  test "a Managed Repo rejects a second Pika OS process and recovers after kill -9" do
    repo = AlignmentFixtures.git_repo()
    root = missing_workspace()
    port_number = Pika.MCP.ProbeServer.available_port()
    first = start_server(root, port_number, repo: repo)
    on_exit(fn -> stop_server(first) end)

    assert {:ok, 200, _headers, body} = bearer_get(port_number, "/api/status", first.token)
    first_status = Jason.decode!(body)
    assert first_status["workspace"]["mode"] == "managed_repo"
    campaign_id = first_status["campaign"]["id"]

    contender_root = missing_workspace()
    contender_port_number = Pika.MCP.ProbeServer.available_port()
    contender = open_server(contender_root, contender_port_number, repo: repo)
    {contender_output, contender_status} = await_exit(contender.port, "", @startup_timeout)
    assert contender_status == 2
    assert contender_output =~ "managed_repo_locked"
    refute File.exists?(contender_root)

    kill_9(first)
    first_port = first.port
    assert_receive {^first_port, {:exit_status, _status}}, 10_000

    recovered = start_server(root, port_number, repo: repo)
    on_exit(fn -> stop_server(recovered) end)
    assert recovered.output =~ "(recovered)"

    assert {:ok, 200, _headers, recovered_body} =
             bearer_get(port_number, "/api/status", recovered.token)

    recovered_status = Jason.decode!(recovered_body)
    assert recovered_status["campaign"]["id"] == campaign_id
    assert recovered_status["workspace"]["mode"] == "managed_repo"
    assert Pika.Git.clean?(repo)
    assert singleton_count(root) == 1
  end

  test "a corrupted registered Artifact restarts in Blocked while diagnostics stay available" do
    root = missing_workspace()
    port_number = Pika.MCP.ProbeServer.available_port()
    first = start_server(root, port_number)
    on_exit(fn -> stop_server(first) end)

    assert {:ok, 200, _headers, body} = bearer_get(port_number, "/api/status", first.token)
    campaign_id = Jason.decode!(body)["campaign"]["id"]
    kill_9(first)
    first_port = first.port
    assert_receive {^first_port, {:exit_status, _status}}, 10_000

    relative = "artifacts/logs/corrupted.jsonl"
    artifact_path = Path.join(root, relative)
    File.write!(artifact_path, "corrupted\n")
    insert_corrupt_artifact(root, campaign_id, relative)

    blocked = start_server(root, port_number)
    on_exit(fn -> stop_server(blocked) end)
    assert blocked.output =~ "(blocked)"

    assert {:ok, 200, _headers, blocked_body} =
             bearer_get(port_number, "/api/status", blocked.token)

    status = Jason.decode!(blocked_body)
    assert status["campaign"]["id"] == campaign_id
    assert status["campaign"]["status"] == "blocked"
    assert inspect(status["recovery"]) =~ "artifact_verification_failed"

    Process.sleep(300)
    assert campaign_status(root) == "blocked"

    assert {:ok, 302, blocked_headers, _body} = http_get(blocked.url)
    blocked_cookie = response_cookie(blocked_headers)
    assert {:ok, 200, _headers, html} = cookie_get(port_number, "/alignment", blocked_cookie)
    assert html =~ "Alignment Campaign 恢复已阻止"
  end

  test "a legacy setup Artifact is preserved under artifacts and recovery continues" do
    root = missing_workspace()
    port_number = Pika.MCP.ProbeServer.available_port()
    first = start_server(root, port_number)
    on_exit(fn -> stop_server(first) end)

    assert {:ok, 200, _headers, body} = bearer_get(port_number, "/api/status", first.token)
    campaign_id = Jason.decode!(body)["campaign"]["id"]
    kill_9(first)
    first_port = first.port
    assert_receive {^first_port, {:exit_status, _status}}, 10_000

    relative = "setup/1/reference_review_evidence.json"
    source = Path.join(root, relative)
    contents = "{\"latency_us\":12.5}\n"
    File.write!(source, contents)
    artifact_id = Ecto.UUID.generate()
    sha256 = :crypto.hash(:sha256, contents) |> Base.encode16(case: :lower)

    insert_artifact(
      root,
      campaign_id,
      artifact_id,
      relative,
      sha256,
      byte_size(contents),
      "reference_review_evidence",
      "application/json"
    )

    recovered = start_server(root, port_number)
    on_exit(fn -> stop_server(recovered) end)
    assert recovered.output =~ "(recovered)"

    migrated = "artifacts/recovered/#{artifact_id}/reference_review_evidence.json"
    assert artifact_path(root, artifact_id) == migrated
    assert File.read!(source) == contents
    assert File.read!(Path.join(root, migrated)) == contents

    assert {:ok, 200, _headers, recovered_body} =
             bearer_get(port_number, "/api/status", recovered.token)

    assert Jason.decode!(recovered_body)["recovery"] == "recovered"
  end

  test "recovery accepts an advanced Best descended from the historical Campaign Base" do
    root = missing_workspace()
    port_number = Pika.MCP.ProbeServer.available_port()
    first = start_server(root, port_number)
    on_exit(fn -> stop_server(first) end)

    assert {:ok, 200, _headers, body} = bearer_get(port_number, "/api/status", first.token)
    first_status = Jason.decode!(body)
    campaign_id = first_status["campaign"]["id"]
    historical_base = first_status["campaign"]["base_sha"]
    kill_9(first)
    first_port = first.port
    assert_receive {^first_port, {:exit_status, _status}}, 10_000

    repo = Path.join(root, "repo")
    Pika.Git.run!(repo, ["config", "user.name", "Pika Test"])
    Pika.Git.run!(repo, ["config", "user.email", "pika-test@example.invalid"])
    File.write!(Path.join(repo, "accepted.txt"), "accepted\n")
    Pika.Git.run!(repo, ["add", "accepted.txt"])
    Pika.Git.run!(repo, ["commit", "-m", "advance best"])
    advanced_best = Pika.Git.run!(repo, ["rev-parse", "HEAD"])

    assert advanced_best != historical_base

    assert {:ok, _} =
             Pika.Git.run(repo, ["merge-base", "--is-ancestor", historical_base, advanced_best])

    sqlite_execute(
      root,
      "UPDATE campaigns SET best_sha = ?, status = 'blocked', resume_state = 'optimizing' WHERE id = ?",
      [advanced_best, campaign_id]
    )

    sqlite_execute(root, "DELETE FROM campaign_runtime_snapshots WHERE campaign_id = ?", [
      campaign_id
    ])

    recovered = start_server(root, port_number)
    on_exit(fn -> stop_server(recovered) end)
    assert recovered.output =~ "(recovered)"

    assert {:ok, 200, _headers, recovered_body} =
             bearer_get(port_number, "/api/status", recovered.token)

    recovered_status = Jason.decode!(recovered_body)
    assert recovered_status["recovery"] == "recovered"
    assert recovered_status["campaign"]["status"] == "optimizing"
    assert recovered_status["campaign"]["base_sha"] == historical_base
    assert recovered_status["campaign"]["best_sha"] == advanced_best
  end

  defp start_server(root, port_number, opts \\ []) do
    server = open_server(root, port_number, opts)

    {output, url, token} =
      await_url(server.port, "", System.monotonic_time(:millisecond) + @startup_timeout)

    Map.merge(server, %{output: output, url: url, token: token})
  end

  defp open_server(root, port_number, opts) do
    executable = System.find_executable("mix") || raise "mix executable not found"
    project_root = File.cwd!()

    args =
      [
        "run",
        "--no-compile",
        "--no-start",
        "-e",
        "Pika.CLI.main(System.argv())",
        "--",
        "serve",
        "--workspace",
        root,
        "--config",
        Path.join(project_root, "config/pika.example.yaml"),
        "--port",
        Integer.to_string(port_number)
      ] ++ if(opts[:repo], do: ["--repo", opts[:repo]], else: [])

    port =
      Port.open({:spawn_executable, executable}, [
        :binary,
        :exit_status,
        :stderr_to_stdout,
        {:line, 8_192},
        args: args,
        cd: String.to_charlist(project_root),
        env: [{~c"MIX_ENV", ~c"test"}, {~c"NO_COLOR", ~c"1"}]
      ])

    {:os_pid, os_pid} = Port.info(port, :os_pid)
    %{port: port, os_pid: os_pid}
  end

  defp await_url(port, output, deadline) do
    remaining = max(deadline - System.monotonic_time(:millisecond), 0)

    receive do
      {^port, {:data, {_line_kind, line}}} ->
        output = output <> line <> "\n"

        case Regex.run(~r{Pika URL: (http://[^?]+/\?token=([A-Za-z0-9_-]+))}, output) do
          [_, url, token] -> {output, url, token}
          _ -> await_url(port, output, deadline)
        end

      {^port, {:exit_status, status}} ->
        flunk("pika serve exited with #{status} before startup:\n#{output}")
    after
      remaining -> flunk("timed out waiting for pika serve:\n#{output}")
    end
  end

  defp await_exit(port, output, timeout) do
    receive do
      {^port, {:data, {_line_kind, line}}} -> await_exit(port, output <> line <> "\n", timeout)
      {^port, {:exit_status, status}} -> {output, status}
    after
      timeout -> flunk("timed out waiting for pika serve to exit:\n#{output}")
    end
  end

  defp kill_9(server) do
    {_output, 0} =
      System.cmd("kill", ["-9", Integer.to_string(server.os_pid)], stderr_to_stdout: true)

    :ok
  end

  defp stop_server(%{port: port, os_pid: os_pid}) do
    if Port.info(port) do
      _ = System.cmd("kill", ["-TERM", Integer.to_string(os_pid)], stderr_to_stdout: true)

      receive do
        {^port, {:exit_status, _}} -> :ok
      after
        2_000 ->
          _ = System.cmd("kill", ["-9", Integer.to_string(os_pid)], stderr_to_stdout: true)
      end
    end

    :ok
  end

  defp http_get(url), do: request(:get, url, [], nil)

  defp cookie_get(port, path, cookie),
    do: request(:get, url(port, path), [{~c"cookie", String.to_charlist(cookie)}], nil)

  defp bearer_get(port, path, token),
    do:
      request(
        :get,
        url(port, path),
        [{~c"authorization", String.to_charlist("Bearer #{token}")}],
        nil
      )

  defp mcp_initialize(port, token) do
    body = Jason.encode!(%{"jsonrpc" => "2.0", "id" => 1, "method" => "initialize"})

    request(
      :post,
      url(port, "/mcp"),
      [{~c"authorization", String.to_charlist("Bearer #{token}")}],
      {~c"application/json", body}
    )
  end

  defp request(method, url, headers, nil) do
    case :httpc.request(method, {String.to_charlist(url), headers}, [autoredirect: false],
           body_format: :binary
         ) do
      {:ok, {{_version, status, _reason}, response_headers, body}} ->
        {:ok, status, response_headers, body}

      error ->
        error
    end
  end

  defp request(method, url, headers, {content_type, body}) do
    case :httpc.request(
           method,
           {String.to_charlist(url), headers, content_type, body},
           [autoredirect: false],
           body_format: :binary
         ) do
      {:ok, {{_version, status, _reason}, response_headers, response_body}} ->
        {:ok, status, response_headers, response_body}

      error ->
        error
    end
  end

  defp response_cookie(headers) do
    headers
    |> Enum.find_value(fn
      {name, value} when name in [~c"set-cookie", ~c"Set-Cookie"] -> value
      _ -> nil
    end)
    |> to_string()
    |> String.split(";", parts: 2)
    |> List.first()
  end

  defp singleton_count(root) do
    sqlite_scalar(root, "SELECT count(*) FROM campaigns")
  end

  defp campaign_status(root) do
    sqlite_scalar(root, "SELECT status FROM campaigns LIMIT 1")
  end

  defp sqlite_storage_bytes(root) do
    ["pika.sqlite3", "pika.sqlite3-wal", "pika.sqlite3-shm"]
    |> Enum.map(&Path.join(root, &1))
    |> Enum.map_join(fn path ->
      case File.read(path) do
        {:ok, contents} -> contents
        {:error, :enoent} -> ""
      end
    end)
  end

  defp insert_corrupt_artifact(root, campaign_id, relative) do
    artifact_id = Ecto.UUID.generate()
    wrong_hash = String.duplicate("0", 64)

    insert_artifact(
      root,
      campaign_id,
      artifact_id,
      relative,
      wrong_hash,
      10,
      "backend_log",
      "application/x-ndjson"
    )
  end

  defp insert_artifact(root, campaign_id, artifact_id, relative, sha256, size, kind, mime) do
    now = System.system_time(:microsecond)

    sqlite_execute(
      root,
      """
      INSERT INTO artifacts
        (id, campaign_id, owner_type, owner_id, kind, relative_path, sha256, byte_size, mime_type, metadata_json, created_at)
      VALUES
        (?, ?, 'campaign', ?, ?, ?, ?, ?, ?, '{}', ?)
      """,
      [artifact_id, campaign_id, campaign_id, kind, relative, sha256, size, mime, now]
    )
  end

  defp artifact_path(root, artifact_id) do
    sqlite_scalar(root, "SELECT relative_path FROM artifacts WHERE id = ?", [artifact_id])
  end

  defp sqlite_scalar(root, sql, params \\ []) do
    with_sqlite(root, :readonly, fn database ->
      {:ok, statement} = Exqlite.Sqlite3.prepare(database, sql)

      try do
        :ok = Exqlite.Sqlite3.bind(statement, params)
        {:row, [value]} = Exqlite.Sqlite3.step(database, statement)
        value
      after
        :ok = Exqlite.Sqlite3.release(database, statement)
      end
    end)
  end

  defp sqlite_execute(root, sql, params) do
    with_sqlite(root, :readwrite, fn database ->
      {:ok, statement} = Exqlite.Sqlite3.prepare(database, sql)

      try do
        :ok = Exqlite.Sqlite3.bind(statement, params)
        :done = Exqlite.Sqlite3.step(database, statement)
        :ok
      after
        :ok = Exqlite.Sqlite3.release(database, statement)
      end
    end)
  end

  defp with_sqlite(root, mode, fun) do
    path = Path.join(root, "pika.sqlite3")
    {:ok, database} = Exqlite.Sqlite3.open(path, mode: mode)

    try do
      fun.(database)
    after
      :ok = Exqlite.Sqlite3.close(database)
    end
  end

  defp url(port, path), do: "http://127.0.0.1:#{port}#{path}"

  defp missing_workspace do
    root = CampaignFixtures.workspace()
    File.rmdir!(root)
    root
  end
end
