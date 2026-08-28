package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/reyoung/pika-go/internal/control"
	"github.com/reyoung/pika-go/internal/daemonupdate"
	"github.com/reyoung/pika-go/internal/herdr"
	"github.com/reyoung/pika-go/internal/instance"
	"github.com/reyoung/pika-go/internal/optimizationworkspace"
)

const kickOffPluginID = "pika-go"

func runOpen(ctx context.Context, args []string, input io.Reader, stdout, stderr io.Writer) int {
	flags := newCommandFlagSet("open", stderr)
	label := flags.String("label", "", "`LABEL` for the new Herdr workspace")
	noFocus := flags.Bool("no-focus", false, "Create the Herdr workspace without switching to it")
	timeout := flags.Duration("timeout", 30*time.Second, "Maximum time to wait for the daemon")
	if ok, code := parseCommandFlags(flags, args); !ok {
		return code
	}
	if flags.NArg() > 1 {
		_, _ = fmt.Fprintln(stderr, "open: at most one WORKSPACE path is supported")
		return 2
	}
	root := ""
	if flags.NArg() == 1 {
		root = flags.Arg(0)
	} else {
		workspace, err := optimizationworkspace.Discover("")
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "open: %v\n", err)
			return 2
		}
		root = workspace.Root
	}
	kickOffArgs := []string{"--workspace", root, "--timeout", timeout.String()}
	if *label != "" {
		kickOffArgs = append(kickOffArgs, "--label", *label)
	}
	if *noFocus {
		kickOffArgs = append(kickOffArgs, "--no-focus")
	}
	return runKickOff(ctx, kickOffArgs, input, stdout, stderr)
}

func runKickOff(ctx context.Context, args []string, input io.Reader, stdout, stderr io.Writer) int {
	flags := newCommandFlagSet("kick-off", stderr)
	repository := flags.String("repository", "", "`PATH` to optimize (default: current directory)")
	workspaceRoot := flags.String("workspace", "", "Optimization Workspace `PATH` (default: sibling of repository)")
	label := flags.String("label", "", "`LABEL` for the new Herdr workspace")
	defaults := flags.Bool("defaults", false, "Use the all-Codex default configuration")
	configPath := flags.String("config", "", "Read the complete instance TOML from `PATH`")
	noFocus := flags.Bool("no-focus", false, "Create the workspace without switching to it")
	timeout := flags.Duration("timeout", 30*time.Second, "Maximum time to wait for the daemon")
	if ok, code := parseCommandFlags(flags, args); !ok {
		return code
	}
	if flags.NArg() != 0 {
		_, _ = fmt.Fprintln(stderr, "kick-off: positional arguments are not supported; use --repository PATH")
		return 2
	}
	if *defaults && *configPath != "" {
		_, _ = fmt.Fprintln(stderr, "kick-off: --defaults and --config are mutually exclusive")
		return 2
	}
	if *repository != "" && *workspaceRoot != "" {
		if _, err := os.Stat(filepath.Join(*workspaceRoot, optimizationworkspace.ManifestName)); err == nil {
			_, _ = fmt.Fprintln(stderr, "kick-off: --repository cannot be combined with an existing --workspace")
			return 2
		}
	}
	if *configPath != "" {
		absoluteConfig, absoluteErr := filepath.Abs(*configPath)
		if absoluteErr != nil {
			_, _ = fmt.Fprintf(stderr, "kick-off: resolve --config: %v\n", absoluteErr)
			return 2
		}
		*configPath = filepath.Clean(absoluteConfig)
	}
	if *timeout <= 0 {
		_, _ = fmt.Fprintln(stderr, "kick-off: --timeout must be greater than zero")
		return 2
	}

	herdrSocket := os.Getenv("HERDR_SOCKET_PATH")
	if herdrSocket == "" {
		_, _ = fmt.Fprintln(stderr, "kick-off: no Herdr session detected (HERDR_SOCKET_PATH is unset)")
		_, _ = fmt.Fprintln(stderr, "Start Herdr, open a shell pane, and run `pika-go kick-off` again.")
		return 2
	}
	if !filepath.IsAbs(herdrSocket) {
		_, _ = fmt.Fprintln(stderr, "kick-off: HERDR_SOCKET_PATH must be absolute")
		return 2
	}

	client := herdr.NewClient(herdrSocket)
	snapshot, err := client.Snapshot(ctx)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "kick-off: connect to Herdr: %v\n", err)
		return 1
	}
	if snapshot.Protocol < herdr.MinimumProtocol {
		_, _ = fmt.Fprintf(stderr, "kick-off: Herdr protocol %d is too old; protocol %d or newer is required\n", snapshot.Protocol, herdr.MinimumProtocol)
		return 1
	}
	herdrConfigPath, err := herdr.ResolveConfigPath()
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "kick-off: resolve Herdr configuration: %v\n", err)
		return 1
	}
	if configErr := herdr.ValidateFreshSessionConfiguration(herdrConfigPath); configErr != nil {
		_, _ = fmt.Fprintln(stdout, "Pika-Go requires fresh Agent Sessions, but Herdr native Agent restore is currently enabled or not explicitly disabled.")
		_, _ = fmt.Fprintf(stdout, "  Configuration: %s\n", herdrConfigPath)
		_, _ = fmt.Fprintln(stdout, "  Required:      [session] resume_agents_on_restore = false")
		confirmed, promptErr := promptKickOffConfirmation(input, stdout, "May Pika-Go update it and reload Herdr now?")
		if promptErr != nil {
			_, _ = fmt.Fprintf(stderr, "kick-off: read Herdr configuration confirmation: %v\n", promptErr)
			writeFreshSessionInstructions(stderr, herdrConfigPath)
			return 2
		}
		if !confirmed {
			_, _ = fmt.Fprintln(stderr, "kick-off: Herdr configuration was not changed.")
			writeFreshSessionInstructions(stderr, herdrConfigPath)
			return 2
		}
		if err := herdr.ConfigureFreshSessions(ctx, client, herdrConfigPath); err != nil {
			_, _ = fmt.Fprintf(stderr, "kick-off: configure Herdr fresh Agent Sessions: %v\n", err)
			return 1
		}
		_, _ = fmt.Fprintln(stdout, "Updated Herdr configuration and reloaded the server.")
	} else if err := herdr.RequireFreshSessions(ctx, client, herdrConfigPath); err != nil {
		_, _ = fmt.Fprintf(stderr, "kick-off: reload Herdr configuration: %v\n", err)
		return 1
	}
	workspace, createdWorkspace, err := resolveKickOffWorkspace(ctx, *repository, *workspaceRoot)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "kick-off: %v\n", err)
		return 2
	}
	resolvedRepository := workspace.Identity.SourceRepository
	if *label == "" {
		*label = "Pika-Go: " + filepath.Base(resolvedRepository)
	}
	if createdWorkspace {
		_, _ = fmt.Fprintf(stdout, "Created Optimization Workspace %s\n", workspace.Root)
	} else {
		_, _ = fmt.Fprintf(stdout, "Resuming Optimization Workspace %s\n", workspace.Root)
	}
	if _, statErr := os.Stat(workspace.ConfigPath); statErr == nil && (*defaults || *configPath != "") {
		_, _ = fmt.Fprintln(stderr, "kick-off: existing Workspace configuration is user-owned; --defaults and --config are not allowed")
		return 2
	}
	if binding, bindingErr := workspace.ReadHerdrBinding(); bindingErr == nil && binding.SocketPath == herdrSocket && snapshotHasWorkspace(snapshot, binding.WorkspaceID) {
		if _, healthErr := control.Health(ctx, binding.DaemonSocket); healthErr == nil {
			_, _ = fmt.Fprintf(stdout, "Optimization is already running in Herdr workspace %s.\n", binding.WorkspaceID)
			if *noFocus {
				_, _ = fmt.Fprintf(stdout, "Open it with: herdr workspace focus %s\n", binding.WorkspaceID)
			} else {
				focusKickOffWorkspace(ctx, client, binding.WorkspaceID, binding.TabID, binding.ControlPane, stderr)
			}
			return 0
		}
	}

	_, _ = fmt.Fprintf(stdout, "Creating Herdr workspace %q for %s...\n", *label, workspace.Root)
	var created struct {
		Workspace herdr.Workspace `json:"workspace"`
		RootPane  herdr.Pane      `json:"root_pane"`
	}
	if err := client.Call(ctx, "workspace.create", map[string]any{
		"cwd": workspace.Root, "label": *label, "focus": false,
	}, &created); err != nil {
		_, _ = fmt.Fprintf(stderr, "kick-off: create Herdr workspace: %v\n", err)
		return 1
	}
	if created.Workspace.WorkspaceID == "" || created.RootPane.PaneID == "" || created.RootPane.TabID == "" {
		_, _ = fmt.Fprintln(stderr, "kick-off: Herdr returned an incomplete workspace response")
		return 1
	}
	if err := client.Call(ctx, "tab.rename", map[string]string{
		"tab_id": created.RootPane.TabID, "label": "Pika",
	}, nil); err != nil {
		_, _ = fmt.Fprintf(stderr, "kick-off: name the new Herdr tab: %v\n", err)
		_, _ = fmt.Fprintf(stderr, "The new workspace %s was left open for diagnostics.\n", created.Workspace.WorkspaceID)
		return 1
	}
	if err := client.Call(ctx, "pane.rename", map[string]string{
		"pane_id": created.RootPane.PaneID, "label": "Control",
	}, nil); err != nil {
		_, _ = fmt.Fprintf(stderr, "kick-off: name the initialization pane: %v\n", err)
		_, _ = fmt.Fprintf(stderr, "The new workspace %s was left open for diagnostics.\n", created.Workspace.WorkspaceID)
		return 1
	}

	_, _ = fmt.Fprintf(stdout, "Starting the Pika-Go daemon in workspace %s...\n", created.Workspace.WorkspaceID)
	pikaExecutable, err := os.Executable()
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "kick-off: resolve current pika-go executable: %v\n", err)
		return 1
	}
	pikaExecutable, err = filepath.Abs(pikaExecutable)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "kick-off: resolve current pika-go executable path: %v\n", err)
		return 1
	}
	integrationExecutable := pikaExecutable
	currentGeneration, err := daemonupdate.EnsureCurrent(ctx, workspace.Root, pikaExecutable, Version)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "kick-off: bootstrap managed daemon generation: %v\n", err)
		return 1
	}
	pikaExecutable = currentGeneration.Path
	panePath := strings.Join([]string{filepath.Dir(pikaExecutable), os.Getenv("PATH")}, string(os.PathListSeparator))
	var opened struct {
		PluginPane struct {
			Pane herdr.Pane `json:"pane"`
		} `json:"plugin_pane"`
	}
	if err := client.Call(ctx, "plugin.pane.open", map[string]any{
		"plugin_id":      kickOffPluginID,
		"entrypoint":     "symphony",
		"placement":      "split",
		"target_pane_id": created.RootPane.PaneID,
		"direction":      "right",
		"focus":          false,
		"cwd":            workspace.Root,
		"env": map[string]string{
			"PIKA_GO_WORKSPACE":              workspace.Root,
			"PIKA_GO_INTEGRATION_EXECUTABLE": integrationExecutable,
			"PATH":                           panePath,
		},
	}, &opened); err != nil {
		_, _ = fmt.Fprintf(stderr, "kick-off: start the Pika-Go daemon pane: %v\n", err)
		_, _ = fmt.Fprintln(stderr, "Install or refresh the Herdr plugin with `pika-go install`, then retry.")
		_, _ = fmt.Fprintf(stderr, "The new workspace %s was left open for diagnostics.\n", created.Workspace.WorkspaceID)
		return 1
	}
	if opened.PluginPane.Pane.PaneID == "" {
		_, _ = fmt.Fprintln(stderr, "kick-off: Herdr returned an incomplete plugin pane response")
		return 1
	}
	if err := client.Call(ctx, "pane.rename", map[string]string{
		"pane_id": opened.PluginPane.Pane.PaneID, "label": "Daemon",
	}, nil); err != nil {
		_, _ = fmt.Fprintf(stderr, "kick-off: name the daemon pane: %v\n", err)
		_, _ = fmt.Fprintf(stderr, "The new workspace %s was left open for diagnostics.\n", created.Workspace.WorkspaceID)
		return 1
	}

	socketPath, err := instance.SocketForHerdrWorkspace(herdrSocket, created.Workspace.WorkspaceID)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "kick-off: resolve daemon socket: %v\n", err)
		return 1
	}
	if err := workspace.WriteHerdrBinding(optimizationworkspace.HerdrBinding{
		SocketPath: herdrSocket, WorkspaceID: created.Workspace.WorkspaceID, TabID: created.RootPane.TabID,
		ControlPane: created.RootPane.PaneID, DaemonPane: opened.PluginPane.Pane.PaneID, DaemonSocket: socketPath,
		UpdatedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}); err != nil {
		_, _ = fmt.Fprintf(stderr, "kick-off: persist Herdr binding: %v\n", err)
		return 1
	}
	_, _ = fmt.Fprintln(stdout, "Waiting for the daemon to become ready...")
	if err := waitForKickOffDaemon(ctx, socketPath, *timeout); err != nil {
		_, _ = fmt.Fprintf(stderr, "kick-off: daemon did not become ready: %v\n", err)
		_, _ = fmt.Fprintf(stderr, "Inspect pane %s in workspace %s for startup logs.\n", opened.PluginPane.Pane.PaneID, created.Workspace.WorkspaceID)
		return 1
	}

	configurationExists := false
	if _, statErr := os.Stat(workspace.ConfigPath); statErr == nil {
		configurationExists = true
	} else if !errors.Is(statErr, os.ErrNotExist) {
		_, _ = fmt.Fprintf(stderr, "kick-off: inspect Workspace configuration: %v\n", statErr)
		return 1
	}
	if !configurationExists {
		initArgs := []string{filepath.Clean(pikaExecutable), "init", "--socket", socketPath, "--repository", resolvedRepository}
		if *defaults {
			initArgs = append(initArgs, "--defaults")
		}
		if *configPath != "" {
			initArgs = append(initArgs, "--config", *configPath)
		}
		if err := client.Call(ctx, "pane.send_input", map[string]any{
			"pane_id": created.RootPane.PaneID, "text": shellCommand(initArgs), "keys": []string{"enter"},
		}, nil); err != nil {
			_, _ = fmt.Fprintf(stderr, "kick-off: start init command in pane %s: %v\n", created.RootPane.PaneID, err)
			_, _ = fmt.Fprintf(stderr, "The new workspace %s was left open for diagnostics.\n", created.Workspace.WorkspaceID)
			return 1
		}
		_, _ = fmt.Fprintln(stdout, "\nInitialization command started in the new Herdr workspace.")
	} else {
		_, _ = fmt.Fprintln(stdout, "\nExisting optimization resumed in the new Herdr workspace.")
	}
	_, _ = fmt.Fprintf(stdout, "  Workspace:   %s (%s)\n", created.Workspace.WorkspaceID, *label)
	_, _ = fmt.Fprintf(stdout, "  Init pane:   %s\n", created.RootPane.PaneID)
	_, _ = fmt.Fprintf(stdout, "  Daemon pane: %s\n", opened.PluginPane.Pane.PaneID)
	_, _ = fmt.Fprintf(stdout, "  Repository:  %s\n", resolvedRepository)
	_, _ = fmt.Fprintf(stdout, "  Pika data:   %s\n", workspace.Root)
	if *noFocus {
		_, _ = fmt.Fprintf(stdout, "\nOpen it with: herdr workspace focus %s\n", created.Workspace.WorkspaceID)
	} else {
		focusKickOffWorkspace(ctx, client, created.Workspace.WorkspaceID, created.RootPane.TabID, created.RootPane.PaneID, stderr)
	}
	return 0
}

func snapshotHasWorkspace(snapshot herdr.Snapshot, workspaceID string) bool {
	for _, workspace := range snapshot.Workspaces {
		if workspace.WorkspaceID == workspaceID {
			return true
		}
	}
	return false
}

func focusKickOffWorkspace(ctx context.Context, client *herdr.Client, workspaceID, tabID, paneID string, stderr io.Writer) {
	for _, focus := range []struct {
		method string
		params map[string]string
	}{
		{method: "workspace.focus", params: map[string]string{"workspace_id": workspaceID}},
		{method: "tab.focus", params: map[string]string{"tab_id": tabID}},
		{method: "pane.focus", params: map[string]string{"pane_id": paneID}},
	} {
		if err := client.Call(ctx, focus.method, focus.params, nil); err != nil {
			_, _ = fmt.Fprintf(stderr, "kick-off: optimization is running, but Herdr could not focus its workspace/tab: %v\n", err)
			_, _ = fmt.Fprintf(stderr, "Open it with: herdr workspace focus %s\n", workspaceID)
			return
		}
	}
}

func promptKickOffConfirmation(input io.Reader, output io.Writer, question string) (bool, error) {
	if input == nil {
		return false, io.ErrUnexpectedEOF
	}
	reader := bufio.NewReader(input)
	for {
		_, _ = fmt.Fprintf(output, "%s [y/N] ", question)
		line, err := reader.ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return false, err
		}
		answer := strings.TrimSpace(line)
		if answer == "" {
			if errors.Is(err, io.EOF) && line == "" {
				return false, io.ErrUnexpectedEOF
			}
			return false, nil
		}
		switch {
		case strings.EqualFold(answer, "y"), strings.EqualFold(answer, "yes"):
			return true, nil
		case strings.EqualFold(answer, "n"), strings.EqualFold(answer, "no"):
			return false, nil
		default:
			_, _ = fmt.Fprintln(output, "Please answer yes or no.")
		}
		if errors.Is(err, io.EOF) {
			return false, io.ErrUnexpectedEOF
		}
	}
}

func resolveKickOffWorkspace(ctx context.Context, repository, root string) (optimizationworkspace.Workspace, bool, error) {
	if root != "" {
		absolute, err := filepath.Abs(root)
		if err != nil {
			return optimizationworkspace.Workspace{}, false, fmt.Errorf("resolve Workspace path: %w", err)
		}
		if _, err := os.Stat(filepath.Join(absolute, optimizationworkspace.ManifestName)); err == nil {
			workspace, openErr := optimizationworkspace.Open(absolute)
			return workspace, false, openErr
		} else if !errors.Is(err, os.ErrNotExist) {
			return optimizationworkspace.Workspace{}, false, fmt.Errorf("inspect Workspace: %w", err)
		}
		if repository == "" {
			return optimizationworkspace.Workspace{}, false, fmt.Errorf("Optimization Workspace does not exist: %s", absolute)
		}
		resolvedRepository, err := resolveKickOffRepository(repository)
		if err != nil {
			return optimizationworkspace.Workspace{}, false, err
		}
		workspace, err := optimizationworkspace.Create(ctx, absolute, resolvedRepository)
		return workspace, true, err
	}
	if repository == "" {
		if workspace, err := optimizationworkspace.Discover(""); err == nil {
			return workspace, false, nil
		} else if !strings.Contains(err.Error(), "was not found") {
			return optimizationworkspace.Workspace{}, false, err
		}
	}
	resolvedRepository, err := resolveKickOffRepository(repository)
	if err != nil {
		return optimizationworkspace.Workspace{}, false, err
	}
	defaultRoot, err := optimizationworkspace.DefaultRoot(resolvedRepository)
	if err != nil {
		return optimizationworkspace.Workspace{}, false, err
	}
	_, statErr := os.Stat(filepath.Join(defaultRoot, optimizationworkspace.ManifestName))
	created := errors.Is(statErr, os.ErrNotExist)
	workspace, err := optimizationworkspace.Create(ctx, defaultRoot, resolvedRepository)
	return workspace, created, err
}

func writeFreshSessionInstructions(output io.Writer, configPath string) {
	_, _ = fmt.Fprintf(output, "Set the following in %s, reload Herdr, then retry:\n\n", configPath)
	_, _ = fmt.Fprintln(output, "[session]")
	_, _ = fmt.Fprintln(output, "resume_agents_on_restore = false")
	_, _ = fmt.Fprintln(output, "\nherdr server reload-config")
}

func shellCommand(arguments []string) string {
	quoted := make([]string, 0, len(arguments))
	for _, argument := range arguments {
		quoted = append(quoted, shellWord(argument))
	}
	return strings.Join(quoted, " ")
}

func shellWord(value string) string {
	if value != "" && strings.IndexFunc(value, func(character rune) bool {
		return !((character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') || strings.ContainsRune("_@%+=:,./-", character))
	}) == -1 {
		return value
	}
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func resolveKickOffRepository(repository string) (string, error) {
	if repository == "" {
		var err error
		repository, err = os.Getwd()
		if err != nil {
			return "", fmt.Errorf("resolve current directory: %w", err)
		}
	}
	absolute, err := filepath.Abs(repository)
	if err != nil {
		return "", fmt.Errorf("resolve repository path: %w", err)
	}
	info, err := os.Stat(absolute)
	if err != nil || !info.IsDir() {
		return "", fmt.Errorf("repository is not a directory: %s", absolute)
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", fmt.Errorf("resolve repository: %w", err)
	}
	return resolved, nil
}

func waitForKickOffDaemon(ctx context.Context, socketPath string, timeout time.Duration) error {
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	var lastErr error
	for {
		if _, err := control.Health(waitCtx, socketPath); err == nil {
			return nil
		} else {
			lastErr = err
		}
		select {
		case <-waitCtx.Done():
			if lastErr != nil {
				return fmt.Errorf("%w (last health check: %v)", waitCtx.Err(), lastErr)
			}
			return waitCtx.Err()
		case <-ticker.C:
		}
	}
}
