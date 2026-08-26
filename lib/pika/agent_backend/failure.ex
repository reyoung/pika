defmodule Pika.AgentBackend.Failure do
  @moduledoc "Provider-neutral reason for a Backend Session or Turn failure."

  @categories [
    :capacity_exhausted,
    :authentication_failed,
    :context_exhausted,
    :session_budget_exhausted,
    :transient,
    :fatal,
    :unknown
  ]

  @eligible_categories [:capacity_exhausted, :authentication_failed]

  @enforce_keys [:category, :message]
  defstruct [:category, :code, :message, :retry_at, details: %{}]

  @type category ::
          :capacity_exhausted
          | :authentication_failed
          | :context_exhausted
          | :session_budget_exhausted
          | :transient
          | :fatal
          | :unknown

  @type t :: %__MODULE__{
          category: category(),
          code: String.t() | atom() | nil,
          message: String.t(),
          retry_at: non_neg_integer() | nil,
          details: map()
        }

  @spec new(category(), String.t(), keyword()) :: t()
  def new(category, message, opts \\ []) when category in @categories and is_binary(message) do
    %__MODULE__{
      category: category,
      code: Keyword.get(opts, :code),
      message: message,
      retry_at: Keyword.get(opts, :retry_at),
      details: Keyword.get(opts, :details, %{})
    }
  end

  @spec eligible?(t() | map() | nil) :: boolean()
  def eligible?(%__MODULE__{category: category}), do: category in @eligible_categories

  def eligible?(map) when is_map(map) do
    case from_map(map) do
      %__MODULE__{} = failure -> eligible?(failure)
      nil -> false
    end
  end

  def eligible?(_failure), do: false

  @spec to_map(t()) :: map()
  def to_map(%__MODULE__{} = failure) do
    %{
      category: Atom.to_string(failure.category),
      code: if(is_nil(failure.code), do: nil, else: to_string(failure.code)),
      message: failure.message,
      retry_at: failure.retry_at,
      details: failure.details
    }
  end

  @spec from_map(map() | nil) :: t() | nil
  def from_map(nil), do: nil

  def from_map(%__MODULE__{} = failure), do: failure

  def from_map(map) when is_map(map) do
    category = value(map, :category)

    with {:ok, category} <- category(category),
         message when is_binary(message) <- value(map, :message) do
      %__MODULE__{
        category: category,
        code: value(map, :code),
        message: message,
        retry_at: value(map, :retry_at),
        details: value(map, :details) || %{}
      }
    else
      _other -> nil
    end
  end

  def from_map(_value), do: nil

  @spec from_event_data(map()) :: t() | nil
  def from_event_data(data) when is_map(data), do: data |> value(:failure) |> from_map()

  @spec unknown(String.t(), keyword()) :: t()
  def unknown(message, opts \\ []), do: new(:unknown, message, opts)

  defp category(value) when is_atom(value) and value in @categories, do: {:ok, value}

  defp category(value) when is_binary(value) do
    case Enum.find(@categories, &(Atom.to_string(&1) == value)) do
      nil -> :error
      category -> {:ok, category}
    end
  end

  defp category(_value), do: :error

  defp value(map, key), do: Map.get(map, key) || Map.get(map, Atom.to_string(key))
end
