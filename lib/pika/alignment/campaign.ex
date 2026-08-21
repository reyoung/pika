defmodule Pika.Alignment.Campaign do
  @moduledoc false

  use GenServer

  alias Pika.AgentBackend

  alias Pika.{Baseline, Harness, PromptCatalog, ReferenceCatalog, Sampling, TargetSnapshot}

  alias Pika.Alignment.{
    ArtifactStore,
    BaselineManifest,
    ImplementationReviewEvidence,
    Workspace
  }

  alias Pika.CampaignSpec, as: Spec

  @topic "alignment:campaign"
  @write_tools ~w(register_artifact submit_spec submit_harness submit_implementation_review complete_setup_merge reopen_baseline_definition submit_baseline submit_iteration_sample)

  def start_link(opts), do: GenServer.start_link(__MODULE__, opts, name: __MODULE__)

  def start(opts) do
    DynamicSupervisor.start_child(Pika.CampaignSupervisor, %{
      id: __MODULE__,
      start: {__MODULE__, :start_link, [opts]},
      restart: :temporary,
      shutdown: 15_000,
      type: :worker
    })
  end

  def topic, do: @topic

  def snapshot do
    call_if_started(:snapshot, {:error, :not_started})
  end

  def implementation_review do
    call_if_started(:implementation_review, {:error, :not_started})
  end

  def authorize(token), do: call_if_started({:authorize, token}, {:error, :not_started})

  def mcp_call(token, tool, args),
    do: call_if_started({:mcp, token, tool, args}, {:error, :not_started}, :infinity)

  def send_message(body, artifacts \\ []),
    do: call_if_started({:send_message, body, artifacts}, {:error, :not_started})

  def toggle_reference(id), do: call_if_started({:toggle_reference, id}, {:error, :not_started})

  def add_reference_project(attrs),
    do: call_if_started({:add_reference_project, attrs}, {:error, :not_started})

  def remove_reference_project(id),
    do: call_if_started({:remove_reference_project, id}, {:error, :not_started})

  def confirm_spec(reviewed_target_digest, reviewed_development_sha, reviewed_evidence_digest),
    do:
      call_if_started(
        {:confirm_spec, reviewed_target_digest, reviewed_development_sha,
         reviewed_evidence_digest},
        {:error, :not_started}
      )

  def confirm_spec(reviewed_reference_sha, reviewed_evidence_digest),
    do:
      call_if_started(
        {:confirm_spec_v1, reviewed_reference_sha, reviewed_evidence_digest},
        {:error, :not_started}
      )

  def request_changes(body, artifacts \\ []),
    do: call_if_started({:request_changes, body, artifacts}, {:error, :not_started})

  def answer_question(question_id, answer, option_id \\ nil),
    do:
      call_if_started(
        {:answer_question, question_id, answer, option_id},
        {:error, :not_started}
      )

  @impl true
  def init(opts) do
    workspace = Keyword.fetch!(opts, :workspace)
    backend = Keyword.get(opts, :backend, :codex_app_server)

    initial_mcp_tokens =
      case Keyword.get(opts, :mcp_token) do
        token when is_binary(token) ->
          %{
            token_hash(token) => %{
              workflow: :alignment,
              role: :boundary,
              session_key: "test-session"
            }
          }

        _ ->
          %{}
      end

    state = %{
      campaign_id: Keyword.get(opts, :campaign_id),
      persistence: Keyword.get(opts, :persistence),
      status: :drafting_spec,
      workspace: workspace,
      backend_name: backend,
      backend_module: Keyword.get(opts, :backend_module, backend_module(backend)),
      backend_profile: Keyword.get(opts, :backend_profile, %{}),
      reload_backend_profile: Keyword.get(opts, :reload_backend_profile, false),
      model: Keyword.get(opts, :model),
      reasoning_effort: Keyword.get(opts, :reasoning_effort, :high),
      backend_enabled: Keyword.get(opts, :start_backend, true),
      backend: nil,
      backend_session: nil,
      backend_open_generation: 0,
      provider_session_id: Keyword.get(opts, :resume_session_id),
      backend_token_hash: nil,
      closed_sessions: MapSet.new(),
      backend_workflow: :alignment,
      active_turn_id: nil,
      agent_responding: false,
      stream_message_id: nil,
      activity_message_id: nil,
      pending_questions: nil,
      mcp_url: Keyword.fetch!(opts, :mcp_url),
      mcp_tokens: initial_mcp_tokens,
      idempotency: %{},
      messages: [
        message(
          :system,
          if(workspace.source_status == "",
            do: "Alignment Workspace 已从 clean HEAD 创建；源仓库不会被修改。",
            else: "Alignment Workspace 已从提交 HEAD 重新 clone；源仓库未提交修改被排除且不会被修改。"
          )
        )
      ],
      artifacts: %{},
      spec_result: Spec.validate(%{}),
      spec_diff: nil,
      harness: nil,
      reference_review_evidence: nil,
      implementation_review_evidence: nil,
      required: draft_required_operations(),
      references: Keyword.get(opts, :references, ReferenceCatalog.entries()),
      reference_progress: nil,
      resolve_references: Keyword.get(opts, :resolve_references, true),
      materialize_references: Keyword.get(opts, :materialize_references, false),
      skill: Keyword.fetch!(opts, :skill),
      skill_roots: Keyword.get(opts, :skill_roots, []),
      best_sha: workspace.source_sha,
      setup_base_sha: workspace.source_sha,
      setup_sha: nil,
      prepared_setup_sha: nil,
      target_snapshot: nil,
      inherited_target_snapshot: nil,
      target_submission: nil,
      target_progress: nil,
      baseline: nil,
      baseline_retry_count: 0,
      baseline_error: nil,
      baseline_submission: nil,
      baseline_progress: nil,
      sampling_revisions: [],
      iteration_sampling: nil,
      workflow_kickoffs: %{},
      kickoff_dispatched: false,
      recovery_pending: is_map(Keyword.get(opts, :durable_state)),
      pending_confirmation_input: nil,
      last_error: nil
    }

    state = restore_durable_state(state, Keyword.get(opts, :durable_state))

    with :ok <- restore_target_views(state),
         :ok <- PromptCatalog.validate() do
      broadcast(state)

      if state.status == :resolving_references do
        {:ok, state, {:continue, :resume_reference_resolution}}
      else
        if state.backend_enabled and state.status != :optimizing do
          {:ok, state, {:continue, :open_backend}}
        else
          {:ok, state}
        end
      end
    else
      {:error, reason} ->
        {:stop, reason}
    end
  end

  @impl true
  def handle_continue(:open_backend, state) do
    cwd =
      if state.backend_workflow == :baseline,
        do: state.workspace.repo,
        else: state.workspace.setup_worktree

    {:noreply, begin_open_session(state, state.backend_workflow, cwd)}
  end

  def handle_continue(:resume_reference_resolution, state) do
    {:noreply, start_reference_resolution(state)}
  end

  @impl true
  def handle_call(:snapshot, _from, state), do: {:reply, public_snapshot(state), state}

  def handle_call(:implementation_review, _from, state) do
    {:reply, load_implementation_review(state), state}
  end

  def handle_call({:authorize, token}, _from, state) do
    authorized = Map.has_key?(state.mcp_tokens, token_hash(token))
    {:reply, if(authorized, do: :ok, else: {:error, :unauthorized}), state}
  end

  def handle_call({:send_message, body, artifacts}, _from, state)
      when is_binary(body) and is_list(artifacts) do
    body = String.trim(body)

    if body == "" and artifacts == [] do
      {:reply, {:error, :empty_message}, state}
    else
      state = register_user_artifacts(state, artifacts)
      state = %{state | messages: state.messages ++ [message(:user, body, artifacts)]}
      input = user_input(body, artifacts)
      state = record_workflow_kickoff(state, input)
      state = dispatch_input(state, input)
      state = mark_kickoff_dispatched(state)
      broadcast(state)
      {:reply, :ok, state}
    end
  end

  def handle_call({:toggle_reference, id}, _from, state) do
    if state.status in [:drafting_spec, :awaiting_confirmation] do
      case Enum.find_index(state.references, &(&1.id == id)) do
        nil ->
          {:reply, {:error, :reference_project_not_found}, state}

        index ->
          references = List.update_at(state.references, index, &%{&1 | selected: not &1.selected})
          state = update_reference_projects(state, references, "Reference selection changed")
          broadcast(state)
          {:reply, :ok, state}
      end
    else
      {:reply, {:error, :references_frozen}, state}
    end
  end

  def handle_call({:add_reference_project, attrs}, _from, state) do
    if state.status in [:drafting_spec, :awaiting_confirmation] do
      case ReferenceCatalog.new_user_entry(attrs, state.references) do
        {:ok, entry} ->
          references = state.references ++ [entry]

          state =
            update_reference_projects(
              state,
              references,
              "User added Reference Project #{entry.id} from #{entry.url}"
            )

          broadcast(state)
          {:reply, {:ok, entry}, state}

        {:error, _reason} = error ->
          {:reply, error, state}
      end
    else
      {:reply, {:error, :references_frozen}, state}
    end
  end

  def handle_call({:remove_reference_project, id}, _from, state) do
    if state.status in [:drafting_spec, :awaiting_confirmation] do
      case Enum.find(state.references, &(&1.id == id)) do
        nil ->
          {:reply, {:error, :reference_project_not_found}, state}

        %{origin: :user} ->
          references = Enum.reject(state.references, &(&1.id == id))

          state =
            update_reference_projects(
              state,
              references,
              "User removed Reference Project #{id}"
            )

          broadcast(state)
          {:reply, :ok, state}

        _entry ->
          {:reply, {:error, :builtin_reference_project_cannot_be_removed}, state}
      end
    else
      {:reply, {:error, :references_frozen}, state}
    end
  end

  def handle_call(
        {:confirm_spec, reviewed_target_digest, reviewed_development_sha,
         reviewed_evidence_digest},
        _from,
        state
      ) do
    if state.status == :awaiting_confirmation and state.spec_result.ready? and
         not is_nil(state.harness) and not is_nil(state.target_snapshot) and
         not is_nil(state.prepared_setup_sha) do
      with :ok <- require_confirmation_idle(state),
           {:ok, _review} <- load_implementation_review(state),
           :ok <-
             require_review_match(
               reviewed_target_digest,
               state.target_snapshot.digest,
               :target_not_reviewed
             ),
           :ok <-
             require_review_match(
               reviewed_development_sha,
               state.prepared_setup_sha,
               :development_not_reviewed
             ),
           :ok <-
             require_review_match(
               reviewed_evidence_digest,
               state.implementation_review_evidence &&
                 state.implementation_review_evidence.digest,
               :implementation_evidence_not_reviewed
             ),
           :ok <- Harness.verify_digest(state.workspace.setup_worktree, state.harness),
           :ok <- TargetSnapshot.verify(state.workspace.root, state.target_snapshot),
           :ok <- verify_prepared_setup(state),
           :ok <- verify_implementation_review_evidence(state) do
        advance_confirmed_spec(state)
      else
        {:error, reason} when reason in [:agent_still_responding, :questions_pending] ->
          {:reply, {:error, reason}, state}

        {:error, reason}
        when reason in [
               :target_not_reviewed,
               :development_not_reviewed,
               :implementation_evidence_not_reviewed
             ] ->
          {:reply, {:error, reason}, state}

        {:error, reason} ->
          {:reply, {:error, {:implementation_review_failed, reason}}, state}
      end
    else
      {:reply, {:error, :spec_not_confirmable}, state}
    end
  end

  def handle_call(
        {:confirm_spec_v1, _reviewed_reference_sha, _reviewed_evidence_digest},
        _from,
        state
      ),
      do: {:reply, {:error, :campaign_spec_v2_review_required}, state}

  def handle_call({:request_changes, body, artifacts}, _from, state)
      when is_binary(body) and is_list(artifacts) do
    body = String.trim(body)

    cond do
      body == "" and artifacts == [] ->
        {:reply, {:error, :empty_message}, state}

      state.status in [:awaiting_confirmation, :drafting_spec] ->
        state = register_user_artifacts(state, artifacts)

        state = %{
          state
          | status: :drafting_spec,
            required: draft_required_operations(),
            reference_review_evidence: nil,
            implementation_review_evidence: nil,
            prepared_setup_sha: nil,
            target_snapshot: nil,
            inherited_target_snapshot: nil,
            messages: state.messages ++ [message(:user, "修改要求：#{body}", artifacts)]
        }

        state =
          dispatch_input(
            state,
            user_input("用户拒绝当前 Spec，并要求修改：#{body}", artifacts)
          )

        broadcast(state)
        {:reply, :ok, state}

      state.status == :building_baseline ->
        case reopen_confirmed_spec(state, body, artifacts) do
          {:ok, state} ->
            broadcast(state)
            {:reply, :ok, state}

          {:error, reason} ->
            {:reply, {:error, {:spec_reopen_failed, reason}}, state}
        end

      true ->
        {:reply, {:error, :invalid_state}, state}
    end
  end

  def handle_call({:answer_question, question_id, answer, option_id}, _from, state) do
    answer = if is_binary(answer), do: String.trim(answer), else: ""

    case state.pending_questions do
      nil ->
        {:reply, {:error, :no_pending_question}, state}

      batch ->
        question = current_question(batch)

        if question.id == question_id,
          do: answer_current_question(state, batch, question, answer, option_id),
          else: {:reply, {:error, :question_expired}, state}
    end
  end

  def handle_call({:mcp, token, tool, args}, from, state) do
    token_hash = token_hash(token)

    case Map.fetch(state.mcp_tokens, token_hash) do
      :error ->
        {:reply, mcp_error("unauthorized", "invalid Backend Session token"), state}

      {:ok, identity} ->
        execute_mcp(tool, stringify_keys(args), identity, token_hash, from, state)
    end
  end

  @impl true
  def handle_info({:reference_materialization_progress, progress}, state) do
    if state.status == :resolving_references do
      state = %{state | reference_progress: progress}
      broadcast(state, persist: false)
      {:noreply, state}
    else
      {:noreply, state}
    end
  end

  def handle_info(
        {:target_progress, submission_id, progress},
        %{target_submission: %{id: submission_id}} = state
      ) do
    state = %{state | target_progress: Map.merge(state.target_progress || %{}, progress)}
    broadcast(state, persist: false)
    {:noreply, state}
  end

  def handle_info(
        {:target_finished, submission_id, result},
        %{target_submission: %{id: submission_id} = submission} = state
      ) do
    Process.demonitor(submission.monitor_ref, [:flush])
    {response, state} = finish_target_submission(result, submission.args, state)
    entry = %{request_hash: submission.request_hash, response: response}

    state = %{
      state
      | target_submission: nil,
        target_progress: nil,
        idempotency: Map.put(state.idempotency, submission.record_key, entry)
    }

    store_idempotency(
      state,
      submission.identity,
      "submit_implementation_bundle",
      submission.key,
      submission.request_hash,
      response
    )

    state = notify_target_result(state, response)
    broadcast(state)
    {:noreply, state}
  end

  def handle_info(
        {:DOWN, monitor_ref, :process, _pid, reason},
        %{target_submission: %{monitor_ref: monitor_ref} = submission} = state
      ) do
    message =
      "Optimization Target 快照任务异常退出：#{inspect(reason)}。可以重新调用 submit_implementation_bundle。"

    state = %{
      state
      | target_submission: nil,
        target_progress: nil,
        idempotency: Map.delete(state.idempotency, submission.record_key),
        last_error: message,
        messages: state.messages ++ [message(:system, message)]
    }

    state = dispatch_input(state, message)
    broadcast(state)
    {:noreply, state}
  end

  def handle_info({:references_resolved, {:ok, references}}, state) do
    selected = Enum.filter(references, & &1.selected)

    spec =
      state.spec_result.spec
      |> Map.put("reference_ids", Enum.map(selected, & &1.id))
      |> Map.put(
        "reference_snapshot",
        Enum.map(selected, &Map.take(&1, [:id, :url, :branch, :sha]))
      )
      |> Map.put("skill_snapshot", Map.take(state.skill, [:name, :url, :sha]))

    spec_result = Spec.validate(spec)

    state = %{
      state
      | references: references,
        reference_progress: nil,
        spec_result: spec_result,
        status: :building_baseline,
        required: MapSet.new(["complete_setup_merge"]),
        messages:
          state.messages ++
            [
              message(
                :system,
                "Campaign Spec v#{spec_revision(state)} 已由用户确认；等待 Agent 完成 setup squash merge。"
              )
            ]
    }

    state =
      if state.backend_enabled and is_nil(state.backend) do
        state
        |> Map.update!(
          :workflow_kickoffs,
          &Map.put(&1, :alignment, state.pending_confirmation_input)
        )
        |> begin_open_session(:alignment, state.workspace.setup_worktree)
      else
        dispatch_input(state, state.pending_confirmation_input)
      end

    broadcast(state)
    {:noreply, state}
  end

  def handle_info({:references_resolved, {:error, references}}, state) do
    state = %{
      state
      | references: references,
        reference_progress: nil,
        status: :awaiting_confirmation,
        last_error: reference_failure_message(references)
    }

    broadcast(state)
    {:noreply, state}
  end

  def handle_info(
        {:backend_opened, generation, workflow, {:ok, handle, session}},
        %{backend_open_generation: generation} = state
      ) do
    mcp_tokens =
      if state.backend_token_hash do
        Map.update(state.mcp_tokens, state.backend_token_hash, nil, fn identity ->
          %{identity | session_key: session.id}
        end)
      else
        state.mcp_tokens
      end

    state = %{
      state
      | backend: handle,
        backend_session: session,
        provider_session_id: session.backend_session_id,
        mcp_tokens: mcp_tokens,
        backend_workflow: workflow,
        messages:
          state.messages ++
            [message(:system, backend_session_message(workflow, session))]
    }

    state =
      if state.recovery_pending,
        do: maybe_dispatch_recovery(state, session),
        else: maybe_dispatch_workflow_kickoff(state)

    state = %{state | recovery_pending: false}
    broadcast(state)
    {:noreply, state}
  end

  def handle_info(
        {:backend_opened, generation, _workflow, {:error, reason}},
        %{backend_open_generation: generation} = state
      ) do
    state = %{
      state
      | last_error: "Backend Session 启动失败：#{inspect(reason)}",
        backend: nil,
        agent_responding: false
    }

    broadcast(state)
    {:noreply, state}
  end

  def handle_info({:backend_opened, _generation, _workflow, {:ok, handle, _session}}, state) do
    close_backend(handle)
    {:noreply, state}
  end

  def handle_info({:backend_opened, _generation, _workflow, {:error, _reason}}, state),
    do: {:noreply, state}

  def handle_info(
        {:backend_turn_result, generation, result},
        %{backend_open_generation: generation} = state
      ) do
    state =
      case result do
        {:ok, _turn_id} ->
          state

        {:error, reason} ->
          %{state | last_error: "Backend Turn 失败：#{inspect(reason)}", agent_responding: false}
      end

    broadcast(state)
    {:noreply, state}
  end

  def handle_info({:backend_turn_result, _generation, _result}, state), do: {:noreply, state}

  def handle_info({:pika_backend_event, event}, state) do
    state =
      if MapSet.member?(state.closed_sessions, event.session_id) do
        state
      else
        apply_backend_event(state, event)
      end

    broadcast(state, persist: persist_backend_event?(event))
    {:noreply, state}
  end

  def handle_info({:completion_followup, _completed_turn_id}, state) do
    if state.backend && is_nil(state.active_turn_id) &&
         not MapSet.equal?(state.required, MapSet.new()) && state.status != :optimizing do
      if not is_nil(state.baseline_submission) or not is_nil(state.target_submission) do
        {:noreply, state}
      else
        missing = state.required |> MapSet.to_list() |> Enum.sort() |> Enum.join(", ")

        state =
          dispatch_input(
            state,
            "Pika completion gate is still open. Complete these required MCP operations before ending: #{missing}."
          )

        {:noreply, state}
      end
    else
      {:noreply, state}
    end
  end

  def handle_info(
        {:baseline_progress, submission_id, progress},
        %{baseline_submission: %{id: submission_id}} = state
      ) do
    state = %{state | baseline_progress: Map.merge(state.baseline_progress || %{}, progress)}
    broadcast(state, persist: false)
    {:noreply, state}
  end

  def handle_info(
        {:baseline_finished, submission_id, result},
        %{baseline_submission: %{id: submission_id} = submission} = state
      ) do
    Process.demonitor(submission.monitor_ref, [:flush])
    {response, state} = finish_baseline_submission(result, submission.args, state)
    entry = %{request_hash: submission.request_hash, response: response}

    state = %{
      state
      | baseline_submission: nil,
        baseline_progress: nil,
        idempotency: Map.put(state.idempotency, submission.record_key, entry)
    }

    store_idempotency(
      state,
      submission.identity,
      "submit_baseline",
      submission.key,
      submission.request_hash,
      response
    )

    state = notify_baseline_result(state, response)
    broadcast(state)
    {:noreply, state}
  end

  def handle_info(
        {:DOWN, monitor_ref, :process, _pid, reason},
        %{baseline_submission: %{monitor_ref: monitor_ref} = submission} = state
      ) do
    message = "Baseline 后台校验异常退出：#{inspect(reason)}。可以重新调用 submit_baseline。"

    state = %{
      state
      | baseline_submission: nil,
        baseline_progress: nil,
        baseline_error: inspect({:validation_worker_exited, reason}),
        idempotency: Map.delete(state.idempotency, submission.record_key),
        messages: state.messages ++ [message(:system, message)]
    }

    state = dispatch_input(state, message)
    broadcast(state)
    {:noreply, state}
  end

  def handle_info(
        {:DOWN, monitor_ref, :process, _pid, _reason},
        %{pending_questions: %{monitor_ref: monitor_ref}} = state
      ) do
    state = %{state | pending_questions: nil}
    broadcast(state)
    {:noreply, state}
  end

  def handle_info(:recover_backend, state) do
    cwd =
      if state.backend_workflow == :baseline,
        do: state.workspace.repo,
        else: state.workspace.setup_worktree

    {:noreply, begin_open_session(state, state.backend_workflow, cwd)}
  end

  def handle_info(_message, state), do: {:noreply, state}

  @impl true
  def terminate(_reason, state) do
    if state.target_submission && Process.alive?(state.target_submission.pid),
      do: Process.exit(state.target_submission.pid, :shutdown)

    if state.baseline_submission && Process.alive?(state.baseline_submission.pid),
      do: Process.exit(state.baseline_submission.pid, :shutdown)

    if state.backend, do: AgentBackend.close_session(state.backend)
    :ok
  catch
    :exit, _reason -> :ok
  end

  defp execute_mcp("get_context", _args, identity, _token_hash, _from, state) do
    result = %{
      session: identity,
      campaign: public_snapshot(state),
      required_operations: state.required |> MapSet.to_list() |> Enum.sort()
    }

    {:reply, {:ok, result}, state}
  end

  defp execute_mcp("ask_questions", args, identity, _token_hash, from, state) do
    with nil <- state.pending_questions,
         {:ok, questions} <- validate_questions(args) do
      pending = %{
        id: Pika.AgentBackend.Id.new("questions"),
        questions: questions,
        current_index: 0,
        answers: [],
        asked_at: DateTime.utc_now(),
        identity: identity,
        reply_to: from,
        monitor_ref: Process.monitor(elem(from, 0))
      }

      next_state = %{
        state
        | pending_questions: pending,
          messages: state.messages ++ [question_message(current_question(pending))],
          stream_message_id: nil,
          activity_message_id: nil,
          agent_responding: true
      }

      broadcast(next_state)
      {:noreply, next_state}
    else
      %{id: _id} ->
        {:reply,
         mcp_error("questions_pending", "wait for the user to answer the current question batch"),
         state}

      {:error, message} ->
        {:reply, mcp_error("missing_required_data", message), state}
    end
  end

  defp execute_mcp(
         "submit_implementation_bundle" = tool,
         args,
         identity,
         token_hash,
         _from,
         state
       ) do
    key = args["idempotency_key"]

    if not is_binary(key) or key == "" do
      {:reply, mcp_error("missing_required_data", "idempotency_key is required"), state}
    else
      request_hash = :crypto.hash(:sha256, :erlang.term_to_binary({tool, args}))
      record_key = {token_hash, tool, key}

      case lookup_idempotency(state, identity, tool, key, request_hash, record_key) do
        {:replay, response} ->
          {:reply, response, state}

        :missing ->
          case start_target_submission(
                 args,
                 identity,
                 key,
                 request_hash,
                 record_key,
                 state
               ) do
            {:started, response, next_state} ->
              entry = %{request_hash: request_hash, response: response}

              next_state = %{
                next_state
                | idempotency: Map.put(next_state.idempotency, record_key, entry)
              }

              broadcast(next_state, persist: false)
              {:reply, response, next_state}

            {:reply, response, next_state} ->
              {:reply, response, next_state}
          end

        :conflict ->
          {:reply,
           mcp_error("idempotency_conflict", "same key was used with a different request"), state}
      end
    end
  end

  defp execute_mcp("submit_baseline" = tool, args, identity, token_hash, _from, state) do
    case normalize_baseline_args(state, args) do
      {:ok, normalized_args} ->
        execute_baseline_mcp(tool, normalized_args, identity, token_hash, state)

      {:error, reason} ->
        {:reply,
         mcp_error("missing_required_data", "Baseline manifest validation failed", %{
           reason: reason
         }), state}
    end
  end

  defp execute_mcp(tool, args, identity, token_hash, _from, state) when tool in @write_tools do
    key = args["idempotency_key"]

    cond do
      not is_binary(key) or key == "" ->
        {:reply, mcp_error("missing_required_data", "idempotency_key is required"), state}

      true ->
        request_hash = :crypto.hash(:sha256, :erlang.term_to_binary({tool, args}))
        record_key = {token_hash, tool, key}

        case lookup_idempotency(state, identity, tool, key, request_hash, record_key) do
          {:replay, response} ->
            {:reply, response, state}

          :missing ->
            {response, next_state} = perform_write(tool, args, identity, state)
            entry = %{request_hash: request_hash, response: response}

            next_state = %{
              next_state
              | idempotency: Map.put(next_state.idempotency, record_key, entry)
            }

            store_idempotency(next_state, identity, tool, key, request_hash, response)

            broadcast(next_state)
            {:reply, response, next_state}

          :conflict ->
            {:reply,
             mcp_error("idempotency_conflict", "same key was used with a different request"),
             state}
        end
    end
  end

  defp execute_mcp(tool, _args, _identity, _token_hash, _from, state),
    do: {:reply, mcp_error("forbidden_role", "tool is unavailable: #{tool}"), state}

  defp execute_baseline_mcp(tool, args, identity, token_hash, state) do
    key = args["idempotency_key"]

    if not is_binary(key) or key == "" do
      {:reply, mcp_error("missing_required_data", "idempotency_key is required"), state}
    else
      hash_args = Map.drop(args, ["__baseline_manifest"])
      request_hash = :crypto.hash(:sha256, :erlang.term_to_binary({tool, hash_args}))
      record_key = {token_hash, tool, key}

      case lookup_idempotency(state, identity, tool, key, request_hash, record_key) do
        {:replay, response} ->
          {:reply, response, state}

        :missing ->
          case start_baseline_submission(
                 args,
                 identity,
                 key,
                 request_hash,
                 record_key,
                 state
               ) do
            {:started, response, next_state} ->
              entry = %{request_hash: request_hash, response: response}

              next_state = %{
                next_state
                | idempotency: Map.put(next_state.idempotency, record_key, entry)
              }

              broadcast(next_state, persist: false)
              {:reply, response, next_state}

            {:reply, response, next_state} ->
              entry = %{request_hash: request_hash, response: response}

              next_state = %{
                next_state
                | idempotency: Map.put(next_state.idempotency, record_key, entry)
              }

              store_idempotency(next_state, identity, tool, key, request_hash, response)
              broadcast(next_state)
              {:reply, response, next_state}

            {:transient_error, response} ->
              {:reply, response, state}
          end

        :conflict ->
          {:reply,
           mcp_error("idempotency_conflict", "same key was used with a different request"), state}
      end
    end
  end

  defp perform_write("register_artifact", args, _identity, state) do
    attrs = %{
      kind: args["kind"],
      sha256: args["sha256"],
      size: args["size"],
      mime: args["mime"],
      metadata: args["metadata"] || %{}
    }

    case ArtifactStore.register(state.workspace.root, args["relative_path"], attrs) do
      {:ok, artifact} ->
        response = {:ok, artifact}

        {response,
         %{state | artifacts: Map.put(state.artifacts, artifact.relative_path, artifact)}}

      {:error, reason} ->
        {mcp_error("missing_required_data", "artifact registration failed", %{reason: reason}),
         state}
    end
  end

  defp perform_write("submit_spec", args, %{workflow: :alignment}, state)
       when state.status in [:drafting_spec, :awaiting_confirmation] do
    selected_ids = for reference <- state.references, reference.selected, do: reference.id
    spec = args["spec"] |> map() |> Map.put("reference_ids", selected_ids)
    spec = Map.put(spec, "revision", spec_revision(state))
    result = Spec.validate(spec)

    inherited_target_snapshot =
      if target_definition(state.spec_result.spec) == target_definition(result.spec),
        do: state.target_snapshot || state.inherited_target_snapshot,
        else: nil

    state =
      state
      |> invalidate_implementation_definition()
      |> Map.put(:harness, nil)
      |> Map.put(:inherited_target_snapshot, inherited_target_snapshot)

    required =
      if result.ready?,
        do: MapSet.delete(state.required, "submit_spec"),
        else: MapSet.put(state.required, "submit_spec")

    state =
      %{
        state
        | spec_result: result,
          spec_diff: Spec.diff(state.spec_result.spec, result.spec),
          required: MapSet.put(required, "submit_harness")
      }
      |> maybe_awaiting_confirmation()

    response =
      {:ok,
       %{
         ready: result.ready?,
         missing: result.missing,
         errors: result.errors,
         revision: spec_revision(state)
       }}

    {response, state}
  end

  defp perform_write("submit_spec", _args, _identity, state),
    do:
      {mcp_error(
         "invalid_state",
         "submit_spec is only allowed before the Campaign Spec is confirmed"
       ), state}

  defp perform_write("submit_harness", args, %{workflow: :alignment}, state)
       when state.status in [:drafting_spec, :awaiting_confirmation] do
    with true <- state.spec_result.ready?,
         :ok <- validate_harness_definition(state.spec_result.spec, args),
         {:ok, harness} <- Harness.validate(state.workspace.setup_worktree, args) do
      inherited_target_snapshot = state.target_snapshot || state.inherited_target_snapshot

      state =
        state
        |> invalidate_implementation_definition()
        |> Map.put(:harness, harness)
        |> Map.put(:inherited_target_snapshot, inherited_target_snapshot)
        |> Map.update!(:required, &MapSet.delete(&1, "submit_harness"))
        |> maybe_awaiting_confirmation()

      {{:ok, %{digest: harness.digest, protected_paths: harness.protected_paths}}, state}
    else
      false ->
        {mcp_error("invalid_state", "submit_harness requires a valid Campaign Spec"), state}

      {:error, reason} ->
        {mcp_error("missing_required_data", "Harness validation failed", %{reason: reason}),
         state}
    end
  end

  defp perform_write("submit_harness", _args, _identity, state),
    do:
      {mcp_error(
         "invalid_state",
         "submit_harness is only allowed before the Campaign Spec is confirmed"
       ), state}

  defp perform_write("submit_implementation_review", args, %{workflow: :alignment}, state)
       when state.status in [:drafting_spec, :awaiting_confirmation] do
    if state.spec_result.ready? and not is_nil(state.harness) and
         not is_nil(state.target_snapshot) and not is_nil(state.prepared_setup_sha) do
      with {:ok, _review} <- load_implementation_review(state),
           :ok <- Harness.verify_digest(state.workspace.setup_worktree, state.harness),
           :ok <- TargetSnapshot.verify(state.workspace.root, state.target_snapshot),
           :ok <- verify_prepared_setup(state),
           {:ok, artifact} <-
             verified_registered_artifact(
               state,
               args["output_artifact"],
               "implementation_review_evidence"
             ),
           {:ok, evidence} <-
             ImplementationReviewEvidence.validate(
               state.spec_result.spec,
               state.harness,
               state.target_snapshot,
               state.prepared_setup_sha,
               args,
               artifact
             ) do
        state =
          %{
            state
            | implementation_review_evidence: evidence,
              required: MapSet.delete(state.required, "submit_implementation_review")
          }
          |> maybe_awaiting_confirmation()

        response = %{
          digest: evidence.digest,
          case_id: evidence.case_id,
          metrics: evidence.metrics,
          ready_for_user_review: state.status == :awaiting_confirmation
        }

        {{:ok, response}, state}
      else
        {:error, reason} ->
          {mcp_error(
             "missing_required_data",
             "Implementation review evidence validation failed",
             %{reason: reason}
           ), state}
      end
    else
      {mcp_error(
         "invalid_state",
         "submit_implementation_review requires a prepared Target, Development commit, valid Campaign Spec, and Harness"
       ), state}
    end
  end

  defp perform_write("submit_implementation_review", _args, %{workflow: :alignment}, state),
    do:
      {mcp_error(
         "invalid_state",
         "submit_implementation_review is only allowed before the Campaign Spec is confirmed"
       ), state}

  defp perform_write("submit_implementation_review", _args, _identity, state),
    do:
      {mcp_error(
         "forbidden_role",
         "submit_implementation_review requires an alignment session before Spec confirmation"
       ), state}

  defp perform_write("complete_setup_merge", args, %{workflow: :alignment}, state) do
    if setup_merge_required?(state) do
      state = %{state | status: :building_baseline, last_error: nil}

      verification =
        with true <- args["setup_sha"] == state.prepared_setup_sha,
             true <- args["target_snapshot_id"] == state.target_snapshot.id,
             :ok <- TargetSnapshot.verify(state.workspace.root, state.target_snapshot),
             :ok <-
               Workspace.verify_setup_merge(
                 state.workspace,
                 state.setup_base_sha,
                 args["base_sha"],
                 args["setup_sha"],
                 args["best_sha"]
               ) do
          :ok
        else
          false -> {:error, :reviewed_implementation_identity_mismatch}
          {:error, reason} -> {:error, reason}
        end

      case verification do
        :ok ->
          implementation_verification =
            with :ok <- Harness.verify_digest(state.workspace.repo, state.harness),
                 :ok <-
                   TargetSnapshot.link(
                     state.workspace.root,
                     state.workspace.repo,
                     state.target_snapshot
                   ) do
              :ok
            end

          case implementation_verification do
            :ok ->
              old_session_id = state.backend_session && state.backend_session.id
              old_token_hash = state.backend_token_hash

              if state.backend,
                do: Task.start(fn -> AgentBackend.close_session(state.backend) end)

              state = %{
                state
                | best_sha: args["best_sha"],
                  setup_sha: args["setup_sha"],
                  required: MapSet.new(["submit_baseline"]),
                  backend: nil,
                  backend_session: nil,
                  provider_session_id: nil,
                  backend_token_hash: nil,
                  closed_sessions:
                    if(old_session_id,
                      do: MapSet.put(state.closed_sessions, old_session_id),
                      else: state.closed_sessions
                    ),
                  active_turn_id: nil,
                  messages:
                    state.messages ++ [message(:system, "Setup merge 已核验；启动 Baseline Session。")]
              }

              state =
                if state.backend_enabled do
                  %{state | mcp_tokens: Map.delete(state.mcp_tokens, old_token_hash)}
                else
                  %{
                    state
                    | mcp_tokens:
                        Map.new(state.mcp_tokens, fn {hash, identity} ->
                          {hash, %{identity | workflow: :baseline}}
                        end)
                  }
                end

              state =
                if state.backend_enabled,
                  do: begin_open_session(state, :baseline, state.workspace.repo),
                  else: state

              {{:ok,
                %{
                  best_sha: state.best_sha,
                  development_baseline_sha: state.best_sha,
                  target_snapshot_id: state.target_snapshot.id,
                  protected_digest: state.harness.digest
                }}, state}

            {:error, reason} ->
              {mcp_error(
                 "protected_path_changed",
                 "protected Harness or Target link verification failed",
                 %{reason: reason}
               ), state}
          end

        {:error, reason} ->
          {mcp_error("invalid_state", "setup merge verification failed", %{reason: reason}),
           state}
      end
    else
      {mcp_error(
         "invalid_state",
         "complete_setup_merge is unavailable for the current Campaign state",
         %{
           status: state.status,
           required_operations: state.required |> MapSet.to_list() |> Enum.sort()
         }
       ), state}
    end
  end

  defp perform_write("complete_setup_merge", _args, _identity, state),
    do: {mcp_error("forbidden_role", "complete_setup_merge requires alignment session"), state}

  defp perform_write(
         "reopen_baseline_definition",
         args,
         %{workflow: :baseline},
         state
       ) do
    reason = trimmed_text(args["reason"])
    requested_changes = trimmed_text(args["requested_changes"])

    cond do
      reason == "" or requested_changes == "" ->
        {mcp_error(
           "missing_required_data",
           "reason and requested_changes must both be non-empty"
         ), state}

      baseline_definition_reopenable?(state) ->
        handoff = "原因：#{reason}\n请求修改：#{requested_changes}"

        case reopen_confirmed_spec(state, handoff, [], :baseline_agent) do
          {:ok, next_state} ->
            revision = spec_revision(next_state)

            {{:ok,
              %{
                status: "drafting_spec",
                revision: revision,
                setup_branch: "pika/setup/#{revision}",
                required_operations:
                  ~w(submit_harness submit_implementation_bundle submit_implementation_review submit_spec),
                next_action:
                  "the Baseline Session is closing; the Alignment Agent will revise the definition"
              }}, next_state}

          {:error, reopen_reason} ->
            {mcp_error("invalid_state", "Baseline definition could not be reopened", %{
               reason: reopen_reason
             }), state}
        end

      true ->
        {mcp_error(
           "invalid_state",
           "reopen_baseline_definition requires an active Baseline workflow before Optimizing",
           %{
             status: state.status,
             required_operations: state.required |> MapSet.to_list() |> Enum.sort()
           }
         ), state}
    end
  end

  defp perform_write("reopen_baseline_definition", _args, _identity, state),
    do:
      {mcp_error(
         "forbidden_role",
         "reopen_baseline_definition requires a baseline session"
       ), state}

  defp perform_write("submit_iteration_sample", args, %{workflow: :baseline}, state) do
    if state.status == :selecting_iteration_sample and
         MapSet.member?(state.required, "submit_iteration_sample") do
      case Sampling.initial(state.spec_result.spec, args) do
        {:ok, sampling} ->
          state = %{
            state
            | status: :optimizing,
              iteration_sampling: sampling,
              sampling_revisions: state.sampling_revisions ++ [sampling],
              required: MapSet.new(),
              last_error: nil,
              messages:
                state.messages ++
                  [
                    message(
                      :system,
                      "Iteration Sample v#{sampling.revision} 已建立：#{length(sampling.case_ids)} / #{length(state.spec_result.spec["benchmark_cases"])} Cases。Alignment 流程已进入 Optimizing。"
                    )
                  ]
          }

          {{:ok,
            %{
              status: "optimizing",
              sampling_revision: sampling.revision,
              case_ids: sampling.case_ids
            }}, state}

        {:error, reason} ->
          {mcp_error("missing_required_data", "Iteration Sample validation failed", %{
             reason: reason
           }), state}
      end
    else
      {mcp_error("invalid_state", "submit_iteration_sample is not currently required"), state}
    end
  end

  defp perform_write("submit_iteration_sample", _args, _identity, state),
    do: {mcp_error("forbidden_role", "submit_iteration_sample requires baseline session"), state}

  defp setup_merge_required?(state) do
    MapSet.member?(state.required, "complete_setup_merge") and
      (state.status == :building_baseline or confirmed_legacy_draft?(state))
  end

  defp baseline_definition_reopenable?(state) do
    state.status in [:building_baseline, :selecting_iteration_sample] and
      Enum.any?(~w(submit_baseline submit_iteration_sample), &MapSet.member?(state.required, &1))
  end

  defp confirmed_legacy_draft?(state) do
    state.status == :drafting_spec and is_binary(state.pending_confirmation_input) and
      state.spec_result.ready? and not is_nil(state.harness)
  end

  defp start_target_submission(
         _args,
         _identity,
         _key,
         _request_hash,
         _record_key,
         %{target_submission: submission} = state
       )
       when not is_nil(submission) do
    {:reply,
     mcp_error(
       "target_preparation_in_progress",
       "an Optimization Target snapshot is already being prepared",
       %{submission_id: submission.id}
     ), state}
  end

  defp start_target_submission(
         args,
         %{workflow: :alignment} = identity,
         key,
         request_hash,
         record_key,
         state
       ) do
    setup_sha = args["setup_sha"]

    with true <- state.status in [:drafting_spec, :awaiting_confirmation],
         true <- MapSet.member?(state.required, "submit_implementation_bundle"),
         true <- state.spec_result.ready?,
         true <- not is_nil(state.harness),
         true <- is_binary(setup_sha) and setup_sha != "" do
      submission_id = Pika.AgentBackend.Id.new("target")
      parent = self()
      workspace_root = state.workspace.root
      setup_root = state.workspace.setup_worktree
      revision = spec_revision(state)
      spec = state.spec_result.spec
      references = state.references
      inherited = state.inherited_target_snapshot

      {:ok, task_pid} =
        Task.start(fn ->
          send(parent, {
            :target_progress,
            submission_id,
            %{phase: :resolving_source, started_at: DateTime.utc_now()}
          })

          result =
            with {:ok, resolved_references} <-
                   prepare_target_references(
                     workspace_root,
                     spec,
                     references,
                     inherited,
                     fn progress ->
                       send(parent, {
                         :target_progress,
                         submission_id,
                         Map.put(progress, :phase, :materializing_source)
                       })
                     end
                   ),
                 _ <-
                   send(parent, {
                     :target_progress,
                     submission_id,
                     %{phase: :freezing_snapshot}
                   }),
                 {:ok, snapshot} <-
                   TargetSnapshot.prepare(
                     workspace_root,
                     revision,
                     setup_root,
                     setup_sha,
                     spec,
                     resolved_references,
                     inherited_snapshot: inherited
                   ),
                 :ok <- TargetSnapshot.link(workspace_root, setup_root, snapshot) do
              {:ok, %{snapshot: snapshot, references: resolved_references}}
            end

          send(parent, {:target_finished, submission_id, result})
        end)

      monitor_ref = Process.monitor(task_pid)

      submission = %{
        id: submission_id,
        pid: task_pid,
        monitor_ref: monitor_ref,
        args: args,
        identity: identity,
        key: key,
        request_hash: request_hash,
        record_key: record_key
      }

      response =
        {:ok,
         %{
           status: "preparing_optimization_target",
           submission_id: submission_id,
           next_action:
             "wait for Pika to freeze the Target, then run and submit implementation review evidence"
         }}

      next_state = %{
        state
        | target_submission: submission,
          target_progress: %{phase: :queued, started_at: DateTime.utc_now()},
          last_error: nil,
          messages:
            state.messages ++
              [
                message(
                  :system,
                  "正在后台固化 Optimization Target，并绑定 Development 提交 #{short_sha(setup_sha)}。"
                )
              ]
      }

      {:started, response, next_state}
    else
      false ->
        {:reply,
         mcp_error(
           "invalid_state",
           "submit_implementation_bundle requires a valid Spec, Harness, clean setup commit, and an open implementation-bundle gate"
         ), state}
    end
  end

  defp start_target_submission(_args, _identity, _key, _hash, _record_key, state) do
    {:reply,
     mcp_error("forbidden_role", "submit_implementation_bundle requires alignment session"),
     state}
  end

  defp prepare_target_references(workspace_root, spec, references, inherited, on_progress)
       when is_map(inherited) do
    if TargetSnapshot.reusable?(spec, inherited) do
      {:ok, references}
    else
      prepare_target_references(workspace_root, spec, references, nil, on_progress)
    end
  end

  defp prepare_target_references(workspace_root, spec, references, nil, on_progress) do
    source = get_in(spec, ["implementations", "optimization_target", "source"]) || %{}

    case source["kind"] do
      "development_snapshot" ->
        {:ok, references}

      "reference_project" ->
        id = source["reference_id"]

        with %{} = entry <- Enum.find(references, &(&1.id == id)),
             {:ok, [resolved]} <- ReferenceCatalog.resolve_selected([%{entry | selected: true}]),
             {:ok, [materialized]} <-
               ReferenceCatalog.materialize_selected(workspace_root, [resolved],
                 max_concurrency: 1,
                 on_progress: on_progress
               ) do
          {:ok, replace_reference(references, materialized)}
        else
          nil -> {:error, {:target_reference_not_found, id}}
          {:error, failed} -> {:error, {:target_reference_preparation_failed, id, failed}}
          other -> {:error, {:target_reference_preparation_failed, id, other}}
        end

      kind ->
        {:error, {:invalid_target_source, kind}}
    end
  end

  defp replace_reference(references, replacement) do
    Enum.map(references, fn reference ->
      if reference.id == replacement.id,
        do: %{replacement | selected: reference.selected},
        else: reference
    end)
  end

  defp finish_target_submission(
         {:ok, %{snapshot: snapshot, references: references}},
         args,
         state
       ) do
    state = %{
      state
      | target_snapshot: snapshot,
        inherited_target_snapshot: nil,
        prepared_setup_sha: args["setup_sha"],
        references: references,
        implementation_review_evidence: nil,
        required:
          state.required
          |> MapSet.delete("submit_implementation_bundle")
          |> MapSet.put("submit_implementation_review"),
        last_error: nil,
        messages:
          state.messages ++
            [
              message(
                :system,
                "Optimization Target #{snapshot.id} 已固化；请在同一个 Case 上校验 Target 与 Development 的正确性和配对性能。"
              )
            ]
    }

    state = maybe_awaiting_confirmation(state)

    {{:ok,
      %{
        status: "awaiting_implementation_review",
        target_snapshot: TargetSnapshot.public(snapshot),
        development_sha: state.prepared_setup_sha,
        required_operations: state.required |> MapSet.to_list() |> Enum.sort()
      }}, state}
  end

  defp finish_target_submission({:error, reason}, _args, state) do
    {mcp_error("missing_required_data", "Optimization Target preparation failed", %{
       reason: reason
     }), %{state | last_error: "Optimization Target 准备失败：#{inspect(reason)}"}}
  end

  defp notify_target_result(state, {:ok, result}) do
    dispatch_input(
      state,
      "Pika has frozen Optimization Target #{result.target_snapshot.id} and Development #{result.development_sha}. Run both against the Correctness Oracle on the same Benchmark Case, collect paired Target/Development Metrics, register the output as implementation_review_evidence, and call submit_implementation_review."
    )
  end

  defp notify_target_result(state, {:error, _code, message, details}) do
    dispatch_input(
      state,
      "Optimization Target preparation failed: #{message}; details=#{inspect(details)}. Correct the setup and call submit_implementation_bundle again with a new idempotency key."
    )
  end

  defp short_sha(value) when is_binary(value), do: String.slice(value, 0, 12)
  defp short_sha(value), do: inspect(value)

  defp start_baseline_submission(
         _args,
         _identity,
         _key,
         _request_hash,
         _record_key,
         %{baseline_submission: submission}
       )
       when not is_nil(submission) do
    {:transient_error,
     mcp_error(
       "baseline_validation_in_progress",
       "a Baseline submission is already being validated",
       %{submission_id: submission.id}
     )}
  end

  defp start_baseline_submission(
         args,
         %{workflow: :baseline} = identity,
         key,
         request_hash,
         record_key,
         state
       ) do
    with true <- state.status == :building_baseline,
         true <- MapSet.member?(state.required, "submit_baseline"),
         true <- args["measured_sha"] == state.best_sha,
         true <- args["target_snapshot_id"] == state.target_snapshot.id,
         :ok <- TargetSnapshot.verify(state.workspace.root, state.target_snapshot),
         true <- Pika.Git.clean?(state.workspace.repo),
         {:ok, inputs} <- baseline_inputs(state, args),
         :ok <-
           validate_profiler_dependencies(
             state,
             inputs.profiler,
             inputs.profiler_dependencies
           ),
         :ok <- validate_optional_profiler_manifest(state, inputs.profiler) do
      submission_id = Pika.AgentBackend.Id.new("baseline")
      parent = self()
      spec = state.spec_result.spec
      best_sha = state.best_sha
      target_snapshot_id = state.target_snapshot.id
      skill_sha = state.skill.sha
      workspace_root = state.workspace.root

      {:ok, task_pid} =
        Task.start(fn ->
          if inputs.manifest do
            send(parent, {
              :baseline_progress,
              submission_id,
              %{phase: :registering_artifacts}
            })
          end

          result =
            with {:ok, artifacts} <- register_manifest_artifacts(workspace_root, inputs),
                 {:ok, baseline} <-
                   Baseline.evaluate(
                     inputs.samples,
                     inputs.correctness,
                     inputs.profiler,
                     spec,
                     best_sha,
                     skill_sha,
                     expected_samples: inputs.expected_samples,
                     target_snapshot_id: target_snapshot_id,
                     on_progress: fn progress ->
                       send(parent, {:baseline_progress, submission_id, progress})
                     end
                   ),
                 {:ok, sample_artifacts} <-
                   verified_sample_artifacts(workspace_root, inputs, baseline) do
              {:ok, %{baseline: baseline, artifacts: artifacts ++ sample_artifacts}}
            end

          send(parent, {:baseline_finished, submission_id, result})
        end)

      monitor_ref = Process.monitor(task_pid)
      total_groups = length(spec["benchmark_cases"]) * length(spec["metrics"])
      pair_count = get_in(spec, ["benchmark", "pair_count"])

      progress = %{
        phase: :queued,
        processed_records: 0,
        total_records: total_groups * pair_count,
        processed_bytes: 0,
        total_bytes: inputs.samples_size,
        completed_groups: 0,
        total_groups: total_groups,
        case_id: nil,
        metric_id: nil,
        started_at: DateTime.utc_now()
      }

      submission = %{
        id: submission_id,
        pid: task_pid,
        monitor_ref: monitor_ref,
        args: args,
        identity: identity,
        key: key,
        request_hash: request_hash,
        record_key: record_key
      }

      response =
        {:ok,
         %{
           status: "validating_baseline",
           submission_id: submission_id,
           total_records: progress.total_records,
           total_groups: total_groups,
           next_action: "wait for Pika to finish validation; do not resubmit"
         }}

      state = %{
        state
        | baseline_submission: submission,
          baseline_progress: progress,
          baseline_error: nil,
          messages:
            state.messages ++
              [
                message(
                  :system,
                  "已接收全量 Baseline，正在后台流式校验 #{progress.total_records} 条 Pair 记录；页面和 MCP 状态查询保持可用。"
                )
              ]
      }

      {:started, response, state}
    else
      {:error, reason} ->
        {:reply,
         mcp_error("missing_required_data", "Baseline validation could not start", %{
           reason: reason
         }), %{state | baseline_error: inspect(reason)}}

      false ->
        {:reply,
         mcp_error(
           "invalid_state",
           "Baseline state, SHA, required operation, or temporary Best cleanliness is invalid"
         ), state}
    end
  end

  defp start_baseline_submission(_args, _identity, _key, _request_hash, _record_key, state) do
    {:reply, mcp_error("forbidden_role", "submit_baseline requires baseline session"), state}
  end

  defp finish_baseline_submission(
         {:ok, %{baseline: result, artifacts: artifacts}},
         args,
         state
       ) do
    state = register_user_artifacts(state, artifacts)

    state = %{
      state
      | status: :selecting_iteration_sample,
        baseline: Map.put(result, :summary, args["summary"]),
        baseline_error: nil,
        required: MapSet.new(["submit_iteration_sample"]),
        messages:
          state.messages ++
            [
              message(
                :system,
                "全量 Baseline 已通过；等待 Agent 从 #{length(state.spec_result.spec["benchmark_cases"])} 个 Cases 中选择初始 Iteration Sample。"
              )
            ]
    }

    {{:ok,
      %{
        status: "selecting_iteration_sample",
        metric_count: length(result.metrics),
        profiler: result.profiler,
        max_initial_cases:
          get_in(state.spec_result.spec, ["iteration_sampling", "max_initial_cases"])
      }}, state}
  end

  defp finish_baseline_submission(
         {:error, {:insufficient_valid_pairs, _, _, _} = reason},
         _args,
         state
       ),
       do: baseline_retry(reason, state)

  defp finish_baseline_submission({:error, reason}, _args, state) do
    {mcp_error("missing_required_data", "Baseline validation failed", %{reason: reason}),
     %{state | baseline_error: inspect(reason)}}
  end

  defp normalize_baseline_args(state, %{"manifest_artifact" => relative_path} = args)
       when is_binary(relative_path) and relative_path != "" do
    with {:ok, manifest} <- BaselineManifest.load(state.workspace.root, relative_path) do
      {:ok,
       args
       |> Map.merge(%{
         "measured_sha" => manifest.candidate_sha,
         "target_snapshot_id" => manifest.target_snapshot_id,
         "summary" => manifest.summary,
         "samples_artifact" => manifest.samples_artifact,
         "correctness_artifact" => manifest.correctness_artifact,
         "profiler_artifact" => manifest.profiler_artifact,
         "__manifest_sha256" => manifest.manifest_artifact.sha256,
         "__baseline_manifest" => manifest
       })}
    end
  end

  defp normalize_baseline_args(_state, _args), do: {:error, :baseline_manifest_v2_required}

  defp baseline_inputs(state, %{"__baseline_manifest" => manifest}) do
    with {:ok, samples} <- ArtifactStore.resolve(state.workspace.root, manifest.samples_artifact),
         {:ok, correctness} <-
           ArtifactStore.resolve(state.workspace.root, manifest.correctness_artifact),
         {:ok, profiler} <-
           resolve_optional_artifact(state.workspace.root, manifest.profiler_artifact),
         {:ok, stat} <- File.stat(samples),
         true <- stat.type == :regular do
      {:ok,
       %{
         manifest: manifest,
         samples: samples,
         samples_relative: manifest.samples_artifact,
         samples_size: stat.size,
         correctness: correctness,
         profiler: profiler,
         profiler_dependencies: manifest.profiler_dependencies,
         expected_samples: nil
       }}
    else
      false -> {:error, :baseline_samples_not_regular}
      {:error, reason} -> {:error, reason}
    end
  end

  defp baseline_inputs(state, args) do
    with {:ok, samples} <- registered_path(state, args["samples_artifact"]),
         {:ok, correctness} <- registered_path(state, args["correctness_artifact"]),
         {:ok, profiler} <- registered_optional_path(state, args["profiler_artifact"]),
         %{size: size} = sample_artifact <- Map.get(state.artifacts, args["samples_artifact"]) do
      {:ok,
       %{
         manifest: nil,
         samples: samples,
         samples_relative: args["samples_artifact"],
         samples_size: size,
         correctness: correctness,
         profiler: profiler,
         profiler_dependencies: [],
         expected_samples: sample_artifact
       }}
    else
      nil -> {:error, {:artifact_not_registered, args["samples_artifact"]}}
      {:error, reason} -> {:error, reason}
    end
  end

  defp register_manifest_artifacts(_workspace_root, %{manifest: nil}), do: {:ok, []}

  defp register_manifest_artifacts(workspace_root, inputs) do
    references =
      [{inputs.manifest.correctness_artifact, "baseline_correctness"}] ++
        optional_profiler_references(inputs.manifest)

    Enum.reduce_while(
      references,
      {:ok, [inputs.manifest.manifest_artifact]},
      fn {relative_path, kind}, {:ok, artifacts} ->
        case ArtifactStore.register(workspace_root, relative_path, kind: kind) do
          {:ok, artifact} ->
            {:cont, {:ok, [artifact | artifacts]}}

          {:error, reason} ->
            {:halt, {:error, {:artifact_registration_failed, relative_path, reason}}}
        end
      end
    )
    |> case do
      {:ok, artifacts} -> {:ok, Enum.reverse(artifacts)}
      error -> error
    end
  end

  defp verified_sample_artifacts(_workspace_root, %{manifest: nil}, _baseline), do: {:ok, []}

  defp verified_sample_artifacts(workspace_root, inputs, baseline) do
    sample = baseline.samples_artifact

    case ArtifactStore.verified(
           workspace_root,
           inputs.samples_relative,
           sample.sha256,
           sample.size,
           kind: "baseline_samples",
           mime: "application/x-ndjson"
         ) do
      {:ok, artifact} -> {:ok, [artifact]}
      {:error, reason} -> {:error, {:sample_artifact_registration_failed, reason}}
    end
  end

  defp notify_baseline_result(%{status: :selecting_iteration_sample} = state, _response) do
    dispatch_input(
      state,
      "Pika finished validating the full Baseline successfully. Call get_context, then submit_iteration_sample."
    )
  end

  defp notify_baseline_result(state, response) do
    dispatch_input(
      state,
      "Pika finished validating the Baseline with this result: #{inspect(response, limit: 8)}. Call get_context, correct the artifacts if needed, and retry submit_baseline."
    )
  end

  defp baseline_retry(reason, %{baseline_retry_count: 0} = state) do
    min_valid_pairs =
      state.spec_result.spec
      |> Map.fetch!("benchmark")
      |> Map.fetch!("min_valid_pairs")

    state = %{
      state
      | baseline_retry_count: 1,
        baseline_error: inspect(reason),
        messages:
          state.messages ++
            [message(:system, "有效 Pair 少于 #{min_valid_pairs}；允许且要求整组重跑一次。")]
    }

    {mcp_error("missing_required_data", "rerun the entire Baseline group once", %{
       reason: reason,
       retry: 1
     }), state}
  end

  defp baseline_retry(reason, state) do
    state = %{state | baseline_error: inspect(reason)}

    {mcp_error("blocked", "Baseline remained insufficient after the only retry", %{reason: reason}),
     state}
  end

  defp registered_path(state, relative_path) do
    case Map.get(state.artifacts, relative_path) do
      nil -> {:error, {:artifact_not_registered, relative_path}}
      _artifact -> ArtifactStore.resolve(state.workspace.root, relative_path)
    end
  end

  defp resolve_optional_artifact(_workspace_root, nil), do: {:ok, nil}

  defp resolve_optional_artifact(workspace_root, relative_path),
    do: ArtifactStore.resolve(workspace_root, relative_path)

  defp registered_optional_path(_state, nil), do: {:ok, nil}
  defp registered_optional_path(state, relative_path), do: registered_path(state, relative_path)

  defp optional_profiler_references(%{profiler_artifact: nil, profiler_dependencies: []}), do: []

  defp optional_profiler_references(manifest) do
    [{manifest.profiler_artifact, "baseline_profiler"}] ++
      Enum.map(manifest.profiler_dependencies, &{&1, "baseline_profiler_dependency"})
  end

  defp validate_profiler_dependencies(_state, nil, []), do: :ok

  defp validate_profiler_dependencies(state, profiler_path, additional_paths) do
    available_paths =
      state.artifacts
      |> Map.keys()
      |> Kernel.++(additional_paths)
      |> MapSet.new()

    with {:ok, body} <- File.read(profiler_path),
         {:ok, manifest} <- Jason.decode(body),
         report_paths when is_list(report_paths) <- manifest["report_paths"],
         %{"output_paths" => parser_paths} when is_list(parser_paths) <- manifest["parser"],
         evidence_paths when is_list(evidence_paths) <- manifest["remote_evidence_paths"] do
      missing =
        (report_paths ++ parser_paths ++ evidence_paths)
        |> Enum.reject(&MapSet.member?(available_paths, &1))

      if missing == [], do: :ok, else: {:error, {:profiler_dependencies_not_registered, missing}}
    else
      _ ->
        {:error,
         {:invalid_profiler_dependencies,
          "report_paths, parser.output_paths, and remote_evidence_paths must be lists"}}
    end
  end

  defp validate_optional_profiler_manifest(_state, nil), do: :ok

  defp validate_optional_profiler_manifest(state, profiler_path) do
    target_case_ids =
      for case_ <- state.spec_result.spec["benchmark_cases"],
          case_["kind"] == "target",
          do: case_["id"]

    case Pika.Profiler.validate_manifest(
           profiler_path,
           target_case_ids,
           state.best_sha,
           state.skill.sha
         ) do
      {:ok, _manifest} -> :ok
      {:error, reason} -> {:error, reason}
    end
  end

  defp draft_required_operations,
    do:
      MapSet.new(
        ~w(submit_spec submit_harness submit_implementation_bundle submit_implementation_review)
      )

  defp validate_harness_definition(spec, args) do
    oracle = get_in(spec, ["implementations", "oracle"]) || %{}
    development_path = get_in(spec, ["implementations", "development", "entrypoint"])
    expected_oracle_path = if oracle["kind"] == "repository_path", do: oracle["entrypoint"]
    protected_paths = List.wrap(args["protected_paths"])

    cond do
      args["oracle_path"] != expected_oracle_path ->
        {:error, {:oracle_path_mismatch, expected_oracle_path, args["oracle_path"]}}

      args["benchmark_path"] != get_in(spec, ["benchmark", "harness_path"]) ->
        {:error,
         {:benchmark_path_mismatch, get_in(spec, ["benchmark", "harness_path"]),
          args["benchmark_path"]}}

      development_path in protected_paths ->
        {:error, {:development_entrypoint_must_remain_mutable, development_path}}

      true ->
        :ok
    end
  end

  defp target_definition(spec) when is_map(spec),
    do: get_in(spec, ["implementations", "optimization_target"])

  defp target_definition(_spec), do: nil

  defp invalidate_implementation_definition(state) do
    stop_target_submission(state.target_submission)

    %{
      state
      | reference_review_evidence: nil,
        implementation_review_evidence: nil,
        prepared_setup_sha: nil,
        target_snapshot: nil,
        target_submission: nil,
        target_progress: nil,
        required:
          state.required
          |> MapSet.put("submit_implementation_bundle")
          |> MapSet.put("submit_implementation_review"),
        status: :drafting_spec
    }
  end

  defp require_review_match(value, value, _error) when not is_nil(value), do: :ok
  defp require_review_match(_reviewed, _current, error), do: {:error, error}

  defp verify_implementation_review_evidence(%{implementation_review_evidence: nil}),
    do: {:error, :implementation_review_evidence_missing}

  defp verify_implementation_review_evidence(state) do
    evidence = state.implementation_review_evidence

    with {:ok, artifact} <-
           verified_registered_artifact(
             state,
             evidence.output_artifact,
             "implementation_review_evidence"
           ),
         :ok <-
           ImplementationReviewEvidence.verify(
             state.spec_result.spec,
             state.harness,
             state.target_snapshot,
             state.prepared_setup_sha,
             evidence,
             artifact
           ) do
      :ok
    end
  end

  defp verified_registered_artifact(state, relative_path, kind) do
    case Map.get(state.artifacts, relative_path) do
      %{kind: ^kind} = artifact ->
        case ArtifactStore.register(state.workspace.root, relative_path, %{
               sha256: artifact.sha256,
               size: artifact.size,
               kind: artifact.kind,
               mime: artifact.mime,
               metadata: artifact.metadata
             }) do
          {:ok, _verified} -> {:ok, artifact}
          {:error, reason} -> {:error, {:artifact_verification_failed, relative_path, reason}}
        end

      nil ->
        {:error, {:artifact_not_registered, relative_path}}

      _artifact ->
        {:error, {:artifact_kind_mismatch, relative_path, kind}}
    end
  end

  defp maybe_awaiting_confirmation(state) do
    if state.spec_result.ready? and not is_nil(state.harness) and
         not is_nil(state.target_snapshot) and not is_nil(state.prepared_setup_sha) and
         not is_nil(state.implementation_review_evidence) and
         MapSet.equal?(state.required, MapSet.new()) do
      %{state | status: :awaiting_confirmation}
    else
      %{state | status: :drafting_spec}
    end
  end

  defp require_confirmation_idle(%{pending_questions: pending}) when not is_nil(pending),
    do: {:error, :questions_pending}

  defp require_confirmation_idle(%{agent_responding: true}),
    do: {:error, :agent_still_responding}

  defp require_confirmation_idle(%{active_turn_id: turn_id}) when is_binary(turn_id),
    do: {:error, :agent_still_responding}

  defp require_confirmation_idle(_state), do: :ok

  defp load_implementation_review(%{harness: nil}),
    do: {:error, :implementation_bundle_not_ready}

  defp load_implementation_review(%{target_snapshot: nil}),
    do: {:error, :implementation_bundle_not_ready}

  defp load_implementation_review(%{prepared_setup_sha: nil}),
    do: {:error, :implementation_bundle_not_ready}

  defp load_implementation_review(state) do
    implementations = state.spec_result.spec["implementations"]
    oracle = implementations["oracle"]
    development = implementations["development"]
    target_root = TargetSnapshot.checkout_path(state.workspace.root, state.target_snapshot)

    with :ok <- TargetSnapshot.verify(state.workspace.root, state.target_snapshot),
         :ok <- verify_prepared_setup(state),
         {:ok, target_review} <-
           Harness.source_review(target_root, state.target_snapshot.entrypoint),
         {:ok, development_review} <-
           Harness.source_review(state.workspace.setup_worktree, development["entrypoint"]),
         {:ok, oracle_review} <- load_oracle_review(state, oracle, target_review) do
      {:ok,
       %{
         target_snapshot: TargetSnapshot.public(state.target_snapshot),
         target: Map.put(target_review, :role, :optimization_target),
         development:
           development_review
           |> Map.put(:role, :development)
           |> Map.put(:sha, state.prepared_setup_sha),
         oracle: oracle_review
       }}
    end
  end

  defp load_oracle_review(_state, %{"kind" => "optimization_target"}, target_review),
    do: {:ok, Map.put(target_review, :role, :oracle)}

  defp load_oracle_review(
         state,
         %{"kind" => "repository_path", "entrypoint" => entrypoint},
         _target_review
       ) do
    case Harness.source_review(state.workspace.setup_worktree, entrypoint) do
      {:ok, review} -> {:ok, Map.put(review, :role, :oracle)}
      error -> error
    end
  end

  defp load_oracle_review(_state, _oracle, _target_review), do: {:error, :invalid_oracle}

  defp verify_prepared_setup(state) do
    with {:ok, actual} <- Pika.Git.head(state.workspace.setup_worktree),
         true <- actual == state.prepared_setup_sha,
         true <- Pika.Git.clean?(state.workspace.setup_worktree) do
      :ok
    else
      false ->
        {:error, :development_changed_after_review_preparation}

      {:ok, actual} ->
        {:error, {:development_sha_changed, state.prepared_setup_sha, actual}}

      {:error, reason} ->
        {:error, {:development_verification_failed, reason}}
    end
  end

  defp advance_confirmed_spec(state) do
    confirmation_input =
      "确认 Campaign Spec v#{spec_revision(state)}，并建立 Baseline。"

    state = %{
      state
      | status: :resolving_references,
        last_error: nil,
        pending_confirmation_input: confirmation_input,
        workflow_kickoffs: Map.put(state.workflow_kickoffs, :baseline, confirmation_input),
        messages: state.messages ++ [message(:user, confirmation_input)]
    }

    state = start_reference_resolution(state)

    broadcast(state)
    {:reply, :ok, state}
  end

  defp reopen_confirmed_spec(state, body, artifacts, requester \\ :user) do
    with {:ok, workspace, revision, setup_base_sha} <- prepare_reopened_workspace(state) do
      {input_body, request_messages, completion_message} =
        reopen_handoff(requester, body, artifacts, revision)

      input = user_input(input_body, artifacts)

      stop_baseline_submission(state.baseline_submission)
      stop_target_submission(state.target_submission)
      close_backend(state.backend)

      old_session_id = state.backend_session && state.backend_session.id
      old_token_hash = state.backend_token_hash

      spec =
        state.spec_result.spec
        |> Map.put("revision", revision)
        |> Map.drop(["reference_snapshot", "skill_snapshot"])

      state =
        state
        |> cancel_pending_questions(
          "spec_reopened",
          if(requester == :baseline_agent,
            do: "the Baseline Agent reopened the Campaign definition",
            else: "the user returned the Campaign to Spec drafting"
          )
        )
        |> register_user_artifacts(artifacts)

      mcp_tokens =
        if state.backend_enabled do
          if old_token_hash,
            do: Map.delete(state.mcp_tokens, old_token_hash),
            else: state.mcp_tokens
        else
          Map.new(state.mcp_tokens, fn {hash, identity} ->
            {hash, %{identity | workflow: :alignment}}
          end)
        end

      state = %{
        state
        | workspace: workspace,
          status: :drafting_spec,
          spec_result: Spec.validate(spec),
          spec_diff: Spec.diff(state.spec_result.spec, spec),
          harness: nil,
          reference_review_evidence: nil,
          implementation_review_evidence: nil,
          required: draft_required_operations(),
          setup_base_sha: setup_base_sha,
          setup_sha: nil,
          prepared_setup_sha: nil,
          inherited_target_snapshot: state.target_snapshot || state.inherited_target_snapshot,
          target_snapshot: nil,
          target_submission: nil,
          target_progress: nil,
          baseline: nil,
          baseline_submission: nil,
          baseline_progress: nil,
          baseline_retry_count: 0,
          baseline_error: nil,
          iteration_sampling: nil,
          sampling_revisions: [],
          pending_confirmation_input: nil,
          workflow_kickoffs:
            state.workflow_kickoffs
            |> Map.put(:alignment, input)
            |> Map.delete(:baseline),
          backend: nil,
          backend_session: nil,
          provider_session_id: nil,
          backend_token_hash: nil,
          backend_workflow: :alignment,
          mcp_tokens: mcp_tokens,
          closed_sessions:
            if(old_session_id,
              do: MapSet.put(state.closed_sessions, old_session_id),
              else: state.closed_sessions
            ),
          active_turn_id: nil,
          agent_responding: false,
          stream_message_id: nil,
          activity_message_id: nil,
          kickoff_dispatched: false,
          recovery_pending: false,
          last_error: nil,
          messages:
            state.messages ++
              request_messages ++ [message(:system, completion_message)]
      }

      state =
        if state.backend_enabled,
          do: begin_open_session(state, :alignment, workspace.setup_worktree),
          else: state

      {:ok, state}
    end
  end

  defp reopen_handoff(:user, body, artifacts, revision) do
    {
      "用户要求停止当前 Baseline，返回 Campaign Spec v#{revision} 修改：#{body}",
      [message(:user, "修改要求：#{body}", artifacts)],
      "已停止当前 Baseline，并返回 Campaign Spec v#{revision} 草稿；需要重新提交 Spec、Harness 并由用户确认。"
    }
  end

  defp reopen_handoff(:baseline_agent, body, _artifacts, revision) do
    {
      "Baseline Agent 通过 reopen_baseline_definition 判定冻结定义无法产生有效 Baseline，已返回 Campaign Spec v#{revision}。技术交接如下：\n#{body}\n请修改 Spec、Reference 与 Harness；仍需用户决定的边界必须使用 ask_questions 询问。",
      [message(:system, "Baseline Agent 请求修订冻结的 Baseline 定义：\n#{body}")],
      "Baseline Agent 已停止当前工作并自主返回 Campaign Spec v#{revision} 草稿；需要重新提交 Spec、Harness 并由用户确认。"
    }
  end

  defp prepare_reopened_workspace(state) do
    if setup_merge_completed?(state) do
      revision = spec_revision(state) + 1

      case Workspace.prepare_setup_revision(state.workspace, state.best_sha, revision) do
        {:ok, workspace} -> {:ok, workspace, revision, state.best_sha}
        {:error, reason} -> {:error, reason}
      end
    else
      {:ok, state.workspace, spec_revision(state), state.setup_base_sha}
    end
  end

  defp setup_merge_completed?(state) do
    not is_nil(state.setup_sha) or MapSet.member?(state.required, "submit_baseline") or
      state.best_sha != state.setup_base_sha
  end

  defp stop_baseline_submission(nil), do: :ok

  defp stop_baseline_submission(submission) do
    Process.demonitor(submission.monitor_ref, [:flush])
    if Process.alive?(submission.pid), do: Process.exit(submission.pid, :shutdown)
    :ok
  end

  defp stop_target_submission(nil), do: :ok

  defp stop_target_submission(submission) do
    Process.demonitor(submission.monitor_ref, [:flush])
    if Process.alive?(submission.pid), do: Process.exit(submission.pid, :shutdown)
    :ok
  end

  defp close_backend(nil), do: :ok
  defp close_backend(backend), do: Task.start(fn -> AgentBackend.close_session(backend) end)

  defp spec_revision(state) do
    case get_in(state, [:spec_result, :spec, "revision"]) do
      revision when is_integer(revision) and revision > 0 -> revision
      _ -> 1
    end
  end

  defp begin_open_session(state, workflow, cwd) do
    state = reload_backend_profile(state)
    skill_roots = [state.skill.path | state.skill_roots] |> Enum.uniq()

    case session_instructions(state, workflow, skill_roots) do
      {:ok, instructions} ->
        token = :crypto.strong_rand_bytes(32) |> Base.url_encode64(padding: false)
        token_hash = token_hash(token)

        identity = %{
          workflow: workflow,
          role: :boundary,
          session_key: Pika.AgentBackend.Id.new("alignment")
        }

        generation = state.backend_open_generation + 1

        state = %{
          state
          | mcp_tokens: Map.put(state.mcp_tokens, token_hash, identity),
            backend_token_hash: token_hash,
            backend_workflow: workflow,
            backend_open_generation: generation,
            kickoff_dispatched: false
        }

        parent = self()
        module = state.backend_module

        profile =
          state.backend_profile
          |> Map.put_new(:backend, state.backend_name)
          |> Map.put(:artifact_dir, Path.join(state.workspace.artifacts, "logs"))

        model = state.model
        effort = state.reasoning_effort

        mcp = %{
          url: state.mcp_url,
          token: token,
          resume_session_id: state.provider_session_id
        }

        Task.start(fn ->
          result =
            with {:ok, handle} <- AgentBackend.start_link(module, profile, parent),
                 {:ok, session} <-
                   AgentBackend.open_session(
                     handle,
                     cwd,
                     model,
                     effort,
                     mcp,
                     skill_roots,
                     instructions
                   ) do
              {:ok, handle, session}
            end

          send(parent, {:backend_opened, generation, workflow, result})
        end)

        state

      {:error, reason} ->
        %{state | last_error: "#{workflow} Agent Instructions 加载失败：#{inspect(reason)}"}
    end
  end

  defp reload_backend_profile(%{reload_backend_profile: false} = state), do: state

  defp reload_backend_profile(state) do
    workspace = Pika.WorkspaceLock.workspace()

    case Pika.RuntimeConfig.alignment_profile(workspace) do
      {:ok, profile} ->
        backend_name =
          if profile["type"] == "cursor_acp", do: :cursor_acp, else: :codex_app_server

        {command, args} = runtime_backend_command(backend_name, profile["command"])

        %{
          state
          | backend_name: backend_name,
            backend_module: backend_module(backend_name),
            backend_profile: %{
              command: command,
              args: args,
              approval_policy: profile["approval_policy"],
              sandbox_policy: profile["sandbox_policy"],
              protocol_config: profile["protocol_config"] || %{}
            },
            model: profile["model"],
            reasoning_effort: profile["reasoning_effort"] || :high,
            last_error: nil
        }

      {:error, reason} ->
        %{state | last_error: "Backend 配置热加载失败：#{inspect(reason)}"}
    end
  end

  defp runtime_backend_command(_backend, [command, "app-server", "--listen", "stdio://"]),
    do: {command, []}

  defp runtime_backend_command(_backend, [command, "acp"]), do: {command, []}
  defp runtime_backend_command(_backend, [command | args]), do: {command, args}
  defp runtime_backend_command(:cursor_acp, nil), do: {"cursor-agent", []}
  defp runtime_backend_command(_, nil), do: {"codex", []}

  defp start_reference_resolution(state) do
    parent = self()
    references = state.references
    resolve? = state.resolve_references
    materialize? = state.materialize_references
    workspace_root = state.workspace.root
    setup_worktree = state.workspace.setup_worktree
    total = Enum.count(references, & &1.selected)

    state = %{
      state
      | reference_progress: %{id: nil, completed: 0, total: total, status: :starting}
    }

    Task.start(fn ->
      resolve_missing? =
        materialize? and
          Enum.any?(references, &(&1.selected and not ReferenceCatalog.frozen?(&1)))

      resolved =
        if resolve? or resolve_missing?,
          do: ReferenceCatalog.resolve_selected(references),
          else: {:ok, references}

      result =
        case resolved do
          {:ok, entries} when materialize? ->
            ReferenceCatalog.materialize_selected(workspace_root, entries,
              link_into: setup_worktree,
              on_progress: fn progress ->
                send(parent, {:reference_materialization_progress, progress})
              end
            )

          value ->
            value
        end

      send(parent, {:references_resolved, result})
    end)

    state
  end

  defp update_reference_projects(state, references, change) do
    selected_ids = for reference <- references, reference.selected, do: reference.id
    spec_result = Spec.validate(Map.put(state.spec_result.spec, "reference_ids", selected_ids))

    state = invalidate_implementation_definition(state)

    state = %{
      state
      | references: references,
        spec_result: spec_result,
        status: :drafting_spec,
        required: MapSet.put(state.required, "submit_spec"),
        inherited_target_snapshot: nil,
        last_error: nil
    }

    if Map.has_key?(state.workflow_kickoffs, :alignment) do
      dispatch_input(
        state,
        "#{change}. Selected Reference Projects are now: #{Enum.join(selected_ids, ", ")}. Update and resubmit the Campaign Spec with submit_spec before asking for confirmation."
      )
    else
      state
    end
  end

  defp reference_failure_message(references) do
    failed_ids =
      for reference <- references,
          reference.selected and reference.status == :error,
          do: reference.id

    case failed_ids do
      [] -> "Reference 准备失败；未进入 Baseline。"
      ids -> "Reference 准备失败：#{Enum.join(ids, ", ")}。未进入 Baseline，可重试确认。"
    end
  end

  defp dispatch_input(%{backend: nil} = state, _input), do: state

  defp dispatch_input(state, input) do
    parent = self()
    backend = state.backend
    active? = is_binary(state.active_turn_id)
    generation = state.backend_open_generation

    Task.start(fn ->
      result =
        if active?,
          do: AgentBackend.steer(backend, input),
          else: AgentBackend.start_turn(backend, input)

      send(parent, {:backend_turn_result, generation, result})
    end)

    %{state | agent_responding: true}
  end

  defp apply_backend_event(state, %{type: :session_started}), do: state

  defp apply_backend_event(state, %{type: :turn_started, turn_id: turn_id}) do
    %{
      state
      | active_turn_id: turn_id,
        agent_responding: true,
        stream_message_id: nil,
        activity_message_id: nil
    }
  end

  defp apply_backend_event(state, %{type: :message_delta, data: data}) do
    delta = data[:delta] || data["delta"] || ""

    case state.stream_message_id do
      nil ->
        message = message(:agent, delta)
        id = message.id

        %{
          state
          | messages: state.messages ++ [message],
            stream_message_id: id,
            activity_message_id: nil,
            agent_responding: true
        }

      id ->
        messages =
          Enum.map(state.messages, fn
            %{id: ^id} = message -> %{message | content: message.content <> delta}
            message -> message
          end)

        %{state | messages: messages, agent_responding: true}
    end
  end

  defp apply_backend_event(state, %{type: type, data: data} = event)
       when type in [:tool_started, :tool_completed] do
    case activity_for_tool(type, data) do
      nil ->
        state

      activity ->
        activity =
          if Pika.CommandConsole.command?(event),
            do: Map.put(activity, :console_ref, Pika.CommandConsole.ref(event)),
            else: activity

        record_activity(state, activity)
    end
  end

  defp apply_backend_event(state, %{type: :plan_updated, data: data}) do
    record_activity(state, %{
      key: activity_key(data, "plan"),
      tags: ["plan"],
      label: "更新执行计划",
      detail: plan_detail(data),
      status: "completed"
    })
  end

  defp apply_backend_event(state, %{type: :file_changed, data: data}) do
    record_activity(state, %{
      key: activity_key(data, "file"),
      tags: ["edit"],
      label: "编辑文件",
      detail: file_change_detail(data),
      status: activity_status(data, :tool_completed)
    })
  end

  defp apply_backend_event(state, %{type: :backend_error, data: data}),
    do: %{
      state
      | messages: state.messages ++ [message(:system, "Backend 错误：#{summarize_event(data)}")]
    }

  defp apply_backend_event(state, %{type: type})
       when type in [:tool_updated, :command_output, :usage_updated],
       do: state

  defp apply_backend_event(state, %{type: :turn_completed, turn_id: turn_id}) do
    if not MapSet.equal?(state.required, MapSet.new()),
      do: Process.send_after(self(), {:completion_followup, turn_id}, 100)

    if state.active_turn_id in [nil, turn_id] do
      state
      |> cancel_pending_questions(
        "questions_cancelled",
        "the Agent turn ended before all answers"
      )
      |> Map.merge(%{
        active_turn_id: nil,
        agent_responding: false,
        stream_message_id: nil,
        activity_message_id: nil
      })
    else
      state
    end
  end

  defp apply_backend_event(state, %{type: :process_exited}) do
    if state.backend_enabled and state.status != :optimizing,
      do: Process.send_after(self(), :recover_backend, 100)

    state =
      cancel_pending_questions(
        state,
        "backend_exited",
        "the Backend exited before the user answered all questions"
      )

    %{
      state
      | backend: nil,
        backend_session: nil,
        backend_token_hash: nil,
        mcp_tokens:
          if(state.backend_token_hash,
            do: Map.delete(state.mcp_tokens, state.backend_token_hash),
            else: state.mcp_tokens
          ),
        active_turn_id: nil,
        agent_responding: false,
        recovery_pending: true,
        stream_message_id: nil,
        activity_message_id: nil,
        messages: state.messages ++ [message(:system, "Backend 进程退出；将尝试恢复原 Session。")]
    }
  end

  defp apply_backend_event(state, _event), do: state

  defp session_instructions(state, :alignment, skill_roots) do
    with {:ok, alignment} <-
           PromptCatalog.render(:alignment, instruction_assigns(state, :alignment)),
         {:ok, setup_merge} <-
           PromptCatalog.render(:setup_merge, instruction_assigns(state, :setup_merge)) do
      {:ok, append_skill_instructions(alignment <> "\n\n" <> setup_merge, skill_roots)}
    end
  end

  defp session_instructions(state, :baseline, skill_roots) do
    with {:ok, baseline} <-
           PromptCatalog.render(:baseline, instruction_assigns(state, :baseline)) do
      {:ok, append_skill_instructions(baseline, skill_roots)}
    end
  end

  defp instruction_assigns(state, :alignment) do
    %{
      setup_worktree: state.workspace.setup_worktree,
      source_sha: state.setup_base_sha,
      revision: spec_revision(state),
      setup_branch: "pika/setup/#{spec_revision(state)}"
    }
  end

  defp instruction_assigns(state, :setup_merge) do
    %{
      source_sha: state.setup_base_sha,
      revision: spec_revision(state),
      setup_branch: "pika/setup/#{spec_revision(state)}"
    }
  end

  defp instruction_assigns(state, :baseline) do
    benchmark = Map.fetch!(state.spec_result.spec, "benchmark")

    %{
      best_sha: state.best_sha,
      target_snapshot_id: state.target_snapshot.id,
      target_entrypoint: state.target_snapshot.entrypoint,
      repo: state.workspace.repo,
      artifacts: state.workspace.artifacts,
      pair_count: Map.fetch!(benchmark, "pair_count"),
      min_valid_pairs: Map.fetch!(benchmark, "min_valid_pairs"),
      max_initial_cases:
        get_in(state.spec_result.spec, ["iteration_sampling", "max_initial_cases"]) || 10
    }
  end

  defp append_skill_instructions(instructions, skill_roots) do
    paths = Enum.map_join(skill_roots, "\n", &"- #{Path.join(&1, "SKILL.md")}")

    instructions <>
      "\n\nPika-provided skills are system context. Read each required SKILL.md before acting:\n" <>
      paths
  end

  defp record_workflow_kickoff(state, input) do
    if Map.has_key?(state.workflow_kickoffs, state.backend_workflow) do
      state
    else
      %{
        state
        | workflow_kickoffs: Map.put(state.workflow_kickoffs, state.backend_workflow, input)
      }
    end
  end

  defp mark_kickoff_dispatched(%{backend: nil} = state), do: state
  defp mark_kickoff_dispatched(state), do: %{state | kickoff_dispatched: true}

  defp maybe_dispatch_workflow_kickoff(%{kickoff_dispatched: true} = state), do: state

  defp maybe_dispatch_workflow_kickoff(state) do
    case Map.get(state.workflow_kickoffs, state.backend_workflow) do
      nil ->
        state

      input ->
        state
        |> dispatch_input(input)
        |> Map.put(:kickoff_dispatched, true)
    end
  end

  defp public_snapshot(state) do
    %{
      campaign_id: state.campaign_id,
      status: state.status,
      workspace: %{
        root: state.workspace.root,
        source_repo: state.workspace.source_repo,
        source_sha: state.workspace.source_sha,
        source_dirty: state.workspace.source_status != ""
      },
      backend: state.backend_name,
      backend_session_id: state.backend_session && state.backend_session.id,
      active_turn_id: state.active_turn_id,
      agent_responding: state.agent_responding,
      pending_question: public_question(state.pending_questions),
      messages: state.messages,
      artifacts: Map.values(state.artifacts),
      spec: state.spec_result.spec,
      spec_diff: state.spec_diff,
      spec_ready: state.spec_result.ready?,
      missing: state.spec_result.missing,
      spec_errors: state.spec_result.errors,
      harness: state.harness,
      reference_review_evidence: state.reference_review_evidence,
      implementation_review_evidence: state.implementation_review_evidence,
      prepared_setup_sha: state.prepared_setup_sha,
      target_snapshot:
        if(state.target_snapshot, do: TargetSnapshot.public(state.target_snapshot), else: nil),
      target_progress: state.target_progress,
      required_operations: state.required |> MapSet.to_list() |> Enum.sort(),
      references: state.references,
      reference_progress: state.reference_progress,
      skill: Map.drop(state.skill, [:path]),
      best_sha: state.best_sha,
      baseline: state.baseline,
      baseline_retry_count: state.baseline_retry_count,
      baseline_error: state.baseline_error,
      baseline_progress: state.baseline_progress,
      iteration_sampling: state.iteration_sampling,
      sampling_revisions: state.sampling_revisions,
      last_error: state.last_error
    }
  end

  defp broadcast(state, opts \\ []) do
    if state.persistence && Keyword.get(opts, :persist, true) do
      case state.persistence.persist(state) do
        :ok -> :ok
        {:error, reason} -> raise "cannot persist Campaign state: #{inspect(reason)}"
      end
    end

    if Process.whereis(Pika.PubSub),
      do:
        Phoenix.PubSub.broadcast(Pika.PubSub, @topic, {:campaign_updated, public_snapshot(state)})

    :ok
  end

  defp message(role, content, attachments \\ []),
    do: %{
      id: Pika.AgentBackend.Id.new("message"),
      role: role,
      content: content,
      attachments: attachments,
      at: DateTime.utc_now(),
      kind: :message
    }

  defp answer_current_question(state, batch, question, answer, option_id) do
    selected = Enum.find(question.options, &(&1.id == option_id))
    resolved_answer = if selected, do: selected.label, else: answer

    cond do
      option_id not in [nil, ""] and is_nil(selected) ->
        {:reply, {:error, :invalid_option}, state}

      resolved_answer == "" ->
        {:reply, {:error, :empty_answer}, state}

      true ->
        result = %{
          question_id: question.key,
          question: question.question,
          answer: resolved_answer,
          selected_option: selected && selected.label
        }

        answers = batch.answers ++ [result]
        messages = state.messages ++ [message(:user, resolved_answer)]
        next_index = batch.current_index + 1

        if next_index < length(batch.questions) do
          next_batch = %{batch | current_index: next_index, answers: answers}
          next_question = current_question(next_batch)

          next_state = %{
            state
            | pending_questions: next_batch,
              messages: messages ++ [question_message(next_question)],
              stream_message_id: nil,
              activity_message_id: nil
          }

          broadcast(next_state)
          {:reply, :ok, next_state}
        else
          Process.demonitor(batch.monitor_ref, [:flush])
          next_state = %{state | pending_questions: nil, messages: messages}
          broadcast(next_state)
          GenServer.reply(batch.reply_to, {:ok, %{answers: answers}})
          {:reply, :ok, next_state}
        end
    end
  end

  defp validate_questions(args) do
    questions = args["questions"]

    cond do
      not is_list(questions) or questions == [] or not Enum.all?(questions, &is_map/1) ->
        {:error, "questions must contain at least one question object"}

      true ->
        questions
        |> Enum.with_index(1)
        |> Enum.reduce_while({:ok, []}, fn {question, index}, {:ok, acc} ->
          case validate_question(question, index) do
            {:ok, normalized} -> {:cont, {:ok, [normalized | acc]}}
            {:error, message} -> {:halt, {:error, "question #{index}: #{message}"}}
          end
        end)
        |> reverse_questions()
        |> ensure_unique_question_keys()
    end
  end

  defp reverse_questions({:ok, questions}), do: {:ok, Enum.reverse(questions)}
  defp reverse_questions(error), do: error

  defp validate_question(args, index) do
    args = stringify_keys(args)
    key = question_text(args["id"])
    question = question_text(args["question"])
    options = args["options"]

    cond do
      key == "" ->
        {:error, "id is required"}

      question == "" ->
        {:error, "question is required"}

      not is_list(options) or length(options) not in 2..4 or
          not Enum.all?(options, &is_map/1) ->
        {:error, "options must contain between 2 and 4 choices"}

      true ->
        normalized =
          options
          |> Enum.with_index(1)
          |> Enum.map(fn {option, index} ->
            option = stringify_keys(option)

            %{
              id: "option-#{index}",
              label: question_text(option["label"]),
              description: question_text(option["description"])
            }
          end)

        if Enum.any?(normalized, &(&1.label == "")),
          do: {:error, "every option requires a label"},
          else:
            {:ok,
             %{
               id: Pika.AgentBackend.Id.new("question"),
               key: key,
               question: question,
               options: normalized,
               ordinal: index
             }}
    end
  end

  defp ensure_unique_question_keys({:error, _message} = error), do: error

  defp ensure_unique_question_keys({:ok, questions}) do
    keys = Enum.map(questions, & &1.key)

    if length(keys) == length(Enum.uniq(keys)),
      do: {:ok, questions},
      else: {:error, "question ids must be unique"}
  end

  defp question_text(value) when is_binary(value), do: String.trim(value)
  defp question_text(_value), do: ""

  defp public_question(nil), do: nil

  defp public_question(batch) do
    batch
    |> current_question()
    |> Map.take([:id, :question, :options])
    |> Map.merge(%{
      batch_id: batch.id,
      position: batch.current_index + 1,
      total: length(batch.questions),
      asked_at: batch.asked_at
    })
  end

  defp current_question(batch), do: Enum.fetch!(batch.questions, batch.current_index)

  defp question_message(question) do
    :agent
    |> message(question.question)
    |> Map.merge(%{kind: :question, question_id: question.id})
  end

  defp cancel_pending_questions(%{pending_questions: nil} = state, _code, _message), do: state

  defp cancel_pending_questions(state, code, message) do
    Process.demonitor(state.pending_questions.monitor_ref, [:flush])
    GenServer.reply(state.pending_questions.reply_to, mcp_error(code, message))
    %{state | pending_questions: nil}
  end

  defp register_user_artifacts(state, artifacts) do
    artifact_map =
      Enum.reduce(artifacts, state.artifacts, fn artifact, acc ->
        Map.put(acc, artifact.relative_path, artifact)
      end)

    %{state | artifacts: artifact_map}
  end

  defp user_input(body, []) when body != "", do: body

  defp user_input(body, artifacts) do
    attachment_text =
      Enum.map_join(artifacts, "\n", fn artifact ->
        "- #{artifact.relative_path} (#{artifact.mime}, #{artifact.size} bytes, sha256=#{artifact.sha256})"
      end)

    text =
      if body == "",
        do: "User provided the following Artifacts:",
        else: body <> "\n\nAttached Artifacts:"

    text <> "\n" <> attachment_text
  end

  defp mcp_error(code, message, details \\ %{}), do: {:error, code, message, details}
  defp token_hash(token), do: :crypto.hash(:sha256, token)
  defp trimmed_text(value) when is_binary(value), do: String.trim(value)
  defp trimmed_text(_value), do: ""
  defp map(value) when is_map(value), do: value
  defp map(_), do: %{}

  defp stringify_keys(map) when is_map(map),
    do: Map.new(map, fn {key, value} -> {to_string(key), stringify_keys(value)} end)

  defp stringify_keys(list) when is_list(list), do: Enum.map(list, &stringify_keys/1)
  defp stringify_keys(value), do: value

  defp summarize_event(data) do
    inspect(data, limit: 5, printable_limit: 240)
  end

  defp activity_for_tool(type, %{item: %{"type" => "mcpToolCall"} = item}) do
    %{
      key: item["id"] || item["callId"] || "mcp:#{item["tool"]}",
      tags: ["mcp"],
      label: item["tool"] || "MCP tool",
      detail: compact_detail(item["arguments"] || item["server"]),
      status: activity_status(item, type)
    }
  end

  defp activity_for_tool(type, %{item: %{"type" => "commandExecution"} = item}) do
    action_types = Enum.map(item["commandActions"] || [], & &1["type"])

    tags =
      ["command"] ++
        if(Enum.any?(action_types, &(&1 in ["read", "search", "list"])),
          do: ["read"],
          else: []
        ) ++
        if(Enum.any?(action_types, &(&1 in ["write", "edit", "delete"])),
          do: ["edit"],
          else: []
        )

    %{
      key: item["id"] || "command:#{:erlang.phash2(item["command"])}",
      tags: tags,
      label: "运行命令",
      detail: compact_detail(item["command"]),
      status: activity_status(item, type)
    }
  end

  defp activity_for_tool(type, %{"sessionUpdate" => update} = data)
       when update in ["tool_call", "tool_call_update"] do
    kind = data["kind"] || data["toolKind"] || "tool"

    %{
      key: data["toolCallId"] || data["id"] || "cursor:#{:erlang.phash2(data)}",
      tags: cursor_activity_tags(kind, data),
      label: data["title"] || "Agent tool",
      detail: cursor_activity_detail(data),
      status: activity_status(data, type)
    }
  end

  defp activity_for_tool(_type, _data), do: nil

  defp record_activity(state, activity) do
    case state.activity_message_id do
      nil ->
        id = Pika.AgentBackend.Id.new("message")

        message = %{
          id: id,
          role: :activity,
          content: activity_summary(activity.tags),
          at: DateTime.utc_now(),
          kind: :activity,
          activity: %{
            tags: Enum.uniq(activity.tags),
            details: [Map.drop(activity, [:tags])],
            running: activity.status == "running"
          }
        }

        %{
          state
          | messages: state.messages ++ [message],
            activity_message_id: id,
            stream_message_id: nil
        }

      id ->
        messages =
          Enum.map(state.messages, fn
            %{id: ^id, activity: current} = message ->
              tags = Enum.uniq(current.tags ++ activity.tags)
              details = upsert_activity_detail(current.details, Map.drop(activity, [:tags]))

              %{
                message
                | content: activity_summary(tags),
                  activity: %{
                    tags: tags,
                    details: details,
                    running: Enum.any?(details, &(&1.status == "running"))
                  }
              }

            message ->
              message
          end)

        %{state | messages: messages, stream_message_id: nil}
    end
  end

  defp upsert_activity_detail(details, detail) do
    case Enum.find_index(details, &(&1.key == detail.key)) do
      nil -> details ++ [detail]
      index -> List.replace_at(details, index, Map.merge(Enum.at(details, index), detail))
    end
  end

  defp activity_summary(tags) do
    [
      {"edit", "编辑了文件"},
      {"read", "读取文件"},
      {"command", "运行了命令"},
      {"mcp", "调用 MCP"},
      {"plan", "Planning"}
    ]
    |> Enum.filter(fn {tag, _label} -> tag in tags end)
    |> Enum.map_join(" · ", &elem(&1, 1))
  end

  defp activity_status(data, event_type) do
    status = data["status"] || data[:status]

    cond do
      event_type == :tool_started and status not in ["completed", "failed"] -> "running"
      status in ["running", "inProgress", "pending"] -> "running"
      status in ["failed", "error", "cancelled"] -> "failed"
      true -> "completed"
    end
  end

  defp activity_key(%{item: item}, fallback), do: activity_key(item, fallback)

  defp activity_key(data, fallback) when is_map(data) do
    data["id"] || data[:id] || data["itemId"] || data[:item_id] ||
      "#{fallback}:#{:erlang.phash2(data)}"
  end

  defp activity_key(_data, fallback), do: fallback

  defp compact_detail(nil), do: nil

  defp compact_detail(value) when is_binary(value) do
    value
    |> String.replace(~r/\s+/, " ")
    |> String.trim()
    |> String.slice(0, 600)
  end

  defp compact_detail(value),
    do: value |> inspect(limit: 8, printable_limit: 600) |> compact_detail()

  defp plan_detail(%{"plan" => plan}), do: compact_detail(plan)
  defp plan_detail(%{plan: plan}), do: compact_detail(plan)
  defp plan_detail(_data), do: nil

  defp file_change_detail(%{item: item}), do: file_change_detail(item)

  defp file_change_detail(data) when is_map(data) do
    compact_detail(data["path"] || data[:path] || data["itemId"] || data[:item_id])
  end

  defp file_change_detail(_data), do: nil

  defp cursor_activity_tags(kind, data) do
    text = String.downcase("#{kind} #{data["title"]}")

    cond do
      String.contains?(text, ["edit", "write", "patch"]) -> ["edit"]
      String.contains?(text, ["read", "search", "list"]) -> ["read"]
      String.contains?(text, ["terminal", "command", "shell"]) -> ["command"]
      String.contains?(text, "mcp") -> ["mcp"]
      true -> ["command"]
    end
  end

  defp cursor_activity_detail(data) do
    compact_detail(data["rawInput"] || data["command"] || data["content"])
  end

  defp backend_module(:codex_app_server), do: Pika.AgentBackend.CodexAppServer
  defp backend_module(:cursor_acp), do: Pika.AgentBackend.CursorACP

  defp lookup_idempotency(state, identity, tool, key, request_hash, record_key) do
    case Map.get(state.idempotency, record_key) do
      %{request_hash: ^request_hash, response: response} ->
        {:replay, response}

      nil ->
        if state.persistence && function_exported?(state.persistence, :lookup_idempotency, 5) do
          state.persistence.lookup_idempotency(
            identity.session_key,
            tool,
            key,
            request_hash,
            state
          )
        else
          :missing
        end

      _ ->
        :conflict
    end
  end

  defp store_idempotency(state, identity, tool, key, request_hash, response) do
    if state.persistence && function_exported?(state.persistence, :store_idempotency, 6) do
      case state.persistence.store_idempotency(
             identity.session_key,
             tool,
             key,
             request_hash,
             response,
             state
           ) do
        :ok -> :ok
        {:error, reason} -> raise "cannot persist MCP idempotency record: #{inspect(reason)}"
      end
    end

    :ok
  end

  defp restore_durable_state(state, durable) when is_map(durable) do
    state
    |> Map.merge(Map.take(durable, Map.keys(state)))
    |> Map.merge(%{
      backend: nil,
      backend_session: nil,
      backend_token_hash: nil,
      closed_sessions: MapSet.new(),
      active_turn_id: nil,
      stream_message_id: nil,
      activity_message_id: nil,
      mcp_tokens: state.mcp_tokens,
      idempotency: %{},
      kickoff_dispatched: false,
      recovery_pending: true,
      persistence: state.persistence,
      campaign_id: state.campaign_id,
      workspace: state.workspace,
      skill: state.skill,
      references: state.references
    })
    |> require_v2_revision()
    |> refresh_spec_validation()
    |> ensure_implementation_review_requirement()
    |> repair_post_confirmation_draft()
  end

  defp restore_durable_state(state, _durable), do: state

  defp restore_target_views(state) do
    snapshot = state.target_snapshot || state.inherited_target_snapshot

    if is_map(snapshot) do
      with :ok <- TargetSnapshot.verify(state.workspace.root, snapshot),
           :ok <-
             TargetSnapshot.link(state.workspace.root, state.workspace.setup_worktree, snapshot),
           :ok <- restore_best_target_view(state, snapshot) do
        :ok
      else
        {:error, reason} -> {:error, {:target_snapshot_restore_failed, reason}}
      end
    else
      :ok
    end
  end

  defp restore_best_target_view(%{setup_sha: setup_sha} = state, snapshot)
       when is_binary(setup_sha),
       do: TargetSnapshot.link(state.workspace.root, state.workspace.repo, snapshot)

  defp restore_best_target_view(_state, _snapshot), do: :ok

  defp require_v2_revision(%{spec_result: %{spec: %{"schema_version" => 2}}} = state),
    do: state

  defp require_v2_revision(%{spec_result: %{spec: legacy_spec}} = state)
       when is_map(legacy_spec) do
    old_revision = legacy_spec["revision"] || 1

    spec =
      legacy_spec
      |> Map.put("schema_version", 2)
      |> Map.put("revision", old_revision + 1)
      |> Map.delete("implementations")

    %{
      state
      | status: :drafting_spec,
        spec_result: Spec.validate(spec),
        spec_diff: Spec.diff(legacy_spec, spec),
        harness: nil,
        reference_review_evidence: nil,
        implementation_review_evidence: nil,
        required: draft_required_operations(),
        setup_base_sha: state.best_sha,
        setup_sha: nil,
        prepared_setup_sha: nil,
        target_snapshot: nil,
        inherited_target_snapshot: nil,
        target_progress: nil,
        baseline: nil,
        baseline_error: nil,
        iteration_sampling: nil,
        sampling_revisions: [],
        pending_confirmation_input: nil,
        backend_workflow: :alignment,
        last_error:
          "Campaign Spec v#{old_revision} used the ambiguous v1 Reference model. Pika opened v#{old_revision + 1}; explicitly define the Correctness Oracle, frozen Optimization Target, and mutable Development implementation.",
        messages:
          state.messages ++
            [
              message(
                :system,
                "旧 Campaign Spec v#{old_revision} 不能安全推断三种实现角色；已创建 v#{old_revision + 1} 草稿，必须明确 Oracle、Optimization Target 与 Development。"
              )
            ]
    }
  end

  defp require_v2_revision(state), do: state

  defp refresh_spec_validation(%{spec_result: %{spec: spec}} = state) when is_map(spec),
    do: %{state | spec_result: Spec.validate(spec)}

  defp refresh_spec_validation(state), do: state

  defp ensure_implementation_review_requirement(%{status: status} = state)
       when status in [:drafting_spec, :awaiting_confirmation] do
    bundle_missing = is_nil(state.target_snapshot) or is_nil(state.prepared_setup_sha)
    evidence_missing = is_nil(state.implementation_review_evidence)

    required =
      state.required
      |> then(fn required ->
        if bundle_missing,
          do: MapSet.put(required, "submit_implementation_bundle"),
          else: required
      end)
      |> then(fn required ->
        if evidence_missing,
          do: MapSet.put(required, "submit_implementation_review"),
          else: required
      end)

    needs_repair = bundle_missing or evidence_missing

    %{
      state
      | status: if(needs_repair, do: :drafting_spec, else: state.status),
        required: required,
        last_error:
          cond do
            status != :awaiting_confirmation ->
              state.last_error

            bundle_missing ->
              "Optimization Target 或 Development 提交身份缺失；已退回 Agent 重新固化实现并补充审阅证据。"

            evidence_missing ->
              "Target 与 Development 尚无可审阅的正确性和配对性能证据；已退回 Agent 补充 smoke run。"

            true ->
              state.last_error
          end
    }
  end

  defp ensure_implementation_review_requirement(state), do: state

  defp repair_post_confirmation_draft(
         %{
           status: :drafting_spec,
           required: required,
           pending_confirmation_input: confirmation,
           spec_result: %{ready?: true},
           harness: harness
         } = state
       )
       when is_binary(confirmation) and not is_nil(harness) do
    if MapSet.member?(required, "complete_setup_merge") do
      %{
        state
        | status: :awaiting_confirmation,
          required: MapSet.new(),
          pending_confirmation_input: nil,
          last_error: "检测到确认后 Spec 或 Harness 曾被修改；请重新确认后再建立 Baseline。"
      }
    else
      state
    end
  end

  defp repair_post_confirmation_draft(state), do: state

  defp persist_backend_event?(%{type: type}),
    do: type in [:turn_completed, :backend_error, :process_exited]

  defp maybe_dispatch_recovery(state, session) do
    if state.status != :awaiting_confirmation and
         not MapSet.equal?(state.required, MapSet.new()) do
      resume_status =
        if session.resumed,
          do: "the provider session was resumed",
          else: "a replacement provider session was created"

      required = state.required |> MapSet.to_list() |> Enum.sort() |> Enum.join(", ")

      state
      |> dispatch_input(
        "Pika restarted and #{resume_status}. Call get_context now, continue from the persisted Campaign state, and do not repeat completed operations. Current status: #{state.status}. Required MCP operations: #{required}."
      )
      |> Map.put(:kickoff_dispatched, true)
    else
      state
    end
  end

  defp backend_session_message(workflow, %{resumed: true} = session),
    do: "#{workflow} Backend Session 已恢复：#{session.backend_protocol}"

  defp backend_session_message(workflow, %{resume_error: resume_error} = session)
       when not is_nil(resume_error),
       do:
         "#{workflow} 原 Backend Session 无法恢复，已创建新 Session：#{session.backend_protocol}（#{summarize_event(resume_error)}）"

  defp backend_session_message(workflow, session),
    do: "#{workflow} Backend Session 已启动：#{session.backend_protocol}"

  defp call_if_started(message, default, timeout \\ 120_000) do
    case Process.whereis(__MODULE__) do
      nil -> default
      _pid -> GenServer.call(__MODULE__, message, timeout)
    end
  end
end
