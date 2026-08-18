defmodule Pika.MCP.ProbeServer do
  @moduledoc false

  def start_link(opts \\ []) do
    port = Keyword.get_lazy(opts, :port, &available_port/0)

    case Bandit.start_link(plug: Pika.MCP.ProbeRouter, ip: {127, 0, 0, 1}, port: port) do
      {:ok, pid} -> {:ok, %{pid: pid, port: port, url: "http://127.0.0.1:#{port}/mcp"}}
      error -> error
    end
  end

  def stop(%{pid: pid}), do: GenServer.stop(pid)

  def available_port do
    {:ok, socket} = :gen_tcp.listen(0, [:binary, active: false, ip: {127, 0, 0, 1}])
    {:ok, port} = :inet.port(socket)
    :ok = :gen_tcp.close(socket)
    port
  end
end
