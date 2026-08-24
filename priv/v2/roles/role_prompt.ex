defmodule Pika.Agent.RolePrompt.FileRef do
  @moduledoc false

  @enforce_keys [:label, :path]
  defstruct @enforce_keys ++ [kind: :file]

  @type t :: %__MODULE__{label: String.t(), path: Path.t(), kind: :file | :directory}
end

defmodule Pika.Agent.RolePrompt.Section do
  @moduledoc false

  alias Pika.Agent.RolePrompt.FileRef

  @enforce_keys [:title, :files]
  defstruct @enforce_keys ++ [required?: false]

  @type t :: %__MODULE__{title: String.t(), files: [FileRef.t()], required?: boolean()}
end

defmodule Pika.Agent.RolePrompt.Sections do
  @moduledoc false

  alias Pika.Agent.RolePrompt.{FileRef, Section}

  @spec render([Section.t()]) :: {:ok, String.t()} | {:error, :invalid_context_sections}
  def render(sections) when is_list(sections) do
    if Enum.all?(sections, &valid_section?/1) do
      optional = Enum.reject(sections, & &1.required?)
      required = Enum.filter(sections, & &1.required?)

      {:ok,
       [
         render_group(optional, "工作前可以读取以下信息。"),
         render_group(required, "开始工作前必须读取以下信息，并避免重复其中已经确认的问题。")
       ]
       |> IO.iodata_to_binary()}
    else
      {:error, :invalid_context_sections}
    end
  end

  def render(_sections), do: {:error, :invalid_context_sections}

  defp render_group([], _lead), do: ""

  defp render_group(sections, lead) do
    rendered =
      Enum.map_join(sections, "\n\n", fn %Section{title: title, files: files} ->
        items =
          Enum.map_join(files, "\n", fn %FileRef{label: label, path: path} ->
            "- #{label}：[#{Path.basename(path)}](<#{path}>)"
          end)

        "## #{title}\n\n#{items}"
      end)

    "\n\n" <> lead <> "\n\n" <> rendered
  end

  defp valid_section?(%Section{title: title, files: files, required?: required?}) do
    is_binary(title) and String.trim(title) != "" and files != [] and is_boolean(required?) and
      Enum.all?(files, fn
        %FileRef{label: label, path: path, kind: kind} ->
          is_binary(label) and String.trim(label) != "" and valid_path?(path, kind)

        _other ->
          false
      end)
  end

  defp valid_section?(_section), do: false

  defp valid_path?(path, :file), do: File.regular?(path)
  defp valid_path?(path, :directory), do: File.dir?(path)
  defp valid_path?(_path, _kind), do: false
end

defmodule Pika.Agent.RolePrompt.Schemas do
  @moduledoc "Pika-owned JSON Schemas injected into a Role system prompt."

  @schema_files %{
    baseline_definition: "baseline-definition.schema.json",
    benchmark_record: "benchmark-record.schema.json",
    cases: "cases.schema.json",
    integration_result: "integration-result.schema.json",
    integration_validation: "integration-validation.schema.json",
    iteration_result: "iteration-result.schema.json",
    metrics: "metrics.schema.json",
    baseline_verification_result: "baseline-verification-result.schema.json",
    target_manifest: "target-manifest.schema.json",
    verify_result: "verify-result.schema.json"
  }

  @enforce_keys Map.keys(@schema_files)
  defstruct @enforce_keys

  @type t :: %__MODULE__{
          baseline_definition: Path.t(),
          baseline_verification_result: Path.t(),
          benchmark_record: Path.t(),
          cases: Path.t(),
          integration_result: Path.t(),
          integration_validation: Path.t(),
          iteration_result: Path.t(),
          metrics: Path.t(),
          target_manifest: Path.t(),
          verify_result: Path.t()
        }

  @spec from_dir(Path.t()) :: {:ok, t()} | {:error, {:missing_schema_files, [Path.t()]}}
  def from_dir(directory) when is_binary(directory) do
    paths =
      Map.new(@schema_files, fn {key, filename} -> {key, Path.join(directory, filename)} end)

    case Enum.reject(Map.values(paths), &File.regular?/1) do
      [] -> {:ok, struct!(__MODULE__, paths)}
      missing -> {:error, {:missing_schema_files, Enum.sort(missing)}}
    end
  end
end

defmodule Pika.Agent.RolePrompt.Context do
  @moduledoc false

  alias Pika.Agent.RolePrompt.{Schemas, Section}

  @enforce_keys [:schemas]
  defstruct schemas: nil, sections: []

  @type t :: %__MODULE__{schemas: Schemas.t(), sections: [Section.t()]}
end

defmodule Pika.Agent.RolePrompt.FollowupInput do
  @moduledoc "Shared three-file input used by v2 Follow-up Role prompts."

  @enforce_keys [:context_file, :target_messages_file, :target_state_file]
  defstruct @enforce_keys

  @type t :: %__MODULE__{
          context_file: Path.t(),
          target_messages_file: Path.t(),
          target_state_file: Path.t()
        }

  @spec validate(t()) :: :ok | {:error, {:missing_prompt_files, [Path.t()]}}
  def validate(%__MODULE__{} = input) do
    missing =
      [input.context_file, input.target_messages_file, input.target_state_file]
      |> Enum.reject(&File.regular?/1)

    if missing == [], do: :ok, else: {:error, {:missing_prompt_files, Enum.sort(missing)}}
  end
end

defmodule Pika.Agent.RolePrompt do
  @moduledoc "Interface implemented by each v2 Role-owned system prompt."

  @callback system_prompt(input :: struct()) :: {:ok, String.t()} | {:error, term()}
end
