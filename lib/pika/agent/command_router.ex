defmodule Pika.Agent.CommandRouter do
  @moduledoc "Role-authorized v2 MCP query/command adapter over the domain Lifecycles."

  alias Pika.Agent.{ToolCatalog, SessionBinding}
  alias Pika.Attempt.Lifecycle, as: AttemptLifecycle
  alias Pika.Baseline.Lifecycle, as: BaselineLifecycle
  alias Pika.Baseline.Questions, as: BaselineQuestions
  alias Pika.Followup.Lifecycle, as: FollowupLifecycle
  alias Pika.Integration.Lifecycle, as: IntegrationLifecycle
  alias Pika.Optimization.{Config, FileContract, OperationReceipts, SchemaValidator}
  alias Pika.ProgressSummary.Lifecycle, as: ProgressSummaryLifecycle
  alias Pika.Repo

  @file_arguments %{
    "submit_baseline_definition" => "definition_path",
    "finish_baseline_verification" => "result_path",
    "finish_iteration" => "result_path",
    "prepare_best_update" => "validation_path",
    "finish_integration" => "result_path",
    "submit_progress_summary" => "summary_path"
  }

  @spec invoke(SessionBinding.t(), String.t(), map(), Config.t(), keyword()) ::
          {:ok, term()} | {:error, term()}
  def invoke(%SessionBinding{} = binding, operation, arguments, %Config{} = config, opts \\ [])
      when is_binary(operation) and is_map(arguments) do
    with {:ok, tool} <- authorized_tool(binding.role_id, operation),
         :ok <- validate_arguments(arguments, tool["inputSchema"]) do
      case tool_kind(binding.role_id, operation) do
        :query -> query(binding, operation, arguments, config, opts)
        :command -> command(binding, operation, arguments, config, opts)
      end
    end
  end

  defp command(binding, operation, arguments, config, opts) do
    idempotency_key = arguments["idempotency_key"]

    with {:ok, request} <- command_request(binding, operation, arguments) do
      identity = %{
        role: binding.role_id,
        work_kind: binding.work_kind,
        work_id: binding.work_id
      }

      case OperationReceipts.run(identity, operation, idempotency_key, request, fn ->
             dispatch_command(binding, operation, arguments, config, opts)
           end) do
        {:ok, response, _mode} -> {:ok, response}
        {:error, reason} -> {:error, reason}
      end
    end
  end

  defp command_request(binding, operation, arguments) do
    request = Map.delete(arguments, "idempotency_key")

    case Map.fetch(@file_arguments, operation) do
      {:ok, field} ->
        path = arguments[field]

        with {:ok, _contents, receipt} <-
               FileContract.read(binding.work_root, path, max_bytes: 64 * 1024 * 1024) do
          {:ok,
           Map.put(request, "submitted_file", %{
             "path" => receipt.relative_path,
             "sha256" => receipt.sha256,
             "byte_size" => receipt.byte_size
           })}
        end

      :error ->
        {:ok, request}
    end
  end

  defp query(binding, "get_context", _arguments, _config, _opts) do
    with {:ok, _contents, receipt} <-
           FileContract.read(Path.dirname(binding.context_file), "context.json") do
      {:ok,
       %{
         role: binding.role_id,
         work_kind: to_string(binding.work_kind),
         work_id: binding.work_id,
         session_id: binding.session_id,
         context_file: binding.context_file,
         context_sha256: receipt.sha256
       }}
    end
  end

  defp query(binding, "query_attempt_history", arguments, config, _opts) do
    limit = min(arguments["limit"] || config.iteration.history_limit, 100)

    attempts =
      Repo.query!(
        """
        SELECT id, summary, status, failure_reason
        FROM attempts
        WHERE optimization_id = 'optimization' AND status IN ('accepted', 'rejected')
        ORDER BY id DESC LIMIT ?
        """,
        [limit]
      ).rows
      |> Enum.map(fn [id, summary, status, failure_reason] ->
        name = id |> Integer.to_string() |> String.pad_leading(6, "0")

        %{
          attempt_id: id,
          summary: summary,
          status: status,
          failure_reason: failure_reason,
          path: Path.join([config.workspace, "attempts", name])
        }
      end)

    {:ok, %{attempts: attempts, requested_by: binding.work_id}}
  end

  defp query(binding, "ask_questions", %{"questions" => questions}, _config, opts) do
    with :ok <- validate_question_identity(questions),
         handler when is_function(handler, 2) <- Keyword.get(opts, :question_handler),
         {:ok, answers} <- handler.(binding, questions) do
      {:ok, %{answers: answers}}
    else
      nil -> {:error, :question_handler_unavailable}
      {:error, _reason} = error -> error
      _other -> {:error, :invalid_question_handler_result}
    end
  end

  defp dispatch_command(
         binding,
         "submit_baseline_definition",
         %{"definition_path" => path},
         _config,
         _opts
       ) do
    with {:ok, revision_id} <- integer_id(binding.work_id),
         {:ok, revision} <- baseline_revision(binding.work_id),
         :ok <- require_answered_baseline_questions(revision_id),
         do: BaselineLifecycle.submit_definition(revision, binding.work_root, path)
  end

  defp dispatch_command(
         binding,
         "finish_baseline_verification",
         %{"result_path" => path},
         _config,
         _opts
       ) do
    with {:ok, revision} <- baseline_revision(binding.work_id),
         do: BaselineLifecycle.finish_verification(revision, binding.work_root, path)
  end

  defp dispatch_command(binding, "finish_iteration", %{"result_path" => path}, _config, _opts) do
    with {:ok, attempt_id} <- integer_id(binding.work_id),
         do: AttemptLifecycle.finish(attempt_id, binding.work_root, path)
  end

  defp dispatch_command(
         binding,
         "prepare_best_update",
         %{"validation_path" => path},
         config,
         _opts
       ) do
    with {:ok, attempt_id} <- integer_id(binding.work_id),
         do:
           IntegrationLifecycle.prepare_best_update(
             attempt_id,
             binding.work_root,
             path,
             config
           )
  end

  defp dispatch_command(
         binding,
         "finish_integration",
         %{"result_path" => path},
         config,
         _opts
       ) do
    with {:ok, attempt_id} <- integer_id(binding.work_id),
         do: IntegrationLifecycle.finish(attempt_id, binding.work_root, path, config)
  end

  defp dispatch_command(
         binding,
         "submit_followup_message",
         %{"message" => message},
         _config,
         _opts
       ) do
    with {:ok, request_id} <- integer_id(binding.work_id),
         do: FollowupLifecycle.submit_message(request_id, message)
  end

  defp dispatch_command(
         binding,
         "submit_progress_summary",
         %{"summary_path" => path},
         config,
         opts
       ) do
    now = Keyword.get(opts, :now, DateTime.utc_now())

    with {:ok, request_id} <- integer_id(binding.work_id),
         do: ProgressSummaryLifecycle.submit(request_id, path, config, now)
  end

  defp authorized_tool(role_id, operation) do
    with {:ok, tools} <- ToolCatalog.for_role(role_id) do
      case Enum.find(tools, &(&1["name"] == operation)) do
        nil -> {:error, {:forbidden_operation, role_id, operation}}
        tool -> {:ok, tool}
      end
    end
  end

  defp tool_kind(role_id, operation) do
    {:ok, definition} = Pika.Optimization.RoleRegistry.fetch(role_id)
    Enum.find(definition.tools, &(&1.name == operation)).kind
  end

  defp validate_arguments(arguments, schema) do
    case SchemaValidator.validate(arguments, schema) do
      :ok -> :ok
      {:error, errors} -> {:error, {:invalid_tool_arguments, errors}}
    end
  end

  defp validate_question_identity(questions) do
    ids = Enum.map(questions, & &1["id"])

    labels_unique? =
      Enum.all?(questions, fn question ->
        labels = Enum.map(question["options"], & &1["label"])
        Enum.uniq(labels) == labels
      end)

    cond do
      Enum.uniq(ids) != ids -> {:error, :duplicate_question_ids}
      not labels_unique? -> {:error, :duplicate_question_option_labels}
      true -> :ok
    end
  end

  defp baseline_revision(work_id) do
    with {:ok, id} <- integer_id(work_id) do
      case Repo.query!("SELECT revision FROM baseline_revisions WHERE id = ?", [id]).rows do
        [[revision]] -> {:ok, revision}
        [] -> {:error, {:baseline_revision_not_found, id}}
      end
    end
  end

  defp require_answered_baseline_questions(revision_id) do
    if BaselineQuestions.answered_for_revision?(revision_id),
      do: :ok,
      else: {:error, :baseline_questions_required}
  end

  defp integer_id(value) do
    case Integer.parse(value) do
      {id, ""} when id > 0 -> {:ok, id}
      _other -> {:error, {:invalid_work_id, value}}
    end
  end
end
