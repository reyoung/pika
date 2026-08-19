defmodule Pika.Stage0.Campaign do
  @moduledoc false

  use GenServer

  alias Pika.AgentBackend

  alias Pika.Stage0.{
    ArtifactStore,
    Baseline,
    Harness,
    PromptCatalog,
    ReferenceCatalog,
    Sampling,
    Spec,
    Workspace
  }

  @topic "stage0:campaign"
  @write_tools ~w(register_artifact submit_spec submit_harness complete_setup_merge submit_baseline submit_iteration_sample)

  def start_link(opts), do: GenServer.start_link(__MODULE__, opts, name: __MODULE__)

  def start(opts) do
    DynamicSupervisor.start_child(Pika.Stage0.CampaignSupervisor, %{
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

  def authorize(token), do: call_if_started({:authorize, token}, {:error, :not_started})

  def mcp_call(token, tool, args),
    do: call_if_started({:mcp, token, tool, args}, {:error, :not_started})

  def send_message(body, artifacts \\ []),
    do: call_if_started({:send_message, body, artifacts}, {:error, :not_started})

  def toggle_reference(id), do: call_if_started({:toggle_reference, id}, {:error, :not_started})
  def confirm_spec, do: call_if_started(:confirm_spec, {:error, :not_started})

  def request_changes(body, artifacts \\ []),
    do: call_if_started({:request_changes, body, artifacts}, {:error, :not_started})

  @impl true
  def init(opts) do
    workspace = Keyword.fetch!(opts, :workspace)
    backend = Keyword.get(opts, :backend, :codex_app_server)

    initial_mcp_tokens =
      case Keyword.get(opts, :mcp_token) do
        token when is_binary(token) ->
          %{
            token_hash(token) => %{
              phase: :alignment,
              role: :boundary,
              session_key: "test-session"
            }
          }

        _ ->
          %{}
      end

    state = %{
      status: :drafting_spec,
      workspace: workspace,
      backend_name: backend,
      backend_module: Keyword.get(opts, :backend_module, backend_module(backend)),
      backend_profile: Keyword.get(opts, :backend_profile, %{}),
      model: Keyword.get(opts, :model),
      reasoning_effort: Keyword.get(opts, :reasoning_effort, :high),
      backend_enabled: Keyword.get(opts, :start_backend, true),
      backend: nil,
      backend_session: nil,
      backend_token_hash: nil,
      closed_sessions: MapSet.new(),
      backend_phase: :alignment,
      active_turn_id: nil,
      stream_message_id: nil,
      activity_message_id: nil,
      mcp_url: Keyword.fetch!(opts, :mcp_url),
      mcp_tokens: initial_mcp_tokens,
      idempotency: %{},
      messages: [
        message(
          :system,
          if(workspace.source_status == "",
            do: "Stage0 Workspace 已从 clean HEAD 创建；源仓库不会被修改。",
            else: "Stage0 Workspace 已从提交 HEAD 重新 clone；源仓库未提交修改被排除且不会被修改。"
          )
        )
      ],
      artifacts: %{},
      spec_result: Spec.validate(%{}),
      spec_diff: nil,
      harness: nil,
      required: MapSet.new(~w(submit_spec submit_harness)),
      references: Keyword.get(opts, :references, ReferenceCatalog.entries()),
      resolve_references: Keyword.get(opts, :resolve_references, true),
      skill: Keyword.fetch!(opts, :skill),
      skill_roots: Keyword.get(opts, :skill_roots, []),
      best_sha: workspace.source_sha,
      setup_sha: nil,
      baseline: nil,
      baseline_retry_count: 0,
      baseline_error: nil,
      sampling_revisions: [],
      iteration_sampling: nil,
      phase_kickoffs: %{},
      kickoff_dispatched: false,
      pending_confirmation_input: nil,
      last_error: nil
    }

    case PromptCatalog.validate() do
      :ok ->
        broadcast(state)

        if state.backend_enabled do
          {:ok, state, {:continue, :open_alignment_backend}}
        else
          {:ok, state}
        end

      {:error, reason} ->
        {:stop, reason}
    end
  end

  @impl true
  def handle_continue(:open_alignment_backend, state) do
    {:noreply, begin_open_session(state, :alignment, state.workspace.setup_worktree)}
  end

  @impl true
  def handle_call(:snapshot, _from, state), do: {:reply, public_snapshot(state), state}

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
      state = record_phase_kickoff(state, input)
      state = dispatch_input(state, input)
      state = mark_kickoff_dispatched(state)
      broadcast(state)
      {:reply, :ok, state}
    end
  end

  def handle_call({:toggle_reference, id}, _from, state) do
    if state.status in [:drafting_spec, :awaiting_confirmation] do
      references =
        Enum.map(state.references, fn
          %{id: ^id} = entry ->
            %{entry | selected: not entry.selected, status: :unresolved, sha: nil}

          entry ->
            entry
        end)

      selected_ids = for reference <- references, reference.selected, do: reference.id
      spec_result = Spec.validate(Map.put(state.spec_result.spec, "reference_ids", selected_ids))

      state = %{
        state
        | references: references,
          spec_result: spec_result,
          status: :drafting_spec,
          required: MapSet.put(state.required, "submit_spec")
      }

      state =
        if Map.has_key?(state.phase_kickoffs, :alignment) do
          dispatch_input(
            state,
            "Reference selection changed to #{Enum.join(selected_ids, ", ")}. Update and resubmit the Campaign Spec with submit_spec before asking for confirmation."
          )
        else
          state
        end

      broadcast(state)
      {:reply, :ok, state}
    else
      {:reply, {:error, :references_frozen}, state}
    end
  end

  def handle_call(:confirm_spec, _from, state) do
    if state.status == :awaiting_confirmation and state.spec_result.ready? and state.harness do
      confirmation_input = "确认 Campaign Spec v1，并建立 Baseline。"

      state = %{
        state
        | status: :resolving_references,
          last_error: nil,
          pending_confirmation_input: confirmation_input,
          phase_kickoffs: Map.put(state.phase_kickoffs, :baseline, confirmation_input),
          messages: state.messages ++ [message(:user, confirmation_input)]
      }

      parent = self()
      references = state.references
      resolve? = state.resolve_references

      Task.start(fn ->
        result =
          if resolve?, do: ReferenceCatalog.resolve_selected(references), else: {:ok, references}

        send(parent, {:references_resolved, result})
      end)

      broadcast(state)
      {:reply, :ok, state}
    else
      {:reply, {:error, :spec_not_confirmable}, state}
    end
  end

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
            required: MapSet.new(~w(submit_spec submit_harness)),
            messages: state.messages ++ [message(:user, "修改要求：#{body}", artifacts)]
        }

        state =
          dispatch_input(
            state,
            user_input("用户拒绝当前 Spec，并要求修改：#{body}", artifacts)
          )

        broadcast(state)
        {:reply, :ok, state}

      true ->
        {:reply, {:error, :invalid_state}, state}
    end
  end

  def handle_call({:mcp, token, tool, args}, _from, state) do
    token_hash = token_hash(token)

    case Map.fetch(state.mcp_tokens, token_hash) do
      :error ->
        {:reply, mcp_error("unauthorized", "invalid Backend Session token"), state}

      {:ok, identity} ->
        execute_mcp(tool, stringify_keys(args), identity, token_hash, state)
    end
  end

  @impl true
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
        spec_result: spec_result,
        status: :building_baseline,
        required: MapSet.new(["complete_setup_merge"]),
        messages:
          state.messages ++
            [message(:system, "Campaign Spec v1 已由用户确认；等待 Agent 完成 setup squash merge。")]
    }

    state = dispatch_input(state, state.pending_confirmation_input)

    broadcast(state)
    {:noreply, state}
  end

  def handle_info({:references_resolved, {:error, references}}, state) do
    state = %{
      state
      | references: references,
        status: :awaiting_confirmation,
        last_error: "一个或多个默认 Reference 无法解析；未进入 Baseline。"
    }

    broadcast(state)
    {:noreply, state}
  end

  def handle_info({:backend_opened, phase, {:ok, handle, session}}, state) do
    state = %{
      state
      | backend: handle,
        backend_session: session,
        backend_phase: phase,
        messages:
          state.messages ++
            [message(:system, "#{phase} Backend Session 已启动：#{session.backend_protocol}")]
    }

    state = maybe_dispatch_phase_kickoff(state)
    broadcast(state)
    {:noreply, state}
  end

  def handle_info({:backend_opened, _phase, {:error, reason}}, state) do
    state = %{state | last_error: "Backend Session 启动失败：#{inspect(reason)}", backend: nil}
    broadcast(state)
    {:noreply, state}
  end

  def handle_info({:backend_turn_result, result}, state) do
    state =
      case result do
        {:ok, _turn_id} -> state
        {:error, reason} -> %{state | last_error: "Backend Turn 失败：#{inspect(reason)}"}
      end

    broadcast(state)
    {:noreply, state}
  end

  def handle_info({:pika_backend_event, event}, state) do
    state =
      if MapSet.member?(state.closed_sessions, event.session_id) do
        state
      else
        apply_backend_event(state, event)
      end

    broadcast(state)
    {:noreply, state}
  end

  def handle_info({:completion_followup, _completed_turn_id}, state) do
    if state.backend && is_nil(state.active_turn_id) &&
         not MapSet.equal?(state.required, MapSet.new()) && state.status != :optimizing do
      missing = state.required |> MapSet.to_list() |> Enum.sort() |> Enum.join(", ")

      state =
        dispatch_input(
          state,
          "Pika completion gate is still open. Complete these required MCP operations before ending: #{missing}."
        )

      {:noreply, state}
    else
      {:noreply, state}
    end
  end

  def handle_info(:recover_backend, state) do
    cwd =
      if state.backend_phase == :baseline,
        do: state.workspace.repo,
        else: state.workspace.setup_worktree

    {:noreply, begin_open_session(state, state.backend_phase, cwd)}
  end

  def handle_info(_message, state), do: {:noreply, state}

  @impl true
  def terminate(_reason, %{backend: backend}) when not is_nil(backend) do
    AgentBackend.close_session(backend)
    :ok
  catch
    :exit, _reason -> :ok
  end

  def terminate(_reason, _state), do: :ok

  defp execute_mcp("get_context", _args, identity, _token_hash, state) do
    result = %{
      session: identity,
      campaign: public_snapshot(state),
      required_operations: state.required |> MapSet.to_list() |> Enum.sort()
    }

    {:reply, {:ok, result}, state}
  end

  defp execute_mcp(tool, args, identity, token_hash, state) when tool in @write_tools do
    key = args["idempotency_key"]

    cond do
      not is_binary(key) or key == "" ->
        {:reply, mcp_error("missing_required_data", "idempotency_key is required"), state}

      true ->
        request_hash = :crypto.hash(:sha256, :erlang.term_to_binary({tool, args}))
        record_key = {token_hash, tool, key}

        case Map.get(state.idempotency, record_key) do
          %{request_hash: ^request_hash, response: response} ->
            {:reply, response, state}

          nil ->
            {response, next_state} = perform_write(tool, args, identity, state)
            entry = %{request_hash: request_hash, response: response}

            next_state = %{
              next_state
              | idempotency: Map.put(next_state.idempotency, record_key, entry)
            }

            broadcast(next_state)
            {:reply, response, next_state}

          _ ->
            {:reply,
             mcp_error("idempotency_conflict", "same key was used with a different request"),
             state}
        end
    end
  end

  defp execute_mcp(tool, _args, _identity, _token_hash, state),
    do: {:reply, mcp_error("forbidden_role", "tool is unavailable: #{tool}"), state}

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

  defp perform_write("submit_spec", args, %{phase: :alignment}, state) do
    selected_ids = for reference <- state.references, reference.selected, do: reference.id
    spec = args["spec"] |> map() |> Map.put("reference_ids", selected_ids)
    result = Spec.validate(spec)

    required =
      if result.ready?,
        do: MapSet.delete(state.required, "submit_spec"),
        else: MapSet.put(state.required, "submit_spec")

    state =
      %{
        state
        | spec_result: result,
          spec_diff: Spec.diff(state.spec_result.spec, result.spec),
          required: required
      }
      |> maybe_awaiting_confirmation()

    response =
      {:ok, %{ready: result.ready?, missing: result.missing, errors: result.errors, revision: 1}}

    {response, state}
  end

  defp perform_write("submit_spec", _args, _identity, state),
    do: {mcp_error("invalid_state", "submit_spec is only allowed during alignment"), state}

  defp perform_write("submit_harness", args, %{phase: :alignment}, state) do
    case Harness.validate(state.workspace.setup_worktree, args) do
      {:ok, harness} ->
        state =
          %{state | harness: harness, required: MapSet.delete(state.required, "submit_harness")}
          |> maybe_awaiting_confirmation()

        {{:ok, %{digest: harness.digest, protected_paths: harness.protected_paths}}, state}

      {:error, reason} ->
        {mcp_error("missing_required_data", "Harness validation failed", %{reason: reason}),
         state}
    end
  end

  defp perform_write("submit_harness", _args, _identity, state),
    do: {mcp_error("invalid_state", "submit_harness is only allowed during alignment"), state}

  defp perform_write("complete_setup_merge", args, %{phase: :alignment}, state) do
    if state.status == :building_baseline and
         MapSet.member?(state.required, "complete_setup_merge") do
      case Workspace.verify_setup_merge(
             state.workspace,
             args["base_sha"],
             args["setup_sha"],
             args["best_sha"]
           ) do
        :ok ->
          case Harness.verify_digest(state.workspace.repo, state.harness) do
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
                          {hash, %{identity | phase: :baseline}}
                        end)
                  }
                end

              state =
                if state.backend_enabled,
                  do: begin_open_session(state, :baseline, state.workspace.repo),
                  else: state

              {{:ok, %{best_sha: state.best_sha, protected_digest: state.harness.digest}}, state}

            {:error, reason} ->
              {mcp_error("protected_path_changed", "protected Harness digest changed", %{
                 reason: reason
               }), state}
          end

        {:error, reason} ->
          {mcp_error("invalid_state", "setup merge verification failed", %{reason: reason}),
           state}
      end
    else
      {mcp_error("invalid_state", "complete_setup_merge is not currently required"), state}
    end
  end

  defp perform_write("complete_setup_merge", _args, _identity, state),
    do: {mcp_error("forbidden_role", "complete_setup_merge requires alignment session"), state}

  defp perform_write("submit_baseline", args, %{phase: :baseline}, state) do
    with true <- state.status == :building_baseline,
         true <- args["measured_sha"] == state.best_sha,
         true <- Pika.Stage0.Git.clean?(state.workspace.repo),
         {:ok, samples} <- registered_path(state, args["samples_artifact"]),
         {:ok, correctness} <- registered_path(state, args["correctness_artifact"]),
         {:ok, profiler} <- registered_path(state, args["profiler_artifact"]),
         :ok <- validate_profiler_dependencies(state, profiler),
         {:ok, result} <-
           Baseline.evaluate(
             samples,
             correctness,
             profiler,
             state.spec_result.spec,
             state.best_sha,
             state.skill.sha
           ) do
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
          metrics: result.metrics,
          profiler: result.profiler,
          max_initial_cases:
            get_in(state.spec_result.spec, ["iteration_sampling", "max_initial_cases"])
        }}, state}
    else
      {:error, {:insufficient_valid_pairs, _, _, _} = reason} ->
        baseline_retry(reason, state)

      {:error, reason} ->
        {mcp_error("missing_required_data", "Baseline validation failed", %{reason: reason}),
         %{state | baseline_error: inspect(reason)}}

      false ->
        {mcp_error(
           "invalid_state",
           "Baseline state, SHA, or temporary Best cleanliness is invalid"
         ), state}
    end
  end

  defp perform_write("submit_baseline", _args, _identity, state),
    do: {mcp_error("forbidden_role", "submit_baseline requires baseline session"), state}

  defp perform_write("submit_iteration_sample", args, %{phase: :baseline}, state) do
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
                      "Iteration Sample v#{sampling.revision} 已建立：#{length(sampling.case_ids)} / #{length(state.spec_result.spec["benchmark_cases"])} Cases。Stage0 Demo 停在 Optimizing。"
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

  defp baseline_retry(reason, %{baseline_retry_count: 0} = state) do
    state = %{
      state
      | baseline_retry_count: 1,
        baseline_error: inspect(reason),
        messages: state.messages ++ [message(:system, "有效 Pair 少于 24；允许且要求整组重跑一次。")]
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

  defp validate_profiler_dependencies(state, profiler_path) do
    with {:ok, body} <- File.read(profiler_path),
         {:ok, manifest} <- Jason.decode(body),
         report_paths when is_list(report_paths) and report_paths != [] <-
           manifest["report_paths"],
         evidence_paths when is_list(evidence_paths) and evidence_paths != [] <-
           manifest["remote_evidence_paths"],
         true <- Enum.all?(report_paths ++ evidence_paths, &Map.has_key?(state.artifacts, &1)) do
      :ok
    else
      _ -> {:error, :profiler_dependencies_not_registered}
    end
  end

  defp maybe_awaiting_confirmation(state) do
    if (state.spec_result.ready? and state.harness) && MapSet.equal?(state.required, MapSet.new()) do
      %{state | status: :awaiting_confirmation}
    else
      %{state | status: :drafting_spec}
    end
  end

  defp begin_open_session(state, phase, cwd) do
    skill_roots = [state.skill.path | state.skill_roots] |> Enum.uniq()

    case session_instructions(state, phase, skill_roots) do
      {:ok, instructions} ->
        token = :crypto.strong_rand_bytes(32) |> Base.url_encode64(padding: false)
        token_hash = token_hash(token)

        identity = %{
          phase: phase,
          role: :boundary,
          session_key: Pika.AgentBackend.Id.new("stage0")
        }

        state = %{
          state
          | mcp_tokens: Map.put(state.mcp_tokens, token_hash, identity),
            backend_token_hash: token_hash,
            backend_phase: phase,
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
        mcp = %{url: state.mcp_url, token: token}

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

          send(parent, {:backend_opened, phase, result})
        end)

        state

      {:error, reason} ->
        %{state | last_error: "#{phase} Agent Instructions 加载失败：#{inspect(reason)}"}
    end
  end

  defp dispatch_input(%{backend: nil} = state, _input), do: state

  defp dispatch_input(state, input) do
    parent = self()
    backend = state.backend
    active? = is_binary(state.active_turn_id)

    Task.start(fn ->
      result =
        if active?,
          do: AgentBackend.steer(backend, input),
          else: AgentBackend.start_turn(backend, input)

      send(parent, {:backend_turn_result, result})
    end)

    state
  end

  defp apply_backend_event(state, %{type: :session_started}), do: state

  defp apply_backend_event(state, %{type: :turn_started, turn_id: turn_id}) do
    %{
      state
      | active_turn_id: turn_id,
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
            activity_message_id: nil
        }

      id ->
        messages =
          Enum.map(state.messages, fn
            %{id: ^id} = message -> %{message | content: message.content <> delta}
            message -> message
          end)

        %{state | messages: messages}
    end
  end

  defp apply_backend_event(state, %{type: type, data: data})
       when type in [:tool_started, :tool_completed] do
    case activity_for_tool(type, data) do
      nil -> state
      activity -> record_activity(state, activity)
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
      %{state | active_turn_id: nil, stream_message_id: nil, activity_message_id: nil}
    else
      state
    end
  end

  defp apply_backend_event(state, %{type: :process_exited}) do
    if state.backend_enabled and state.status != :optimizing,
      do: Process.send_after(self(), :recover_backend, 100)

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
        stream_message_id: nil,
        activity_message_id: nil,
        messages: state.messages ++ [message(:system, "Backend 进程退出；将使用新 Session 重建上下文。")]
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
      source_sha: state.workspace.source_sha
    }
  end

  defp instruction_assigns(state, :setup_merge), do: %{source_sha: state.workspace.source_sha}

  defp instruction_assigns(state, :baseline) do
    %{
      best_sha: state.best_sha,
      repo: state.workspace.repo,
      artifacts: state.workspace.artifacts,
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

  defp record_phase_kickoff(state, input) do
    if Map.has_key?(state.phase_kickoffs, state.backend_phase) do
      state
    else
      %{state | phase_kickoffs: Map.put(state.phase_kickoffs, state.backend_phase, input)}
    end
  end

  defp mark_kickoff_dispatched(%{backend: nil} = state), do: state
  defp mark_kickoff_dispatched(state), do: %{state | kickoff_dispatched: true}

  defp maybe_dispatch_phase_kickoff(%{kickoff_dispatched: true} = state), do: state

  defp maybe_dispatch_phase_kickoff(state) do
    case Map.get(state.phase_kickoffs, state.backend_phase) do
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
      messages: state.messages,
      artifacts: Map.values(state.artifacts),
      spec: state.spec_result.spec,
      spec_diff: state.spec_diff,
      spec_ready: state.spec_result.ready?,
      missing: state.spec_result.missing,
      spec_errors: state.spec_result.errors,
      harness: state.harness,
      required_operations: state.required |> MapSet.to_list() |> Enum.sort(),
      references: state.references,
      skill: Map.drop(state.skill, [:path]),
      best_sha: state.best_sha,
      baseline: state.baseline,
      baseline_retry_count: state.baseline_retry_count,
      baseline_error: state.baseline_error,
      iteration_sampling: state.iteration_sampling,
      sampling_revisions: state.sampling_revisions,
      last_error: state.last_error
    }
  end

  defp broadcast(state) do
    if Process.whereis(Pika.PubSub),
      do: Phoenix.PubSub.broadcast(Pika.PubSub, @topic, {:stage0_updated, public_snapshot(state)})

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

  defp call_if_started(message, default) do
    case Process.whereis(__MODULE__) do
      nil -> default
      _pid -> GenServer.call(__MODULE__, message, 120_000)
    end
  end
end
