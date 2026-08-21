defmodule Pika.Agent.Profile do
  @moduledoc "Resolves and freezes one Role-specific Agent Profile at Session creation."

  def resolve(workspace, definition, work, opts) do
    cond do
      Keyword.has_key?(opts, :profile) ->
        explicit_profile(Keyword.get(opts, :profile))

      Keyword.has_key?(opts, :profiles) ->
        select_attempt_profile(Keyword.get(opts, :profiles), work)

      definition.profile_key == "iteration_agents" ->
        with {:ok, mutable} <- Pika.RuntimeConfig.mutable(workspace) do
          select_attempt_profile(mutable["iteration_agents"], work)
        end

      definition.profile_key == "alignment_backend" ->
        Pika.RuntimeConfig.alignment_profile(workspace)

      true ->
        Pika.RuntimeConfig.profile(workspace, definition.profile_key)
    end
  end

  defp explicit_profile(profile) when is_map(profile), do: {:ok, profile}
  defp explicit_profile(_invalid), do: {:error, :invalid_agent_profile}

  defp select_attempt_profile(profiles, %{kind: :attempt, id: attempt_id})
       when is_list(profiles) and profiles != [] do
    with {:ok, attempt} <- Pika.AttemptStore.attempt(attempt_id),
         profile when is_map(profile) <-
           Enum.at(profiles, attempt.slot_index) || List.first(profiles) do
      {:ok, profile}
    else
      nil -> {:error, :agent_profile_missing}
      {:error, _reason} = error -> error
      _ -> {:error, :invalid_agent_profile}
    end
  end

  defp select_attempt_profile(_profiles, _work), do: {:error, :invalid_agent_profiles}

  def backend(profile, backend_modules) do
    requested = field(profile, :backend) || field(profile, :type) || "codex_app_server"
    backend = backend_atom(requested)

    module =
      Map.get(backend_modules, backend) ||
        case backend do
          :cursor_acp -> Pika.AgentBackend.CursorACP
          :codex_app_server -> Pika.AgentBackend.CodexAppServer
          _ -> nil
        end

    if module, do: {:ok, backend, module}, else: {:error, {:unsupported_agent_backend, requested}}
  end

  def backend_profile(profile, backend, workspace, work) do
    {command, args} = backend_command(backend, field(profile, :command))

    %{
      backend: backend,
      command: command,
      args: args,
      approval_policy: field(profile, :approval_policy),
      sandbox_policy: field(profile, :sandbox_policy),
      env: field(profile, :env) || %{},
      protocol_config: field(profile, :protocol_config) || %{},
      artifact_dir: Path.join([workspace.artifacts, "logs", work.role_id, work.id])
    }
  end

  def model(profile), do: field(profile, :model)
  def reasoning_effort(profile), do: field(profile, :reasoning_effort) || "high"

  def mcp_url(workspace) do
    listen = get_in(workspace.snapshot, ["immutable", "listen"])
    host = if listen["host"] in ["0.0.0.0", "::"], do: "127.0.0.1", else: listen["host"]
    "http://#{host}:#{listen["port"]}/mcp"
  end

  defp backend_command(_backend, [command, "app-server", "--listen", "stdio://"]),
    do: {command, []}

  defp backend_command(_backend, [command, "acp"]), do: {command, []}
  defp backend_command(_backend, [command | args]), do: {command, args}
  defp backend_command(:cursor_acp, nil), do: {"cursor-agent", []}
  defp backend_command(_, nil), do: {"codex", []}

  defp backend_atom(value) when is_atom(value), do: value
  defp backend_atom("cursor_acp"), do: :cursor_acp
  defp backend_atom("codex_app_server"), do: :codex_app_server
  defp backend_atom(value) when is_binary(value), do: existing_atom(value) || value

  defp existing_atom(value) do
    String.to_existing_atom(value)
  rescue
    ArgumentError -> nil
  end

  defp field(map, key), do: Map.get(map, key, Map.get(map, Atom.to_string(key)))
end
