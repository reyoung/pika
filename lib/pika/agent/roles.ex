defmodule Pika.Agent.Roles do
  @moduledoc "Deep Role Runtime interface used by Agent Actors."

  alias Pika.Agent.Completion
  alias Pika.Agent.Role

  alias Pika.Agent.Role.{
    Context,
    Definition,
    Error,
    Instructions,
    Invocation,
    Outcome,
    Prepared,
    Work
  }

  @session_modes [:fresh, :recovering]

  def prepare(%Work{} = work, session_mode, opts \\ []) when session_mode in @session_modes do
    role = Keyword.fetch!(opts, :role)
    workspace = Keyword.fetch!(opts, :workspace)
    profile = Keyword.get(opts, :profile, %{})
    domain_options = Keyword.get(opts, :domain_options, %{})

    with %Definition{} = definition <- role.definition(),
         :ok <- validate_definition(role, definition),
         operation_receipts <-
           Keyword.get_lazy(opts, :operation_receipts, fn ->
             operation_receipts(definition.domain_adapter)
           end),
         :ok <- validate_work(work, definition),
         {:ok, domain} <- definition.domain_adapter.prepare(work, workspace),
         {:ok, template} <-
           Pika.Agent.Template.resolve(
             workspace,
             definition.template,
             domain.durable_context
           ),
         context <- %Context{
           work: work,
           workspace: workspace,
           facts: domain.facts,
           durable_context: domain.durable_context,
           template: template.text,
           session_mode: session_mode
         },
         {:ok, system} <- role.build_system_instructions(context),
         {:ok, activation} <- activation(role, definition, context, session_mode) do
      progress = Completion.evaluate(definition.completion, domain.facts)

      prepared = %Prepared{
        work: work,
        role: role,
        definition: definition,
        workspace: workspace,
        profile: profile,
        cwd: domain.cwd,
        skill_roots: domain.skill_roots,
        instructions: %Instructions{
          system: system,
          sha256: sha256(system),
          template_sha256: template.sha256,
          template_source: template.source
        },
        activation: activation,
        progress: progress,
        operation_receipts: operation_receipts,
        domain_options: domain_options
      }

      if match?({:terminal, _}, progress.state),
        do: {:skip, progress},
        else: {:ok, prepared}
    else
      {:error, %Error{} = error} -> {:error, error}
      {:error, reason} -> {:error, error(:prepare, reason, work)}
      other -> {:error, error(:prepare, {:invalid_role_result, other}, work)}
    end
  rescue
    error -> {:error, error(:prepare, {:exception, Exception.message(error)}, work)}
  end

  def invoke(%Prepared{} = prepared, %Invocation{} = invocation) do
    with {:ok, tool} <- authorize(prepared, invocation),
         :ok <- require_idempotency(tool, invocation),
         {:ok, request_sha256} <- validate_request_sha256(invocation),
         {:ok, value} <- invoke_authorized(prepared, tool, invocation, request_sha256),
         {:ok, domain} <-
           prepared.definition.domain_adapter.prepare(prepared.work, prepared.workspace) do
      progress = Completion.evaluate(prepared.definition.completion, domain.facts)

      {:ok,
       %Outcome{
         value: value,
         progress: progress,
         actor_directive: directive(progress)
       }}
    else
      {:error, %Error{} = error} -> {:error, error}
      {:error, reason} -> {:error, error(:operation, reason, prepared.work)}
    end
  rescue
    error ->
      {:error, error(:operation, {:exception, Exception.message(error)}, prepared.work)}
  end

  def progress(%Prepared{} = prepared) do
    case prepared.definition.domain_adapter.prepare(prepared.work, prepared.workspace) do
      {:ok, domain} ->
        {:ok, Completion.evaluate(prepared.definition.completion, domain.facts)}

      {:error, reason} ->
        {:error, error(:completion, reason, prepared.work)}
    end
  rescue
    error ->
      {:error, error(:completion, {:exception, Exception.message(error)}, prepared.work)}
  end

  def handle_exhaustion(%Prepared{} = prepared, reason, session_id) do
    adapter = prepared.definition.domain_adapter

    if function_exported?(adapter, :handle_exhaustion, 3) do
      with {:ok, _value} <-
             adapter.handle_exhaustion(
               prepared.work,
               reason,
               lifecycle_meta(prepared, session_id)
             ),
           {:ok, domain} <- adapter.prepare(prepared.work, prepared.workspace) do
        {:ok, Completion.evaluate(prepared.definition.completion, domain.facts)}
      else
        {:error, %Error{} = error} ->
          {:error, error}

        {:error, exhaustion_reason} ->
          {:error, error(:exhaustion, exhaustion_reason, prepared.work)}
      end
    else
      {:error, error(:exhaustion, :unsupported_role_exhaustion, prepared.work)}
    end
  rescue
    error ->
      {:error, error(:exhaustion, {:exception, Exception.message(error)}, prepared.work)}
  end

  def validate_definition(role, %Definition{} = definition) when is_atom(role) do
    tool_names = Enum.map(definition.tools, & &1.name)

    checks = [
      {definition.id =~ ~r/^[a-z][a-z0-9_]*$/, :invalid_role_id},
      {is_integer(definition.contract_revision) and definition.contract_revision > 0,
       :invalid_contract_revision},
      {definition.activation in [:automatic, :await_user_kickoff], :invalid_activation},
      {is_integer(definition.max_followups) and definition.max_followups >= 0,
       :invalid_max_followups},
      {definition.followup_strategy in [:direct, :agent], :invalid_followup_strategy},
      {is_atom(definition.work_kind), :invalid_work_kind},
      {Code.ensure_loaded?(definition.domain_adapter), :domain_adapter_unavailable},
      {function_exported?(definition.domain_adapter, :prepare, 2), :invalid_domain_adapter},
      {function_exported?(definition.domain_adapter, :invoke, 4), :invalid_domain_adapter},
      {Enum.uniq(tool_names) == tool_names, :duplicate_tool_name},
      {Enum.all?(definition.tools, &valid_tool?/1), :invalid_tool},
      {Enum.all?(
         [:build_system_instructions, :initial_prompt, :recovery_prompt],
         &function_exported?(role, &1, 1)
       ), :invalid_role_callbacks}
    ]

    case Enum.find(checks, fn {valid?, _reason} -> not valid? end) do
      nil -> Completion.validate(definition.completion, tool_names)
      {_valid?, reason} -> {:error, reason}
    end
  end

  defp activation(role, %Definition{activation: :automatic}, context, :fresh),
    do: start_turn(role.initial_prompt(context), :initial_prompt_missing)

  defp activation(role, %Definition{activation: :automatic}, context, :recovering),
    do: start_turn(role.recovery_prompt(context), :recovery_prompt_missing)

  defp activation(_role, %Definition{activation: :await_user_kickoff}, _context, :fresh),
    do: {:ok, :await_user_kickoff}

  defp activation(role, %Definition{activation: :await_user_kickoff}, context, :recovering) do
    if kickoff_recorded?(context.facts),
      do: start_turn(role.recovery_prompt(context), :recovery_prompt_missing),
      else: {:ok, :await_user_kickoff}
  end

  defp start_turn({:ok, prompt}, _reason) when is_binary(prompt) and prompt != "",
    do: {:ok, {:start_turn, prompt}}

  defp start_turn(:none, reason), do: {:error, reason}
  defp start_turn({:error, _reason} = error, _missing), do: error
  defp start_turn(_other, reason), do: {:error, reason}

  defp kickoff_recorded?(facts),
    do: Map.get(facts, :kickoff_recorded, Map.get(facts, "kickoff_recorded", false)) == true

  defp operation_receipts(adapter) do
    if function_exported?(adapter, :operation_receipts, 0),
      do: adapter.operation_receipts(),
      else: Pika.Agent.OperationReceipts
  end

  defp validate_work(%Work{role_id: role_id, kind: kind}, %Definition{
         id: role_id,
         work_kind: kind
       }),
       do: :ok

  defp validate_work(_work, _definition), do: {:error, :role_work_mismatch}

  defp valid_tool?(%Role.Tool{
         name: name,
         description: description,
         kind: kind,
         input_schema: schema
       }),
       do:
         is_binary(name) and name != "" and is_binary(description) and kind in [:query, :command] and
           is_map(schema)

  defp valid_tool?(_tool), do: false

  defp authorize(%Prepared{definition: definition}, %Invocation{operation: operation}) do
    case Enum.find(definition.tools, &(&1.name == operation)) do
      nil -> {:error, :forbidden_operation}
      tool -> {:ok, tool}
    end
  end

  defp require_idempotency(%Role.Tool{kind: :command}, %Invocation{idempotency_key: key})
       when not is_binary(key) or key == "",
       do: {:error, :idempotency_key_required}

  defp require_idempotency(_tool, _invocation), do: :ok

  defp validate_request_sha256(%Invocation{} = invocation) do
    calculated = request_sha256(invocation)

    case invocation.request_sha256 do
      nil -> {:ok, calculated}
      ^calculated -> {:ok, calculated}
      _other -> {:error, :request_sha256_mismatch}
    end
  end

  defp invoke_authorized(prepared, %Role.Tool{kind: :query}, invocation, request_sha256) do
    prepared.definition.domain_adapter.invoke(
      prepared.work,
      invocation.operation,
      invocation.arguments,
      invocation_meta(prepared, invocation, request_sha256)
    )
  end

  defp invoke_authorized(prepared, %Role.Tool{kind: :command}, invocation, request_sha256) do
    operation = fn ->
      prepared.definition.domain_adapter.invoke(
        prepared.work,
        invocation.operation,
        invocation.arguments,
        invocation_meta(prepared, invocation, request_sha256)
      )
    end

    if function_exported?(prepared.operation_receipts, :run, 6) do
      prepared.operation_receipts.run(
        prepared.work,
        prepared.definition.id,
        invocation,
        request_sha256,
        operation,
        fn stored -> replay(prepared, invocation, request_sha256, stored) end
      )
    else
      prepared.operation_receipts.run(
        prepared.work,
        prepared.definition.id,
        invocation,
        request_sha256,
        operation
      )
    end
  end

  defp replay(prepared, invocation, request_sha256, stored) do
    adapter = prepared.definition.domain_adapter

    if function_exported?(adapter, :replay, 5) do
      adapter.replay(
        prepared.work,
        invocation.operation,
        invocation.arguments,
        stored,
        invocation_meta(prepared, invocation, request_sha256)
      )
    else
      {:ok, stored}
    end
  end

  defp invocation_meta(prepared, invocation, request_sha256) do
    %{
      role_id: prepared.definition.id,
      contract_revision: prepared.definition.contract_revision,
      facts_revision: prepared.progress.facts_revision,
      session_id: invocation.session_id,
      idempotency_key: invocation.idempotency_key,
      request_sha256: request_sha256,
      workspace: prepared.workspace,
      domain_options: prepared.domain_options
    }
  end

  defp lifecycle_meta(prepared, session_id) do
    %{
      role_id: prepared.definition.id,
      contract_revision: prepared.definition.contract_revision,
      facts_revision: prepared.progress.facts_revision,
      session_id: session_id,
      workspace: prepared.workspace,
      domain_options: prepared.domain_options
    }
  end

  defp request_sha256(invocation) do
    Jason.encode!(%{
      operation: invocation.operation,
      arguments: Pika.JSONSafe.json_safe(invocation.arguments)
    })
    |> sha256()
  end

  defp directive(%{state: {:terminal, _outcome}}), do: :finish
  defp directive(_progress), do: :keep_running

  defp error(phase, reason, work) do
    %Error{
      code: error_code(reason),
      phase: phase,
      message: inspect(reason),
      role_id: work.role_id,
      work: work,
      details: %{reason: Pika.JSONSafe.json_safe(reason)}
    }
  end

  defp error_code(reason) when is_atom(reason), do: reason
  defp error_code({code, _details}) when is_atom(code), do: code
  defp error_code(_reason), do: :role_prepare_failed

  defp sha256(value),
    do: :crypto.hash(:sha256, value) |> Base.encode16(case: :lower)
end
