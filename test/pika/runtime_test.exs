defmodule Pika.RuntimeTest do
  use ExUnit.Case, async: true

  test "attempt progress is isolated from the persisted campaign event topic" do
    campaign_id = Ecto.UUID.generate()

    refute Pika.AttemptCoordinator.progress_topic(campaign_id) ==
             Pika.Persistence.topic(campaign_id)
  end

  test "ignores unrelated process notifications defensively" do
    state = %{campaign: :unchanged}

    assert {:noreply, ^state} =
             Pika.Runtime.handle_info({:attempt_progress, Ecto.UUID.generate()}, state)
  end
end
