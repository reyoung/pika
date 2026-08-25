defmodule Pika.DevReloadTest do
  use ExUnit.Case, async: true

  alias Pika.DevReload

  test "fingerprint changes for reloadable source and ignores unrelated files" do
    root = Path.join(System.tmp_dir!(), "pika-dev-reload-#{System.unique_integer([:positive])}")
    source = Path.join(root, "lib/example.ex")
    unrelated = Path.join(root, "notes.txt")
    File.mkdir_p!(Path.dirname(source))
    File.write!(source, "defmodule Example, do: nil\n")
    File.write!(unrelated, "first\n")
    on_exit(fn -> File.rm_rf!(root) end)

    initial = DevReload.fingerprint(root)
    File.write!(unrelated, "second\n")
    assert DevReload.fingerprint(root) == initial

    File.write!(source, "defmodule Example, do: :changed\n")
    refute DevReload.fingerprint(root) == initial
  end
end
