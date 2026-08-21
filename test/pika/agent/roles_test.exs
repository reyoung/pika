defmodule Pika.Agent.RolesTest do
  use ExUnit.Case, async: true

  alias Pika.Agent.Role.{Definition, DomainContext, Tool, Work}

  defmodule DomainAdapter do
    @behaviour Pika.Agent.Role.DomainAdapter

    @impl true
    def prepare(%Work{id: id}, workspace) do
      {:ok,
       %DomainContext{
         facts: %{
           done: id == "done" or Process.get({__MODULE__, id}, false),
           _revision: if(Process.get({__MODULE__, id}, false), do: 8, else: 7)
         },
         durable_context: %{label: "work-#{id}"},
         cwd: workspace.root,
         skill_roots: []
       }}
    end

    @impl true
    def invoke(%Work{id: id}, "finish", _arguments, _meta) do
      Process.put({__MODULE__, id}, true)
      {:ok, %{done: true}}
    end
  end

  defmodule Role do
    @behaviour Pika.Agent.Role

    @impl true
    def definition do
      %Definition{
        id: "test_role",
        contract_revision: 1,
        activation: :automatic,
        work_kind: :test_work,
        profile_key: "test_role",
        domain_adapter: DomainAdapter,
        template: %{relative_path: "roles/test_role.md", builtin: "Default {{label}}"},
        tools: [
          %Tool{
            name: "finish",
            description: "Finish the test work.",
            kind: :command,
            input_schema: %{"type" => "object", "additionalProperties" => false}
          }
        ],
        completion: %{
          terminals: [{:completed, {:fact, :done}}],
          suggestions: [{"finish", {:not, {:fact, :done}}}]
        }
      }
    end

    @impl true
    def build_system_instructions(context),
      do: {:ok, "Fixed invariant.\n\n" <> context.template}

    @impl true
    def initial_prompt(context), do: {:ok, "Start #{context.durable_context.label}."}

    @impl true
    def recovery_prompt(context), do: {:ok, "Recover #{context.durable_context.label}."}
  end

  defmodule UntrackedOperationReceipts do
    def run(_work, _role_id, _invocation, _request_sha256, operation), do: operation.()
  end

  test "prepare returns a complete work-specific Role projection" do
    root = temp_dir("role-prepare")
    File.mkdir_p!(Path.join(root, "roles"))
    File.write!(Path.join(root, "roles/test_role.md"), "Workspace guidance for {{label}}")

    work = %Work{role_id: "test_role", kind: :test_work, id: "open", campaign_id: "c-1"}

    assert {:ok, prepared} =
             Pika.Agent.Roles.prepare(work, :fresh,
               role: Role,
               workspace: %{root: root},
               profile: %{backend: :codex_app_server, model: "test-model"},
               operation_receipts: UntrackedOperationReceipts
             )

    assert prepared.work == work
    assert prepared.definition.id == "test_role"

    assert prepared.instructions.system ==
             "Fixed invariant.\n\nWorkspace guidance for work-open"

    assert prepared.instructions.sha256 =~ ~r/^[0-9a-f]{64}$/
    assert prepared.activation == {:start_turn, "Start work-open."}
    assert prepared.progress.state == :open
    assert prepared.progress.required_operations == ["finish"]
    assert prepared.progress.facts_revision == 7
    assert prepared.profile.model == "test-model"
  end

  test "invoke authorizes through the Role and returns completion from reloaded facts" do
    root = temp_dir("role-invoke")
    work = %Work{role_id: "test_role", kind: :test_work, id: "mutable", campaign_id: "c-1"}

    assert {:ok, prepared} =
             Pika.Agent.Roles.prepare(work, :fresh,
               role: Role,
               workspace: %{root: root},
               profile: %{backend: :codex_app_server},
               operation_receipts: UntrackedOperationReceipts
             )

    invocation = %Pika.Agent.Role.Invocation{
      operation: "finish",
      arguments: %{},
      idempotency_key: "finish-once"
    }

    assert {:ok, outcome} = Pika.Agent.Roles.invoke(prepared, invocation)
    assert outcome.value == %{done: true}
    assert outcome.progress.state == {:terminal, :completed}
    assert outcome.progress.required_operations == []
    assert outcome.progress.facts_revision == 8
    assert outcome.actor_directive == :finish
  end

  test "progress ignores the prepared projection and reloads committed facts" do
    root = temp_dir("role-progress")

    work = %Work{
      role_id: "test_role",
      kind: :test_work,
      id: "progress",
      campaign_id: "c-1"
    }

    assert {:ok, prepared} =
             Pika.Agent.Roles.prepare(work, :fresh,
               role: Role,
               workspace: %{root: root},
               profile: %{},
               operation_receipts: UntrackedOperationReceipts
             )

    Process.put({DomainAdapter, work.id}, true)

    assert {:ok, progress} = Pika.Agent.Roles.progress(prepared)
    assert progress.state == {:terminal, :completed}
    assert progress.facts_revision == 8
  end

  defp temp_dir(label) do
    path = Path.join(System.tmp_dir!(), "pika-#{label}-#{System.unique_integer([:positive])}")
    File.mkdir_p!(path)
    on_exit(fn -> File.rm_rf!(path) end)
    path
  end
end
