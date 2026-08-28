package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"

	"github.com/reyoung/pika-go/internal/legacymigration"
)

func runWorkspace(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		_, _ = fmt.Fprintln(stderr, "workspace: expected legacy-list or import")
		return 2
	}
	switch args[0] {
	case "legacy-list":
		return runWorkspaceLegacyList(ctx, args[1:], stdout, stderr)
	case "import":
		return runWorkspaceImport(ctx, args[1:], stdout, stderr)
	default:
		_, _ = fmt.Fprintf(stderr, "workspace: unknown subcommand %q\n", args[0])
		return 2
	}
}

func runWorkspaceLegacyList(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := newCommandFlagSet("workspace legacy-list", stderr)
	configRoot := flags.String("config-root", "", "Legacy Pika-Go plugin configuration `PATH`")
	stateRoot := flags.String("state-root", "", "Legacy Pika-Go plugin state `PATH`")
	jsonOutput := flags.Bool("json", false, "Print JSON")
	if ok, code := parseCommandFlags(flags, args); !ok {
		return code
	}
	if flags.NArg() != 0 {
		_, _ = fmt.Fprintln(stderr, "workspace legacy-list: positional arguments are not supported")
		return 2
	}
	roots, err := resolveLegacyRoots(*configRoot, *stateRoot)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "workspace legacy-list: %v\n", err)
		return 2
	}
	instances, err := legacymigration.List(ctx, roots)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "workspace legacy-list: %v\n", err)
		return 1
	}
	if *jsonOutput {
		if err := json.NewEncoder(stdout).Encode(instances); err != nil {
			_, _ = fmt.Fprintf(stderr, "workspace legacy-list: encode output: %v\n", err)
			return 1
		}
		return 0
	}
	if len(instances) == 0 {
		_, _ = fmt.Fprintln(stdout, "No legacy Pika-Go instances found.")
		return 0
	}
	for _, item := range instances {
		active := "stopped"
		if item.DaemonActive {
			active = "RUNNING"
		}
		_, _ = fmt.Fprintf(stdout, "%s  %-13s  %-7s  %s\n", item.ID, item.Initialization, active, item.Repository)
	}
	return 0
}

func runWorkspaceImport(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := newCommandFlagSet("workspace import", stderr)
	configRoot := flags.String("config-root", "", "Legacy Pika-Go plugin configuration `PATH`")
	stateRoot := flags.String("state-root", "", "Legacy Pika-Go plugin state `PATH`")
	instanceID := flags.String("instance", "", "Legacy instance `ID`")
	workspaceRoot := flags.String("workspace", "", "New Optimization Workspace `PATH`")
	jsonOutput := flags.Bool("json", false, "Print JSON")
	if ok, code := parseCommandFlags(flags, args); !ok {
		return code
	}
	if flags.NArg() != 0 || *instanceID == "" || *workspaceRoot == "" {
		_, _ = fmt.Fprintln(stderr, "workspace import: --instance ID and --workspace PATH are required")
		return 2
	}
	absolute, err := filepath.Abs(*workspaceRoot)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "workspace import: resolve Workspace path: %v\n", err)
		return 2
	}
	roots, err := resolveLegacyRoots(*configRoot, *stateRoot)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "workspace import: %v\n", err)
		return 2
	}
	workspace, err := legacymigration.Import(ctx, legacymigration.ImportOptions{Roots: roots, InstanceID: *instanceID, WorkspaceRoot: filepath.Clean(absolute)})
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "workspace import: %v\n", err)
		return 1
	}
	if *jsonOutput {
		if err := json.NewEncoder(stdout).Encode(workspace.Identity); err != nil {
			_, _ = fmt.Fprintf(stderr, "workspace import: encode output: %v\n", err)
			return 1
		}
		return 0
	}
	_, _ = fmt.Fprintf(stdout, "Imported legacy instance %s into %s\n", *instanceID, workspace.Root)
	_, _ = fmt.Fprintf(stdout, "Resume with: pika-go resume %s\n", shellWord(workspace.Root))
	return 0
}

func resolveLegacyRoots(configRoot, stateRoot string) (legacymigration.Roots, error) {
	defaults, err := legacymigration.DefaultRoots()
	if err != nil {
		return legacymigration.Roots{}, err
	}
	if configRoot != "" {
		absolute, err := filepath.Abs(configRoot)
		if err != nil {
			return legacymigration.Roots{}, err
		}
		defaults.Config = filepath.Clean(absolute)
	}
	if stateRoot != "" {
		absolute, err := filepath.Abs(stateRoot)
		if err != nil {
			return legacymigration.Roots{}, err
		}
		defaults.State = filepath.Clean(absolute)
	}
	return defaults, nil
}
