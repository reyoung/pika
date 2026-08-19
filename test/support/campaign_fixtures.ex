defmodule Pika.Test.CampaignFixtures do
  @moduledoc false

  alias Pika.Test.AlignmentFixtures

  def config_file(contents \\ default_config()) do
    directory = AlignmentFixtures.temp_dir("pika-campaign-config")
    path = Path.join(directory, "pika.yaml")
    File.write!(path, contents)
    path
  end

  def workspace do
    AlignmentFixtures.temp_dir("pika-campaign-workspace")
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
