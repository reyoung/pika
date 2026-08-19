defmodule Pika.Test.Phase1Fixtures do
  @moduledoc false

  alias Pika.Test.Stage0Fixtures

  def config_file(contents \\ default_config()) do
    directory = Stage0Fixtures.temp_dir("pika-phase1-config")
    path = Path.join(directory, "pika.yaml")
    File.write!(path, contents)
    path
  end

  def workspace do
    Stage0Fixtures.temp_dir("pika-phase1-workspace")
  end

  def default_config(port \\ 18_080) do
    """
    server:
      host: 127.0.0.1
      port: #{port}
    backend:
      type: codex_app_server
      command: [codex, app-server, --listen, stdio://]
      protocol_config: {}
    campaign:
      plan: true
      max_attempts: null
      history_n: 10
      reference_catalog: []
      stop_conditions:
        mode: all_goals
    """
  end

  def load_config(workspace, path, extra \\ []) do
    Pika.Config.load(path, Keyword.merge([workspace: workspace], extra))
  end
end
