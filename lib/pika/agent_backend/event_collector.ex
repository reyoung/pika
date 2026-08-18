defmodule Pika.AgentBackend.EventCollector do
  @moduledoc false

  use GenServer

  def start_link, do: GenServer.start_link(__MODULE__, [])
  def events(server), do: GenServer.call(server, :events)

  def wait_for(server, predicate, timeout \\ 30_000) do
    deadline = System.monotonic_time(:millisecond) + timeout
    do_wait(server, predicate, deadline)
  end

  @impl true
  def init(events), do: {:ok, events}

  @impl true
  def handle_info({:pika_backend_event, event}, events), do: {:noreply, [event | events]}
  def handle_info(_message, events), do: {:noreply, events}

  @impl true
  def handle_call(:events, _from, events), do: {:reply, Enum.reverse(events), events}

  defp do_wait(server, predicate, deadline) do
    case Enum.find(events(server), predicate) do
      nil ->
        if System.monotonic_time(:millisecond) >= deadline do
          {:error, :timeout}
        else
          Process.sleep(50)
          do_wait(server, predicate, deadline)
        end

      event ->
        {:ok, event}
    end
  end
end
