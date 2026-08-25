defmodule Pika.DevReload do
  @moduledoc false

  @patterns [
    "lib/**/*.ex",
    "lib/**/*.heex",
    "priv/v2/**/*.ex",
    "assets/js/**/*.js",
    "assets/css/**/*.css",
    "priv/static/assets/dev_reload.js",
    "priv/static/assets/keyboard_shortcuts.js",
    "priv/static/**/*.svg"
  ]

  @spec fingerprint(Path.t()) :: String.t()
  def fingerprint(root) when is_binary(root) do
    root = Path.expand(root)

    @patterns
    |> Enum.flat_map(&Path.wildcard(Path.join(root, &1), match_dot: true))
    |> Enum.uniq()
    |> Enum.filter(&File.regular?/1)
    |> Enum.sort()
    |> Enum.reduce(:crypto.hash_init(:sha256), fn path, hash ->
      relative = Path.relative_to(path, root)

      case File.read(path) do
        {:ok, contents} -> :crypto.hash_update(hash, [relative, 0, contents, 0])
        {:error, _reason} -> hash
      end
    end)
    |> :crypto.hash_final()
    |> Base.url_encode64(padding: false)
  end
end
