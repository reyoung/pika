defmodule Pika.Release do
  @moduledoc false

  def add_cli(%Mix.Release{} = release) do
    executable = Path.join([release.path, "bin", Atom.to_string(release.name)])
    contents = File.read!(executable)

    serve_command = """
    case $1 in
      serve)
        shift
        export_release_sys_config
        exec "$REL_VSN_DIR/elixir" \\
             --cookie "$RELEASE_COOKIE" \\
             --erl-config "$RELEASE_SYS_CONFIG" \\
             --boot "$REL_VSN_DIR/$RELEASE_BOOT_SCRIPT_CLEAN" \\
             --boot-var RELEASE_LIB "$RELEASE_ROOT/lib" \\
             --vm-args "$RELEASE_VM_ARGS" \\
             --eval 'Pika.CLI.main(["serve" | System.argv()])' -- "$@"
        ;;

    """

    unless String.contains?(contents, "Pika.CLI.main([\"serve\" | System.argv()])") do
      contents = String.replace(contents, "case $1 in\n", serve_command, global: false)

      contents =
        String.replace(
          contents,
          "The known commands are:\n\n",
          "The known commands are:\n\n    serve          Starts Pika Server in the foreground\n",
          global: false
        )

      File.write!(executable, contents)
    end

    release
  end
end
