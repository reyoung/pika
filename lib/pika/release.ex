defmodule Pika.Release do
  @moduledoc false

  def add_cli(%Mix.Release{} = release) do
    executable = Path.join([release.path, "bin", Atom.to_string(release.name)])
    contents = File.read!(executable)

    init_command = """
    case $1 in
      init)
        shift
        export_release_sys_config
        exec "$REL_VSN_DIR/elixir" \\
             --cookie "$RELEASE_COOKIE" \\
             --erl-config "$RELEASE_SYS_CONFIG" \\
             --boot "$REL_VSN_DIR/$RELEASE_BOOT_SCRIPT_CLEAN" \\
             --boot-var RELEASE_LIB "$RELEASE_ROOT/lib" \\
             --vm-args "$RELEASE_VM_ARGS" \\
             --eval 'Pika.CLI.main(["init" | System.argv()])' -- "$@"
        ;;

    """

    contents =
      unless String.contains?(contents, "Pika.CLI.main([\"init\" | System.argv()])") do
        String.replace(contents, "case $1 in\n", init_command, global: false)
      else
        contents
      end

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

    contents =
      unless String.contains?(contents, "Pika.CLI.main([\"serve\" | System.argv()])") do
        String.replace(contents, "case $1 in\n", serve_command, global: false)
      else
        contents
      end

    reconfiguration_command = """
    case $1 in
      reconfiguration|reconfigure)
        shift
        export_release_sys_config
        exec "$REL_VSN_DIR/elixir" \\
             --cookie "$RELEASE_COOKIE" \\
             --erl-config "$RELEASE_SYS_CONFIG" \\
             --boot "$REL_VSN_DIR/$RELEASE_BOOT_SCRIPT_CLEAN" \\
             --boot-var RELEASE_LIB "$RELEASE_ROOT/lib" \\
             --vm-args "$RELEASE_VM_ARGS" \\
             --eval 'Pika.CLI.main(["reconfiguration" | System.argv()])' -- "$@"
        ;;

    """

    contents =
      unless String.contains?(contents, "Pika.CLI.main([\"reconfiguration\" | System.argv()])") do
        String.replace(contents, "case $1 in\n", reconfiguration_command, global: false)
      else
        contents
      end

    contents =
      contents
      |> add_known_command("    init           Interactively initializes a Pika Workspace\n")
      |> add_known_command("    reconfiguration  Updates mutable Pika Workspace configuration\n")
      |> add_known_command("    serve          Starts Pika Server in the foreground\n")

    File.write!(executable, contents)

    release
  end

  defp add_known_command(contents, command) do
    if String.contains?(contents, command) do
      contents
    else
      String.replace(
        contents,
        "The known commands are:\n\n",
        "The known commands are:\n\n" <> command,
        global: false
      )
    end
  end
end
