defmodule Pika.Integration.Lifecycle do
  @moduledoc """
  Deep Integration lifecycle module.

  It projects the next required Integration operation from committed facts and is the single
  state-changing interface used by Agent Role adapters.
  """

  alias Pika.Integration.Lifecycle.Operations
  alias Pika.Integration.Lifecycle.Receipts
  alias Pika.{IntegrationStore, Repo}

  @terminal_attempt_statuses ~w(accepted rejected cancelled)

  defmodule Identity do
    @moduledoc "Stable identity of one Integration Agent Work."
    @enforce_keys [:campaign_id, :attempt_id, :session_id]
    defstruct [:campaign_id, :attempt_id, :session_id]
  end

  defmodule State do
    @moduledoc "Committed Integration facts used by lifecycle decisions."
    @enforce_keys [:attempt_status, :campaign_status]
    defstruct [
      :attempt_status,
      :campaign_status,
      :lease_id,
      :lease_session_id,
      :receipt_status,
      :intent_id,
      stale_base?: false
    ]
  end

  defmodule Decision do
    @moduledoc "The next required Integration operation, if any."
    @enforce_keys [:operation]
    defstruct [:operation]
  end

  defmodule Projection do
    @moduledoc "Typed Integration lifecycle projection."
    @enforce_keys [:identity, :revision, :state, :decision, :context]
    defstruct [:identity, :revision, :state, :decision, :context]
  end

  defmodule Command do
    @moduledoc "A typed state-changing Integration command."
    @enforce_keys [:operation, :facts_revision, :idempotency_key, :params]
    defstruct [:operation, :facts_revision, :idempotency_key, :params]
  end

  def project(%Identity{} = identity) do
    with {:ok, context} <-
           IntegrationStore.integration_context(identity.campaign_id, identity.attempt_id) do
      state = state(context)

      {:ok,
       %Projection{
         identity: identity,
         revision: facts_revision(identity.campaign_id, identity.attempt_id),
         state: state,
         decision: %Decision{operation: next_operation(state, identity.session_id)},
         context: context
       }}
    end
  end

  def execute(
        %Identity{} = identity,
        %Command{
          facts_revision: revision,
          idempotency_key: key,
          params: params
        } = command,
        workspace
      )
      when is_integer(revision) and revision >= 0 and is_binary(key) and key != "" and
             is_map(params) do
    Receipts.run(
      identity,
      command,
      fn -> execute_fresh(identity, command, workspace) end,
      fn stored -> replay(identity, command, workspace, stored) end
    )
  end

  def execute(%Identity{}, %Command{}, _workspace), do: {:error, :invalid_integration_command}

  defp execute_fresh(%Identity{} = identity, %Command{} = command, workspace) do
    with {:ok, projection} <- project(identity),
         :ok <- validate_revision(projection, command),
         :ok <- validate_operation(projection, command) do
      execute_command(identity, command, workspace)
    end
  end

  def ensure_recoverable_best(workspace, attempt),
    do: Operations.ensure_recoverable_best(workspace, attempt)

  def facts(%Projection{} = projection) do
    operation = projection.decision.operation

    %{
      attempt_status: projection.state.attempt_status,
      campaign_status: projection.state.campaign_status,
      needs_acquire: operation == :acquire_integration_lease,
      needs_refresh: operation == :complete_refresh,
      needs_regression: operation == :submit_full_regression,
      needs_reject: operation == :reject_attempt,
      needs_intent: operation == :create_merge_intent,
      needs_merge: operation == :complete_merge,
      _revision: projection.revision
    }
  end

  def required_operations(%Projection{decision: %Decision{operation: nil}}), do: []

  def required_operations(%Projection{decision: %Decision{operation: operation}}),
    do: [Atom.to_string(operation)]

  defp state(context) do
    %State{
      attempt_status: context.attempt.status,
      campaign_status: context.status,
      lease_id: context.lease && context.lease.id,
      lease_session_id: context.lease && context.lease.backend_session_id,
      receipt_status: context.receipt && context.receipt.status,
      intent_id: context.intent && context.intent.id,
      stale_base?: context.attempt.base_sha != context.best_sha
    }
  end

  defp next_operation(
         %State{
           attempt_status: status,
           campaign_status: campaign_status
         },
         _session_id
       )
       when status in @terminal_attempt_statuses or campaign_status == "blocked",
       do: nil

  defp next_operation(%State{lease_id: nil}, _session_id), do: :acquire_integration_lease

  defp next_operation(%State{lease_session_id: lease_session_id}, session_id)
       when not is_binary(lease_session_id) or lease_session_id != session_id,
       do: :acquire_integration_lease

  defp next_operation(%State{stale_base?: true}, _session_id), do: :complete_refresh
  defp next_operation(%State{receipt_status: nil}, _session_id), do: :submit_full_regression
  defp next_operation(%State{receipt_status: "rejected"}, _session_id), do: :reject_attempt

  defp next_operation(%State{receipt_status: "passed", intent_id: nil}, _session_id),
    do: :create_merge_intent

  defp next_operation(%State{receipt_status: "passed"}, _session_id), do: :complete_merge
  defp next_operation(_state, _session_id), do: nil

  defp validate_revision(%Projection{revision: revision}, %Command{facts_revision: revision}),
    do: :ok

  defp validate_revision(%Projection{revision: revision}, %Command{facts_revision: stale}),
    do: {:error, {:stale_integration_facts, %{expected: revision, actual: stale}}}

  defp validate_operation(%Projection{decision: %Decision{operation: operation}}, %Command{
         operation: operation
       }),
       do: :ok

  defp validate_operation(
         %Projection{state: %State{attempt_status: status, campaign_status: campaign_status}},
         %Command{operation: operation}
       )
       when operation in [:register_artifact, :reject_stalled_attempt, :block_integration] and
              status not in @terminal_attempt_statuses and campaign_status != "blocked",
       do: :ok

  defp validate_operation(
         %Projection{decision: %Decision{operation: :submit_full_regression}},
         %Command{operation: :submit_fast_rejection}
       ),
       do: :ok

  defp validate_operation(%Projection{decision: %Decision{operation: expected}}, %Command{
         operation: actual
       }),
       do: {:error, {:invalid_integration_operation, %{expected: expected, actual: actual}}}

  defp execute_command(
         identity,
         %Command{operation: :acquire_integration_lease, params: params},
         _workspace
       ) do
    IntegrationStore.acquire_lease(
      identity.campaign_id,
      identity.attempt_id,
      identity.session_id,
      param(params, :expected_best_sha)
    )
  end

  defp execute_command(identity, command, workspace),
    do: Operations.execute(identity, command, workspace)

  defp replay(
         identity,
         %Command{operation: :acquire_integration_lease} = command,
         _workspace,
         _stored
       ),
       do: json_safe_result(execute_command(identity, command, nil))

  defp replay(_identity, _command, _workspace, stored), do: {:ok, stored}

  defp json_safe_result({:ok, value}), do: {:ok, Pika.JSONSafe.json_safe(value)}
  defp json_safe_result({:error, _reason} = error), do: error

  defp param(params, key) do
    case Map.fetch(params, key) do
      {:ok, value} -> value
      :error -> Map.get(params, Atom.to_string(key))
    end
  end

  defp facts_revision(campaign_id, attempt_id) do
    [[revision]] =
      Repo.query!(
        "SELECT COALESCE(MAX(sequence), 0) FROM domain_events WHERE aggregate_id IN (?, ?)",
        [campaign_id, attempt_id]
      ).rows

    revision
  end
end
