defmodule Pika.AgentBackend.Handle do
  @moduledoc false
  @enforce_keys [:module, :pid]
  defstruct [:module, :pid]

  @type t :: %__MODULE__{module: module(), pid: pid()}
end

defmodule Pika.AgentBackend.Error do
  @moduledoc false
  @enforce_keys [:code, :message]
  defstruct [:code, :message, :failure, details: %{}]

  @type t :: %__MODULE__{
          code: atom(),
          message: String.t(),
          details: map(),
          failure: Pika.AgentBackend.Failure.t() | nil
        }
end

defmodule Pika.AgentBackend do
  @moduledoc """
  Protocol-neutral control contract for one independently supervised backend session.

  The first argument after `start_link/2` is the backend process reference. The receiver is
  implicit in the design-document pseudocode and explicit here so multiple sessions of the same
  provider can run concurrently without global registration. `open_session/7` installs Pika's
  system-level instructions; `start_turn/2` carries only the user- or user-action-authored input
  that owns the Session kick-off.
  """

  alias Pika.AgentBackend.{Error, Handle}

  @type event_sink :: pid() | (Pika.AgentBackend.Event.t() -> any())
  @type backend :: pid() | GenServer.server()

  @callback start_link(Pika.AgentBackend.LaunchConfig.t() | map(), event_sink()) ::
              GenServer.on_start()
  @callback open_session(
              backend(),
              Path.t(),
              String.t() | nil,
              atom() | String.t() | nil,
              map(),
              [Path.t()],
              String.t()
            ) ::
              {:ok, Pika.AgentBackend.Session.t()} | {:error, Error.t()}
  @callback start_turn(backend(), String.t() | [map()]) :: {:ok, String.t()} | {:error, Error.t()}
  @callback steer(backend(), String.t() | [map()]) :: {:ok, String.t()} | {:error, Error.t()}
  @callback interrupt(backend()) :: :ok | {:error, Error.t()}
  @callback close_session(backend()) :: :ok | {:error, Error.t()}
  @callback capabilities(backend()) :: map()

  @spec start_link(module(), map() | Pika.AgentBackend.LaunchConfig.t(), event_sink()) ::
          {:ok, Handle.t()} | {:error, term()}
  def start_link(module, profile, event_sink) do
    start_result =
      if Process.whereis(Pika.AgentBackendSessionSupervisor) do
        DynamicSupervisor.start_child(Pika.AgentBackendSessionSupervisor, %{
          id: make_ref(),
          start: {module, :start_link, [profile, event_sink]},
          restart: :temporary,
          shutdown: 10_000,
          type: :worker
        })
      else
        module.start_link(profile, event_sink)
      end

    with {:ok, pid} <- start_result do
      {:ok, %Handle{module: module, pid: pid}}
    end
  end

  def open_session(
        %Handle{module: module, pid: pid},
        cwd,
        model,
        effort,
        mcp,
        skill_roots,
        instructions
      ),
      do: module.open_session(pid, cwd, model, effort, mcp, skill_roots, instructions)

  def start_turn(%Handle{module: module, pid: pid}, input), do: module.start_turn(pid, input)
  def steer(%Handle{module: module, pid: pid}, input), do: module.steer(pid, input)
  def interrupt(%Handle{module: module, pid: pid}), do: module.interrupt(pid)

  def close_session(%Handle{module: module, pid: pid}) do
    result = module.close_session(pid)

    if result == :ok and Process.alive?(pid) do
      case Process.whereis(Pika.AgentBackendSessionSupervisor) do
        nil -> GenServer.stop(pid, :normal)
        _supervisor -> DynamicSupervisor.terminate_child(Pika.AgentBackendSessionSupervisor, pid)
      end
    end

    result
  end

  def capabilities(%Handle{module: module, pid: pid}), do: module.capabilities(pid)
end
