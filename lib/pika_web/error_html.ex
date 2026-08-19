defmodule PikaWeb.ErrorHTML do
  use PikaWeb, :html

  def render(template, _assigns) do
    Phoenix.Controller.status_message_from_template(template)
  end
end

defmodule PikaWeb.ErrorJSON do
  def render(template, _assigns) do
    %{error: Phoenix.Controller.status_message_from_template(template)}
  end
end
