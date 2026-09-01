package cli

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/reyoung/pika-go/internal/activation"
	"github.com/reyoung/pika-go/internal/configuration"
	"github.com/reyoung/pika-go/internal/control"
	"github.com/reyoung/pika-go/internal/daemon"
	"github.com/reyoung/pika-go/internal/daemonupdate"
	"github.com/reyoung/pika-go/internal/gitworkspace"
	"github.com/reyoung/pika-go/internal/herdr"
	"github.com/reyoung/pika-go/internal/instance"
	"github.com/reyoung/pika-go/internal/instructions"
	"github.com/reyoung/pika-go/internal/mcp"
	"github.com/reyoung/pika-go/internal/optimizationworkspace"
	"github.com/reyoung/pika-go/internal/outbox"
	"github.com/reyoung/pika-go/internal/plugininstall"
	"github.com/reyoung/pika-go/internal/protocol"
	"github.com/reyoung/pika-go/internal/provider"
	"github.com/reyoung/pika-go/internal/scheduler"
	"github.com/reyoung/pika-go/internal/symphony"
	"github.com/reyoung/pika-go/internal/toolapp"
	webuiapp "github.com/reyoung/pika-go/internal/webui"
	"github.com/reyoung/pika-go/internal/workbench"
	"github.com/reyoung/pika-go/internal/workruntime"
)

var Version = "dev"

// UpdateFailBeforeReadyVersion is an integration-test linker hook. Release
// builds leave it empty.
var UpdateFailBeforeReadyVersion string
var UpdateFailAfterCommitVersion string

func Run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if stdout == nil {
		stdout = io.Discard
	}
	if stderr == nil {
		stderr = io.Discard
	}
	if len(args) == 0 {
		printUsage(stderr)
		return 2
	}

	switch args[0] {
	case "kick-off":
		return runKickOff(ctx, args[1:], stdin, stdout, stderr)
	case "open":
		return runOpen(ctx, args[1:], stdin, stdout, stderr)
	case "pause":
		return runSchedulerControl(ctx, "pause", args[1:], stdout, stderr)
	case "resume":
		return runSchedulerControl(ctx, "resume", args[1:], stdout, stderr)
	case "workspace":
		return runWorkspace(ctx, args[1:], stdout, stderr)
	case "install":
		return runInstall(ctx, args[1:], stdin, stdout, stderr)
	case "update":
		return runUpdate(ctx, args[1:], stdout, stderr)
	case "daemon":
		return runDaemon(ctx, args[1:], stderr)
	case "webui":
		return runWebUI(ctx, args[1:], stdout, stderr)
	case "status":
		return runStatus(ctx, args[1:], stdout, stderr)
	case "init":
		return runInit(ctx, args[1:], stdin, stdout, stderr)
	case "draft-baseline":
		return runDraftBaseline(ctx, args[1:], stdout, stderr)
	case "back-off":
		return runBackOff(ctx, args[1:], stdout, stderr)
	case "cancel-work":
		return runCancelWork(ctx, args[1:], stdout, stderr)
	case "shutdown":
		return runShutdown(ctx, args[1:], stdout, stderr)
	case "backup":
		return runBackup(ctx, args[1:], stdout, stderr)
	case "mcp-proxy":
		return runMCPProxy(ctx, args[1:], stdin, stdout, stderr)
	case "edit-instruction":
		return runEditInstruction(ctx, args[1:], stdin, stdout, stderr)
	case "apply-best-update":
		return runApplyBestUpdate(ctx, args[1:], stdout, stderr)
	case "hook":
		return runHook(ctx, args[1:], stdin, stdout)
	case "version", "--version", "-version":
		_, _ = fmt.Fprintln(stdout, Version)
		return 0
	case "update-probe":
		if len(args) != 1 {
			_, _ = fmt.Fprintln(stderr, "update-probe: arguments are not supported")
			return 2
		}
		if err := json.NewEncoder(stdout).Encode(daemonupdate.CurrentProbe(Version)); err != nil {
			return 1
		}
		return 0
	case "help":
		if len(args) == 1 {
			printUsage(stdout)
			return 0
		}
		if len(args) != 2 {
			_, _ = fmt.Fprintln(stderr, "help: exactly one COMMAND is supported")
			return 2
		}
		if _, ok := publicCommandHelp[args[1]]; !ok {
			_, _ = fmt.Fprintf(stderr, "help: unknown command %q\n", args[1])
			return 2
		}
		if args[1] == "version" || args[1] == "workspace" {
			printCommandUsage(stdout, args[1], nil)
			return 0
		}
		return Run(ctx, []string{args[1], "--help"}, stdin, stdout, stdout)
	case "--help", "-h":
		printUsage(stdout)
		return 0
	default:
		_, _ = fmt.Fprintf(stderr, "unknown command %q\n", args[0])
		printUsage(stderr)
		return 2
	}
}

func runWebUI(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := newCommandFlagSet("webui", stderr)
	workspacePath := flags.String("workspace", "", "Optimization Workspace `PATH`")
	listenAddress := flags.String("listen", "0.0.0.0:8080", "HTTP listen `HOST:PORT`")
	rotateToken := flags.Bool("rotate-token", false, "Rotate the persistent bearer token before serving")
	if ok, code := parseCommandFlags(flags, args); !ok {
		return code
	}
	if flags.NArg() != 0 {
		_, _ = fmt.Fprintln(stderr, "webui: unexpected positional arguments")
		return 2
	}
	var workspace optimizationworkspace.Workspace
	var err error
	if *workspacePath == "" {
		workspace, err = optimizationworkspace.Discover("")
	} else {
		workspace, err = optimizationworkspace.Open(*workspacePath)
	}
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "webui: %v\n", err)
		return 2
	}
	binding, err := workspace.ReadHerdrBinding()
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "webui: %v\n", err)
		return 1
	}
	if _, err := control.Health(ctx, binding.DaemonSocket); err != nil {
		_, _ = fmt.Fprintf(stderr, "webui: daemon is unavailable: %v\n", err)
		return 1
	}
	token, tokenPath, err := webuiapp.EnsureToken(workspace.RuntimeRoot, *rotateToken)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "webui: prepare token: %v\n", err)
		return 1
	}
	handler, err := webuiapp.NewHandler(binding.DaemonSocket, token)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "webui: prepare server: %v\n", err)
		return 1
	}
	listener, err := net.Listen("tcp", *listenAddress)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "webui: listen on %s: %v\n", *listenAddress, err)
		return 1
	}
	defer listener.Close()
	webuiRuntime := filepath.Join(workspace.RuntimeRoot, "webui")
	pidPath := filepath.Join(webuiRuntime, "pid")
	if err := os.WriteFile(pidPath, []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600); err != nil {
		_, _ = fmt.Fprintf(stderr, "webui: write PID: %v\n", err)
		return 1
	}
	defer os.Remove(pidPath)
	host := listener.Addr().String()
	if tcpAddress, ok := listener.Addr().(*net.TCPAddr); ok && (tcpAddress.IP.IsUnspecified() || tcpAddress.IP.String() == "::") {
		host = net.JoinHostPort("localhost", strconv.Itoa(tcpAddress.Port))
	}
	_, _ = fmt.Fprintf(stdout, "Pika-Go WebUI: http://%s/\nBearer token: %s\n", host, tokenPath)
	_, _ = fmt.Fprintf(stderr, "webui: serving read-only Workbench for %s on %s\n", workspace.Root, listener.Addr())
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			_, _ = fmt.Fprintf(stderr, "webui: shutdown: %v\n", err)
			return 1
		}
		if err := <-done; err != nil && !errors.Is(err, http.ErrServerClosed) {
			_, _ = fmt.Fprintf(stderr, "webui: serve: %v\n", err)
			return 1
		}
		return 0
	case err := <-done:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			_, _ = fmt.Fprintf(stderr, "webui: serve: %v\n", err)
			return 1
		}
		return 0
	}
}

func runInstall(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	flags := newCommandFlagSet("install", stderr)
	installDir := flags.String("dir", "", "Absolute plugin installation `PATH`")
	herdrExecutable := flags.String("herdr", "", "Herdr executable `PATH`")
	if ok, code := parseCommandFlags(flags, args); !ok {
		return code
	}
	if flags.NArg() != 0 {
		_, _ = fmt.Fprintln(stderr, "install: positional arguments are not supported")
		return 2
	}
	if *installDir == "" {
		resolved, err := plugininstall.DefaultDir()
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "install: %v\n", err)
			return 1
		}
		*installDir = resolved
	}
	if !filepath.IsAbs(*installDir) {
		_, _ = fmt.Fprintln(stderr, "install: --dir must be an absolute path")
		return 2
	}
	if *herdrExecutable == "" {
		*herdrExecutable = os.Getenv("HERDR_BIN_PATH")
	}
	if *herdrExecutable == "" {
		*herdrExecutable = "herdr"
	}
	executable, err := os.Executable()
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "install: resolve pika-go executable: %v\n", err)
		return 1
	}
	executable, err = filepath.Abs(executable)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "install: resolve pika-go executable: %v\n", err)
		return 1
	}
	result, err := plugininstall.Install(ctx, plugininstall.Options{
		Executable: executable, HerdrExecutable: *herdrExecutable, InstallDir: filepath.Clean(*installDir), Version: Version,
		Stdin: stdin, Stdout: stdout, Stderr: stderr,
	})
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "install: %v\n", err)
		return 1
	}
	_, _ = fmt.Fprintf(stdout, "installed pika-go %s to %s\n", Version, result.InstallDir)
	return 0
}

func runUpdate(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	statusOnly := len(args) > 0 && args[0] == "status"
	if statusOnly {
		args = args[1:]
	}
	flags := newCommandFlagSet("update", stderr)
	workspaceRoot := flags.String("workspace", "", "Optimization Workspace `PATH`")
	jsonOutput := flags.Bool("json", false, "Print JSON only")
	binary := flags.String("binary", "", "Local candidate binary `PATH`")
	if ok, code := parseCommandFlags(flags, args); !ok {
		return code
	}
	if flags.NArg() != 0 {
		_, _ = fmt.Fprintln(stderr, "update: positional arguments are not supported")
		return 2
	}
	var workspace optimizationworkspace.Workspace
	var err error
	if *workspaceRoot != "" {
		workspace, err = optimizationworkspace.Open(*workspaceRoot)
	} else {
		workspace, err = optimizationworkspace.Discover("")
	}
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "update: resolve Optimization Workspace: %v\n", err)
		return 2
	}
	if statusOnly {
		if *binary != "" {
			_, _ = fmt.Fprintln(stderr, "update status: --binary is not supported")
			return 2
		}
		status, err := daemonupdate.ReadStatus(workspace.Root)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "update status: %v\n", err)
			return 1
		}
		return writeUpdateStatus(status, *jsonOutput, stdout, stderr)
	}
	if *binary == "" {
		_, _ = fmt.Fprintln(stderr, "update: --binary is required")
		return 2
	}
	candidate, err := daemonupdate.Stage(ctx, workspace.Root, *binary)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "update: stage candidate: %v\n", err)
		return 1
	}
	if current, currentErr := daemonupdate.ReadCurrent(workspace.Root); currentErr != nil {
		_, _ = fmt.Fprintln(stderr, "update: this Workspace has not bootstrapped hot update; open it once with an update-capable daemon")
		return 1
	} else if current.Digest == candidate.Digest {
		_, _ = fmt.Fprintf(stdout, "pika-go daemon already runs generation %s (%s)\n", candidate.Probe.Version, candidate.Digest[:12])
		return 0
	}
	binding, err := workspace.ReadHerdrBinding()
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "update: read running daemon binding: %v\n", err)
		return 1
	}
	accepted, err := control.Update(ctx, binding.DaemonSocket, candidate)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "update: request handoff: %v\n", err)
		return 1
	}
	deadline := time.NewTimer(2 * time.Minute)
	defer deadline.Stop()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	status := accepted
	for {
		switch status.State {
		case daemonupdate.StateCommitted:
			return writeUpdateStatus(status, *jsonOutput, stdout, stderr)
		case daemonupdate.StateRolledBack, daemonupdate.StateFailed:
			_ = writeUpdateStatus(status, *jsonOutput, stdout, stderr)
			return 1
		}
		select {
		case <-ctx.Done():
			_, _ = fmt.Fprintf(stderr, "update: wait for handoff: %v; use `pika-go update status` to inspect the continuing operation\n", ctx.Err())
			return 1
		case <-deadline.C:
			_, _ = fmt.Fprintln(stderr, "update: handoff is still running; use `pika-go update status`")
			return 1
		case <-ticker.C:
			if next, readErr := daemonupdate.ReadStatus(workspace.Root); readErr == nil && next.ID == accepted.ID {
				status = next
			}
		}
	}
}

func writeUpdateStatus(status daemonupdate.Status, jsonOutput bool, stdout, stderr io.Writer) int {
	if jsonOutput {
		if err := json.NewEncoder(stdout).Encode(status); err != nil {
			_, _ = fmt.Fprintf(stderr, "update: encode status: %v\n", err)
			return 1
		}
		return 0
	}
	_, _ = fmt.Fprintf(stdout, "daemon update %s: %s\n", status.ID, status.State)
	_, _ = fmt.Fprintf(stdout, "  from: %s (%s)\n", status.From.Version, shortDigest(status.From.Digest))
	_, _ = fmt.Fprintf(stdout, "  to:   %s (%s)\n", status.To.Version, shortDigest(status.To.Digest))
	if status.Failure != "" {
		_, _ = fmt.Fprintf(stdout, "  error: %s\n", status.Failure)
	}
	return 0
}

func shortDigest(value string) string {
	if len(value) > 12 {
		return value[:12]
	}
	return value
}

func runDaemon(ctx context.Context, args []string, stderr io.Writer) int {
	flags := newCommandFlagSet("daemon", stderr)
	socketPath := flags.String("socket", "", "Unix socket `PATH`")
	workspaceRoot := flags.String("workspace", "", "Optimization Workspace `PATH`")
	stateDir := flags.String("state-dir", "", "Plugin state directory `PATH`")
	configDir := flags.String("config-dir", "", "Plugin configuration directory `PATH`")
	instanceID := flags.String("instance", "", "Pika instance `ID`")
	handoffListenerFD := flags.Int("handoff-listener-fd", -1, "inherited listener file descriptor")
	handoffLockFD := flags.Int("handoff-lock-fd", -1, "inherited lock file descriptor")
	handoffControlFD := flags.Int("handoff-control-fd", -1, "inherited update control file descriptor")
	if ok, code := parseCommandFlags(flags, args); !ok {
		return code
	}
	paths, err := instance.ResolveRuntime(instance.RuntimeOptions{
		SocketPath: *socketPath, WorkspaceRoot: *workspaceRoot,
		ConfigRoot: *configDir, StateRoot: *stateDir, InstanceID: *instanceID,
	})
	if err != nil {
		writeDaemonLog(stderr, "error", "runtime.resolve_failed", err, nil)
		return 2
	}
	handoffCandidate := *handoffListenerFD >= 0 || *handoffLockFD >= 0 || *handoffControlFD >= 0
	var handoffCommitted atomic.Bool
	handoffCommitted.Store(!handoffCandidate)
	var generationReady atomic.Bool
	generationReady.Store(!handoffCandidate)
	if handoffCandidate && (*handoffListenerFD < 0 || *handoffLockFD < 0 || *handoffControlFD < 0) {
		writeDaemonLog(stderr, "error", "update.handoff_invalid", errors.New("listener, lock, and control file descriptors must be inherited together"), nil)
		return 2
	}
	var handoffControl *daemonupdate.Control
	if handoffCandidate {
		handoffControl, err = daemonupdate.NewInheritedControl(os.NewFile(uintptr(*handoffControlFD), "pika-update-control"))
		if err != nil {
			writeDaemonLog(stderr, "error", "update.control_failed", err, nil)
			return 1
		}
		defer handoffControl.Close()
	}
	runtimeCtx, cancelRuntime := context.WithCancel(ctx)
	defer cancelRuntime()
	var runtimeWG sync.WaitGroup
	var engine *symphony.Engine
	var handoffSessions []symphony.ActiveAgentSession
	var daemonLock *instance.Lock
	var symphonyService symphony.Symphony
	var prepareInit func(context.Context, string, *string) (daemon.PreparedInit, error)
	var initOptions func(context.Context) (protocol.InitOptionsResponse, error)
	var recordInitFailure func(context.Context, string) error
	var afterListen func(context.Context) error
	var afterHandoffCommit func(context.Context) error
	var afterCommit func(context.Context)
	var mcpHandler http.Handler
	var applyGitIntent func(context.Context, string, string) (string, error)
	var schedulerControl func(context.Context, symphony.Command) (protocol.SchedulerControlResponse, error)
	var workbenchService *workbench.Service
	var observeRuntime workbench.RuntimeSnapshot
	if paths.DatabasePath != "" {
		worktreeRoot := paths.WorktreeRoot
		runtimeRoot := paths.RuntimeRoot
		runtimeBin := filepath.Join(runtimeRoot, "bin")
		instanceConfigPath := paths.ConfigPath
		assignedRepository, branchNamespace := "", ""
		if paths.Workspace != nil {
			if validateErr := paths.Workspace.ValidateSource(ctx); validateErr != nil {
				writeDaemonLog(stderr, "error", "workspace.identity_failed", validateErr, map[string]any{"workspace": paths.Workspace.Root})
				return 1
			}
			assignedRepository = paths.Workspace.BaseRepository
			branchNamespace = paths.Workspace.BranchNamespace()
		}
		pikaExecutable, _ := os.Executable()
		if integrationExecutable := os.Getenv("PIKA_GO_INTEGRATION_EXECUTABLE"); integrationExecutable != "" {
			if !filepath.IsAbs(integrationExecutable) {
				writeDaemonLog(stderr, "error", "provider.integration_executable_invalid", errors.New("PIKA_GO_INTEGRATION_EXECUTABLE must be absolute"), nil)
				return 2
			}
			pikaExecutable = filepath.Clean(integrationExecutable)
		}
		codexExecutable := os.Getenv("PIKA_GO_CODEX_EXECUTABLE")
		if codexExecutable == "" {
			codexExecutable, _ = exec.LookPath("codex")
		}
		cursorExecutable := os.Getenv("PIKA_GO_CURSOR_EXECUTABLE")
		if cursorExecutable == "" {
			cursorExecutable, _ = exec.LookPath("cursor-agent")
		}
		providerRegistry, registryErr := provider.NewRegistry(provider.NewCodexAdapter(), provider.NewCursorAdapter(provider.CursorOptions{
			Executable: cursorExecutable, RuntimeRoot: runtimeRoot, InstanceBin: runtimeBin, PikaExecutable: pikaExecutable,
		}))
		if registryErr != nil {
			writeDaemonLog(stderr, "error", "provider.registry_failed", registryErr, nil)
			return 1
		}
		var followUpInactivity time.Duration
		var followUpPolicies map[symphony.WorkRole]symphony.FollowUpPolicy
		var configuredIterationAgentCount int64
		if _, statErr := os.Stat(instanceConfigPath); statErr == nil {
			followUpConfig, configErr := configuration.LoadFollowUp(instanceConfigPath)
			if configErr != nil {
				writeDaemonLog(stderr, "error", "configuration.load_failed", configErr, nil)
				return 1
			}
			followUpInactivity = followUpConfig.PaneIdleTimeout
			followUpPolicies = symphonyFollowUpPolicies(followUpConfig)
			iterationAgents, configErr := configuration.LoadIterationAgentsWithRegistry(instanceConfigPath, providerRegistry)
			if configErr != nil {
				writeDaemonLog(stderr, "error", "configuration.load_failed", configErr, nil)
				return 1
			}
			configuredIterationAgentCount = int64(len(iterationAgents))
			if configErr := configuration.ProbeConfiguredProviders(ctx, instanceConfigPath, providerRegistry, map[string]string{"codex": codexExecutable, "cursor": cursorExecutable}); configErr != nil {
				writeDaemonLog(stderr, "error", "provider.preflight_failed", configErr, nil)
				return 1
			}
		}
		if override := os.Getenv("PIKA_GO_FOLLOWUP_INACTIVITY"); override != "" {
			followUpInactivity, err = time.ParseDuration(override)
			if err != nil || followUpInactivity <= 0 {
				writeDaemonLog(stderr, "error", "configuration.override_invalid", fmt.Errorf("invalid PIKA_GO_FOLLOWUP_INACTIVITY %q", override), nil)
				return 2
			}
		}
		if handoffCandidate {
			if err := handoffControl.Send(daemonupdate.MessagePrepared, nil); err != nil {
				writeDaemonLog(stderr, "error", "update.prepare_failed", err, nil)
				return 1
			}
			if err := handoffControl.Expect(daemonupdate.MessageActivate, 90*time.Second); err != nil {
				writeDaemonLog(stderr, "error", "update.activation_failed", err, nil)
				return 1
			}
			daemonLock, err = instance.AdoptLock(os.NewFile(uintptr(*handoffLockFD), paths.LockPath))
		} else {
			daemonLock, err = instance.AcquireLock(paths.LockPath)
		}
		if err != nil {
			writeDaemonLog(stderr, "error", "instance.lock_failed", err, map[string]any{"instance_id": paths.InstanceID})
			return 1
		}
		defer daemonLock.Close()
		engine, err = symphony.Open(ctx, paths.DatabasePath, symphony.Options{FollowUpInactivity: followUpInactivity, FollowUpPolicies: followUpPolicies, Providers: providerRegistry})
		if err != nil {
			writeDaemonLog(stderr, "error", "database.open_failed", err, map[string]any{"instance_id": paths.InstanceID})
			return 1
		}
		defer func() {
			if engine != nil {
				_ = engine.Close()
			}
		}()
		if configuredIterationAgentCount != 0 {
			if handoffCandidate {
				view, inspectErr := engine.Inspect(ctx, symphony.Status{})
				if inspectErr != nil {
					writeDaemonLog(stderr, "error", "configuration.iteration_agents_failed", inspectErr, nil)
					return 1
				}
				if view.Optimization.ID != "" && view.Optimization.IterationConcurrency != configuredIterationAgentCount {
					writeDaemonLog(stderr, "error", "configuration.iteration_agents_failed", errors.New("Iteration Agent count changes require a cold daemon restart"), nil)
					return 1
				}
			} else if reconfigureErr := engine.ReconfigureIterationAgents(ctx, configuredIterationAgentCount); reconfigureErr != nil {
				writeDaemonLog(stderr, "error", "configuration.iteration_agents_failed", reconfigureErr, nil)
				return 1
			}
		}
		if paths.Workspace != nil {
			identity := paths.Workspace.Identity
			if err := engine.EnsureWorkspaceIdentity(ctx, symphony.WorkspaceIdentity{
				ID: identity.ID, Root: identity.Root, SourceRepository: identity.SourceRepository, GitCommonDir: identity.GitCommonDir,
				GitCommonDirDevice: identity.GitCommonDirDevice, GitCommonDirInode: identity.GitCommonDirInode, InitialSHA: identity.InitialSHA,
			}); err != nil {
				writeDaemonLog(stderr, "error", "workspace.database_identity_failed", err, map[string]any{"workspace": paths.Workspace.Root})
				return 1
			}
			baseWorkspace := gitworkspace.Workspace{Repository: assignedRepository, Root: worktreeRoot, Namespace: branchNamespace}
			baseSHA, headErr := baseWorkspace.SourceHEAD(ctx)
			if headErr != nil {
				writeDaemonLog(stderr, "error", "workspace.base_failed", headErr, nil)
				return 1
			}
			if err := engine.UpsertGitWorktree(ctx, symphony.GitWorktreeRecord{
				Role: "base", Branch: paths.Workspace.BaseBranch(), Repository: assignedRepository, HeadSHA: baseSHA, State: "active",
			}); err != nil {
				writeDaemonLog(stderr, "error", "workspace.registry_failed", err, nil)
				return 1
			}
		}
		activeSessions, activeErr := engine.ActiveAgentSessions(ctx)
		if activeErr != nil {
			writeDaemonLog(stderr, "error", "provider.reconciliation_failed", activeErr, nil)
			return 1
		}
		if handoffCandidate {
			handoffSessions = append([]symphony.ActiveAgentSession(nil), activeSessions...)
		}
		activeSessionIDs := make(map[string]bool, len(activeSessions))
		for _, active := range activeSessions {
			activeSessionIDs[active.Session.ID] = true
		}
		if reconcileErr := provider.ReconcileCursorSessions(runtimeRoot, activeSessionIDs); reconcileErr != nil {
			writeDaemonLog(stderr, "error", "provider.reconciliation_failed", reconcileErr, nil)
			return 1
		}
		symphonyService = engine
		workbenchService = workbench.New(engine, func(observeCtx context.Context) (workruntime.Snapshot, error) {
			if observeRuntime == nil {
				return workruntime.Snapshot{}, nil
			}
			return observeRuntime(observeCtx)
		})
		codexHome := os.Getenv("CODEX_HOME")
		if codexHome == "" {
			if userHome, homeErr := os.UserHomeDir(); homeErr == nil {
				codexHome = filepath.Join(userHome, ".codex")
			}
		}
		cursorMCPPath := os.Getenv("PIKA_GO_CURSOR_MCP_PATH")
		cursorHooksPath := os.Getenv("PIKA_GO_CURSOR_HOOKS_PATH")
		if cursorMCPPath == "" {
			if userHome, homeErr := os.UserHomeDir(); homeErr == nil {
				cursorMCPPath = filepath.Join(userHome, ".cursor", "mcp.json")
			}
		}
		if cursorHooksPath == "" {
			if userHome, homeErr := os.UserHomeDir(); homeErr == nil {
				cursorHooksPath = filepath.Join(userHome, ".cursor", "hooks.json")
			}
		}
		initializer := configuration.Initializer{
			Workspace: paths.Workspace, ConfigRoot: paths.ConfigRoot, StateRoot: paths.StateRoot, InstanceID: paths.InstanceID,
			CodexHome: codexHome, CursorMCPPath: cursorMCPPath, CursorHooksPath: cursorHooksPath, PikaExecutable: pikaExecutable, CodexExecutable: codexExecutable, Providers: providerRegistry,
			RequireConfigurationTOML: true, ProbeProviders: true,
			ProviderExecutables: map[string]string{"codex": codexExecutable, "cursor": cursorExecutable},
		}
		if paths.Workspace != nil && !handoffCandidate {
			if _, statErr := os.Stat(instanceConfigPath); statErr == nil {
				if _, refreshErr := initializer.Prepare(ctx, paths.Workspace.Identity.SourceRepository); refreshErr != nil {
					writeDaemonLog(stderr, "error", "provider.integration_refresh_failed", refreshErr, nil)
					return 1
				}
			} else if !errors.Is(statErr, os.ErrNotExist) {
				writeDaemonLog(stderr, "error", "provider.integration_refresh_failed", statErr, nil)
				return 1
			}
		}
		herdrSocket := os.Getenv("HERDR_SOCKET_PATH")
		prepareInit = func(initCtx context.Context, repository string, configurationTOML *string) (daemon.PreparedInit, error) {
			if herdrSocket != "" {
				configPath, resolveErr := herdr.ResolveConfigPath()
				if resolveErr != nil {
					return daemon.PreparedInit{}, resolveErr
				}
				if freshErr := herdr.RequireFreshSessions(initCtx, herdr.NewClient(herdrSocket), configPath); freshErr != nil {
					return daemon.PreparedInit{}, fmt.Errorf("require fresh Herdr Agent Sessions: %w", freshErr)
				}
			}
			preparedInitializer := initializer
			preparedInitializer.ConfigurationTOML = configurationTOML
			rollback, prepareErr := preparedInitializer.Prepare(initCtx, repository)
			if prepareErr != nil {
				return daemon.PreparedInit{}, prepareErr
			}
			schedulerConfig, configErr := configuration.LoadScheduler(instanceConfigPath)
			var iterationAgents []configuration.Agent
			if configErr == nil {
				iterationAgents, configErr = configuration.LoadIterationAgentsWithRegistry(instanceConfigPath, providerRegistry)
			}
			var contextConfig configuration.Context
			if configErr == nil {
				contextConfig, configErr = configuration.LoadContext(instanceConfigPath)
			}
			var followUpConfig configuration.FollowUp
			if configErr == nil {
				followUpConfig, configErr = configuration.LoadFollowUp(instanceConfigPath)
			}
			for _, role := range []string{"baseline", "baseline_verify", "integration", "follow_up"} {
				if configErr != nil {
					break
				}
				_, configErr = configuration.LoadAgentWithRegistry(instanceConfigPath, role, providerRegistry)
			}
			timeout := followUpConfig.PaneIdleTimeout
			if override := os.Getenv("PIKA_GO_FOLLOWUP_INACTIVITY"); override != "" {
				timeout, configErr = time.ParseDuration(override)
			}
			if configErr == nil {
				configErr = engine.SetFollowUpInactivity(timeout)
			}
			if configErr == nil {
				configErr = engine.SetFollowUpPolicies(symphonyFollowUpPolicies(followUpConfig))
			}
			if configErr != nil {
				if rollbackErr := rollback(); rollbackErr != nil {
					return daemon.PreparedInit{}, fmt.Errorf("configure instance: %v; rollback: %w", configErr, rollbackErr)
				}
				return daemon.PreparedInit{}, fmt.Errorf("configure instance: %w", configErr)
			}
			return daemon.PreparedInit{Rollback: rollback, IterationConcurrency: int64(len(iterationAgents)), MaxPendingAttempts: schedulerConfig.MaxPendingAttempts, IterationHistoryLimit: contextConfig.IterationHistoryLimit}, nil
		}
		initOptions = func(optionsCtx context.Context) (protocol.InitOptionsResponse, error) {
			_, statErr := os.Stat(instanceConfigPath)
			response := protocol.InitOptionsResponse{ConfigurationExists: statErr == nil}
			if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
				return protocol.InitOptionsResponse{}, fmt.Errorf("inspect instance configuration: %w", statErr)
			}
			for _, kind := range providerRegistry.Kinds() {
				option := protocol.ProviderOption{Kind: kind}
				probeCtx, cancelProbe := context.WithTimeout(optionsCtx, providerProbeTimeout(kind))
				capabilities, probeErr := providerRegistry.Probe(probeCtx, kind, provider.ProbeRequest{Executable: map[string]string{"codex": codexExecutable, "cursor": cursorExecutable}[kind]})
				cancelProbe()
				if probeErr != nil {
					option.Error = probeErr.Error()
					response.Providers = append(response.Providers, option)
					continue
				}
				option.Executable, option.Version = capabilities.Executable, capabilities.Version
				option.Compatible, option.Authenticated = capabilities.Compatible, capabilities.Authenticated
				option.Capabilities = map[string]bool{
					"journal": capabilities.Journal, "turn_stop": capabilities.TurnStop, "follow_up": capabilities.FollowUp,
					"full_output": capabilities.FullOutput, "fresh_session": capabilities.FreshSession, "interrupt": capabilities.Interrupt,
				}
				modelCtx, cancelModels := context.WithTimeout(optionsCtx, 4*time.Second)
				models, modelErr := providerRegistry.Models(modelCtx, kind, provider.ModelRequest{
					Executable: map[string]string{"codex": codexExecutable, "cursor": cursorExecutable}[kind],
					ConfigRoot: map[string]string{"codex": codexHome}[kind],
				})
				cancelModels()
				if modelErr != nil {
					option.Error = modelErr.Error()
					response.Providers = append(response.Providers, option)
					continue
				}
				for _, model := range models {
					option.Models = append(option.Models, protocol.ModelOption{
						ID: model.ID, DisplayName: model.DisplayName, ReasoningEfforts: model.ReasoningEfforts, Default: model.Default,
					})
				}
				response.Providers = append(response.Providers, option)
			}
			return response, nil
		}
		recordInitFailure = engine.RecordInitFailure
		symphonyPane := os.Getenv("HERDR_PANE_ID")
		if herdrSocket != "" && symphonyPane != "" {
			client := herdr.NewClient(herdrSocket)
			runtimeAdapter := workruntime.NewHerdrRuntime(client, symphonyPane)
			observeRuntime = runtimeAdapter.Snapshot
			runtimeAdapter.OnEvent = func(eventCtx context.Context, event herdr.Event) error {
				if event.Kind != "pane.updated" {
					return nil
				}
				var payload struct {
					Pane herdr.Pane `json:"pane"`
				}
				if err := json.Unmarshal(event.Data, &payload); err != nil {
					return fmt.Errorf("decode Herdr pane.updated: %w", err)
				}
				if payload.Pane.PaneID == "" {
					return errors.New("decode Herdr pane.updated: pane_id is required")
				}
				return engine.ObserveAgentStatus(eventCtx, payload.Pane.PaneID, payload.Pane.AgentStatus)
			}
			runtimeAdapter.LaunchDir = runtimeRoot
			pane, err := herdr.NewRuntime(client).GetPane(ctx, symphonyPane)
			if err != nil {
				writeDaemonLog(stderr, "error", "herdr.symphony_pane_failed", err, map[string]any{"pane_id": symphonyPane})
				return 1
			}
			if err := herdr.NewRuntime(client).ReportInstance(ctx, pane.WorkspaceID, paths.InstanceID); err != nil {
				writeDaemonLog(stderr, "error", "herdr.instance_report_failed", err, map[string]any{"workspace_id": pane.WorkspaceID})
				return 1
			}
			pathEntries := []string{runtimeBin, os.Getenv("PATH")}
			if prefix := os.Getenv("PIKA_GO_AGENT_PATH_PREFIX"); prefix != "" {
				pathEntries = append([]string{prefix}, pathEntries...)
			}
			activationEnvironment := map[string]string{"PATH": strings.Join(pathEntries, string(os.PathListSeparator))}
			preparer := activation.Preparer{Store: engine, InstructionRoot: paths.InstructionsRoot, ContextsRoot: paths.ContextsRoot, SocketPath: paths.SocketPath, Environment: activationEnvironment, AgentConfigPath: instanceConfigPath, Providers: providerRegistry}
			dispatcher := outbox.Dispatcher{Store: engine, Sink: workruntime.Sink{Store: engine, Runtime: runtimeAdapter, AgentKind: "codex", AgentConfigPath: instanceConfigPath, Providers: providerRegistry, ProviderRuntimeRoot: runtimeRoot, RequireProviderCapabilities: true, Preparer: preparer, WorkspacePreparer: gitworkspace.RuntimePreparer{Repository: assignedRepository, Root: worktreeRoot, Namespace: branchNamespace, Recorder: engine}}}
			coordinator := workruntime.Coordinator{
				Reconciler:  workruntime.Reconciler{Store: engine, Runtime: runtimeAdapter, ProviderRuntimeRoot: runtimeRoot},
				Dispatcher:  dispatcher,
				EffectStore: engine,
				Runtime:     runtimeAdapter,
				OnWatchError: func(watchErr error) {
					writeDaemonLog(stderr, "warn", "herdr.watch_reconnecting", watchErr, nil)
				},
			}
			if !handoffCandidate {
				if err := engine.RetireActiveSessionsForRecovery(ctx); err != nil {
					writeDaemonLog(stderr, "error", "runtime.session_retirement_failed", err, nil)
					return 1
				}
			}
			dispatchRequested := make(chan struct{}, 1)
			afterCommit = func(context.Context) {
				select {
				case dispatchRequested <- struct{}{}:
				default:
				}
			}
			schedulerController := scheduler.Controller{
				Store: engine, Dispatcher: dispatcher, Exclusive: &coordinator,
				After: func() { afterCommit(ctx) },
			}
			schedulerControl = schedulerController.Apply
			startRuntime := func(listenCtx context.Context) error {
				if err := coordinator.RecoverAndDispatch(listenCtx); err != nil {
					return fmt.Errorf("recover runtime after daemon listen: %w", err)
				}
				runtimeWG.Add(1)
				go func() {
					defer runtimeWG.Done()
					for {
						select {
						case <-runtimeCtx.Done():
							return
						case <-dispatchRequested:
							if err := coordinator.RecoverAndDispatch(runtimeCtx); err != nil {
								writeDaemonLog(stderr, "error", "runtime.dispatch_failed", err, nil)
							}
						}
					}
				}()
				runtimeWG.Add(1)
				go func() {
					defer runtimeWG.Done()
					ticker := time.NewTicker(time.Second)
					defer ticker.Stop()
					for {
						select {
						case <-runtimeCtx.Done():
							return
						case <-ticker.C:
							promoted, err := engine.PromoteDueFollowUps(runtimeCtx)
							if err != nil {
								writeDaemonLog(stderr, "error", "follow_up.promotion_failed", err, nil)
							} else if promoted {
								afterCommit(runtimeCtx)
							}
						}
					}
				}()
				runtimeWG.Add(1)
				go func() {
					defer runtimeWG.Done()
					_ = coordinator.Run(runtimeCtx)
				}()
				return nil
			}
			if handoffCandidate {
				afterHandoffCommit = startRuntime
			} else {
				afterListen = startRuntime
			}
		} else {
			dispatcher := outbox.Dispatcher{Store: engine, Sink: &outbox.Recorder{}}
			if err := engine.RecoverUncertainEffects(ctx); err != nil {
				writeDaemonLog(stderr, "error", "outbox.recovery_failed", err, nil)
				return 1
			}
			afterCommit = func(ctx context.Context) { _ = dispatcher.DispatchPending(ctx) }
			afterCommit(ctx)
		}
		mcpHandler = mcp.Handler{Application: toolapp.Application{Store: engine, WorktreeRoot: worktreeRoot, Repository: assignedRepository, BranchNamespace: branchNamespace, WorktreeRecorder: engine}, AfterMutation: func() {
			if afterCommit != nil {
				afterCommit(ctx)
			}
		}}
		applyGitIntent = func(ctx context.Context, intentID, message string) (string, error) {
			intent, err := engine.GitIntent(ctx, intentID)
			if err != nil {
				return "", err
			}
			if intent.State != "pending" {
				return "", fmt.Errorf("Git intent %s is %s", intent.ID, intent.State)
			}
			view, err := engine.Inspect(ctx, symphony.Status{})
			if err != nil {
				return "", err
			}
			repository := assignedRepository
			if repository == "" {
				repository = view.Optimization.Repository
			}
			workspace := gitworkspace.Workspace{Repository: repository, Root: worktreeRoot, Namespace: branchNamespace}
			appliedSHA, applyErr := workspace.ApplyAuthorizedBestUpdate(ctx, intent.ID, intent.ExpectedBestSHA, intent.CandidateSHA, message)
			if applyErr != nil {
				return "", applyErr
			}
			if err := engine.UpsertGitWorktree(ctx, symphony.GitWorktreeRecord{
				Role: "best", Branch: workspace.BestBranch(), Repository: filepath.Join(worktreeRoot, "best", "repo"), HeadSHA: appliedSHA, State: "active",
			}); err != nil {
				return "", err
			}
			return appliedSHA, nil
		}
	}
	var ingestProviderEvent func(context.Context, string, string, json.RawMessage) error
	var ingestProviderHookEvent func(context.Context, string, string, string, json.RawMessage) error
	if engine != nil {
		ingestProviderEvent = engine.IngestProviderEvent
		ingestProviderHookEvent = engine.IngestProviderHookEvent
	}
	var backup func(context.Context, string) error
	var drainReady func(context.Context) (bool, error)
	if engine != nil {
		backup = engine.Backup
		drainReady = engine.DrainReady
	}
	var listener net.Listener
	if handoffCandidate {
		listenerFile := os.NewFile(uintptr(*handoffListenerFD), paths.SocketPath)
		listener, err = net.FileListener(listenerFile)
		_ = listenerFile.Close()
		if err != nil {
			writeDaemonLog(stderr, "error", "update.listener_failed", err, nil)
			return 1
		}
	} else {
		listener, err = daemon.Listen(paths.SocketPath)
		if err != nil {
			writeDaemonLog(stderr, "error", "daemon.listen_failed", err, nil)
			return 1
		}
	}
	removeSocket := !handoffCandidate
	defer func() {
		if removeSocket {
			_ = os.Remove(paths.SocketPath)
		}
	}()

	binaryDigest := ""
	if paths.Workspace != nil {
		if handoffCandidate {
			_, err = daemonupdate.ReadCurrent(paths.Workspace.Root)
			if err == nil {
				executable, executableErr := os.Executable()
				if executableErr == nil {
					binaryDigest, err = daemonupdate.FileDigest(executable)
				}
			}
		} else {
			executable, executableErr := os.Executable()
			if executableErr != nil {
				err = executableErr
			} else {
				var current daemonupdate.Generation
				current, err = daemonupdate.EnsureCurrent(ctx, paths.Workspace.Root, executable, Version)
				binaryDigest = current.Digest
			}
		}
		if err != nil {
			writeDaemonLog(stderr, "error", "update.bootstrap_failed", err, nil)
			return 1
		}
	}

	var updateMu sync.Mutex
	var pending *daemonupdate.Handoff
	updateAccepted := make(chan struct{}, 1)
	successorArgs := []string{"--socket", paths.SocketPath}
	if paths.Workspace != nil {
		successorArgs = append(successorArgs, "--workspace", paths.Workspace.Root)
	}
	if paths.ConfigRoot != "" {
		successorArgs = append(successorArgs, "--config-dir", paths.ConfigRoot)
	}
	if paths.StateRoot != "" {
		successorArgs = append(successorArgs, "--state-dir", paths.StateRoot)
	}
	if paths.InstanceID != "" {
		successorArgs = append(successorArgs, "--instance", paths.InstanceID)
	}
	spawnSuccessor := func(path string, listenerKeep, lockKeep *os.File) (*daemonupdate.Child, error) {
		listenerChild, duplicateErr := daemonupdate.DuplicateFile(listenerKeep)
		if duplicateErr != nil {
			return nil, duplicateErr
		}
		defer listenerChild.Close()
		lockChild, duplicateErr := daemonupdate.DuplicateFile(lockKeep)
		if duplicateErr != nil {
			return nil, duplicateErr
		}
		defer lockChild.Close()
		return daemonupdate.Spawn(path, successorArgs, listenerChild, lockChild, stderr)
	}
	var prepareUpdate func(context.Context, daemonupdate.Candidate) (daemonupdate.Status, error)
	if paths.Workspace != nil && daemonLock != nil {
		prepareUpdate = func(_ context.Context, candidate daemonupdate.Candidate) (daemonupdate.Status, error) {
			updateMu.Lock()
			defer updateMu.Unlock()
			if !handoffCommitted.Load() {
				return daemonupdate.Status{}, errors.New("this daemon generation is still activating")
			}
			if pending != nil {
				return daemonupdate.Status{}, fmt.Errorf("update %s is already %s", pending.Status.ID, pending.Status.State)
			}
			if err := daemonupdate.ValidateCandidate(paths.Workspace.Root, candidate, daemonupdate.CurrentProbe(Version)); err != nil {
				return daemonupdate.Status{}, err
			}
			from, err := daemonupdate.ReadCurrent(paths.Workspace.Root)
			if err != nil {
				return daemonupdate.Status{}, err
			}
			status := daemonupdate.NewStatus(newRequestID(), from, candidate)
			if err := daemonupdate.WriteStatus(paths.Workspace.Root, status); err != nil {
				return daemonupdate.Status{}, err
			}
			unixListener, ok := listener.(interface{ File() (*os.File, error) })
			if !ok {
				return daemonupdate.Status{}, errors.New("daemon listener does not support descriptor handoff")
			}
			listenerKeep, err := unixListener.File()
			if err != nil {
				return daemonupdate.Status{}, fmt.Errorf("duplicate daemon listener: %w", err)
			}
			lockKeep, err := daemonLock.Share()
			if err != nil {
				_ = listenerKeep.Close()
				return daemonupdate.Status{}, err
			}
			child, err := spawnSuccessor(candidate.Path, listenerKeep, lockKeep)
			if err != nil {
				_ = listenerKeep.Close()
				_ = lockKeep.Close()
				return daemonupdate.Status{}, err
			}
			if err := child.Control.Expect(daemonupdate.MessagePrepared, 75*time.Second); err != nil {
				_ = child.Command.Process.Kill()
				_, _ = child.Command.Process.Wait()
				_ = child.Control.Close()
				_ = listenerKeep.Close()
				_ = lockKeep.Close()
				status.State, status.Failure = daemonupdate.StateFailed, err.Error()
				_ = daemonupdate.WriteStatus(paths.Workspace.Root, status)
				return daemonupdate.Status{}, err
			}
			status.State = daemonupdate.StateQuiescing
			if err := daemonupdate.WriteStatus(paths.Workspace.Root, status); err != nil {
				_ = child.Command.Process.Kill()
				_, _ = child.Command.Process.Wait()
				_ = child.Control.Close()
				_ = listenerKeep.Close()
				_ = lockKeep.Close()
				return daemonupdate.Status{}, err
			}
			pending = &daemonupdate.Handoff{Child: child, Status: status, ListenerKeep: listenerKeep, LockKeep: lockKeep}
			return status, nil
		}
	}
	if handoffCandidate {
		startupAfterCommit := afterHandoffCommit
		afterListen = func(listenCtx context.Context) error {
			if UpdateFailBeforeReadyVersion != "" && UpdateFailBeforeReadyVersion == Version {
				return errors.New("injected update failure before readiness")
			}
			if engine != nil {
				currentSessions, err := engine.ActiveAgentSessions(listenCtx)
				if err != nil {
					return fmt.Errorf("verify active Agent Sessions before handoff: %w", err)
				}
				if !reflect.DeepEqual(currentSessions, handoffSessions) {
					return errors.New("active Agent Session or pane binding changed during daemon handoff")
				}
			}
			if err := handoffControl.Send(daemonupdate.MessageReady, nil); err != nil {
				return err
			}
			if err := handoffControl.Expect(daemonupdate.MessageCommit, 30*time.Second); err != nil {
				return err
			}
			handoffCommitted.Store(true)
			if UpdateFailAfterCommitVersion != "" && UpdateFailAfterCommitVersion == Version {
				return errors.New("injected update failure after commit")
			}
			if startupAfterCommit != nil {
				if err := startupAfterCommit(listenCtx); err != nil {
					return err
				}
			}
			generationReady.Store(true)
			return nil
		}
	}
	writeDaemonLog(stderr, "info", "daemon.starting", nil, map[string]any{"instance_id": paths.InstanceID, "protocol_version": protocol.Version, "version": Version})
	serveErr := daemon.Serve(ctx, daemon.Config{SocketPath: paths.SocketPath, Listener: listener, Version: Version, InstanceID: paths.InstanceID, Symphony: symphonyService, PrepareInit: prepareInit, InitOptions: initOptions, RecordInitFailure: recordInitFailure, AfterListen: afterListen, AfterCommit: afterCommit, MCPHandler: mcpHandler, ApplyGitIntent: applyGitIntent, IngestProviderEvent: ingestProviderEvent, IngestProviderHookEvent: ingestProviderHookEvent, Backup: backup, DrainReady: drainReady, SchedulerControl: schedulerControl, PrepareUpdate: prepareUpdate, UpdateAccepted: updateAccepted, BinaryDigest: binaryDigest, Ready: generationReady.Load, Workbench: workbenchService})
	if errors.Is(serveErr, daemon.ErrHandoff) {
		removeSocket = false
		cancelRuntime()
		runtimeWG.Wait()
		if engine != nil {
			_ = engine.Close()
			engine = nil
		}
		updateMu.Lock()
		handoff := pending
		updateMu.Unlock()
		if handoff == nil {
			writeDaemonLog(stderr, "error", "update.handoff_missing", errors.New("update accepted without a prepared successor"), nil)
			return 1
		}
		outcome, activationErr := daemonupdate.Activate(paths.Workspace.Root, handoff, spawnSuccessor, func(healthCtx context.Context) (string, error) {
			health, healthErr := control.Health(healthCtx, paths.SocketPath)
			return health.BinaryDigest, healthErr
		})
		handoff.Close()
		if activationErr != nil {
			writeDaemonLog(stderr, "error", "update.handoff_failed", activationErr, map[string]any{"update_id": handoff.Status.ID})
			return 1
		}
		if outcome == daemonupdate.OutcomeCommitted {
			writeDaemonLog(stderr, "info", "update.handoff_committed", nil, map[string]any{"update_id": handoff.Status.ID, "version": handoff.Status.To.Version})
			if os.Getenv("HERDR_SOCKET_PATH") != "" && os.Getenv("HERDR_PANE_ID") != "" && handoff.Child != nil && handoff.Child.Command != nil {
				// The original daemon is the foreground process that owns the Herdr
				// plugin pane. Keep that process alive as a lightweight supervisor;
				// otherwise Herdr closes the pane after a successful handoff and the
				// successor receives the pane teardown with it. Repeated updates form
				// a bounded-to-update-count supervisor chain that unwinds on shutdown.
				if waitErr := handoff.Child.Command.Wait(); waitErr != nil {
					writeDaemonLog(stderr, "warn", "update.successor_exited", waitErr, map[string]any{"update_id": handoff.Status.ID})
				}
			}
		} else {
			rolledBack, _ := daemonupdate.ReadStatus(paths.Workspace.Root)
			writeDaemonLog(stderr, "warn", "update.handoff_rolled_back", errors.New(rolledBack.Failure), map[string]any{"update_id": handoff.Status.ID})
		}
		return 0
	}
	if serveErr != nil {
		cancelRuntime()
		runtimeWG.Wait()
		writeDaemonLog(stderr, "error", "daemon.stopped", serveErr, map[string]any{"instance_id": paths.InstanceID})
		return 1
	}
	cancelRuntime()
	runtimeWG.Wait()
	if engine != nil {
		_ = engine.Close()
		engine = nil
	}
	writeDaemonLog(stderr, "info", "daemon.stopped", nil, map[string]any{"instance_id": paths.InstanceID})
	return 0
}

func providerProbeTimeout(kind string) time.Duration {
	if kind == "cursor" {
		// Cursor independently bounds its version command at fifteen seconds and
		// its authenticated status command at forty-five seconds. Leave headroom
		// around both stages for process startup and cancellation propagation.
		return 65 * time.Second
	}
	return 2 * time.Second
}

func writeDaemonLog(output io.Writer, level, event string, err error, fields map[string]any) {
	record := map[string]any{
		"time":  time.Now().UTC().Format(time.RFC3339Nano),
		"level": level,
		"event": event,
	}
	if err != nil {
		record["error"] = err.Error()
	}
	for key, value := range fields {
		record[key] = value
	}
	_ = json.NewEncoder(output).Encode(record)
}

func symphonyFollowUpPolicies(config configuration.FollowUp) map[symphony.WorkRole]symphony.FollowUpPolicy {
	convert := func(policy configuration.FollowUpPolicy) symphony.FollowUpPolicy {
		return symphony.FollowUpPolicy{MaxMessages: policy.MaxMessages, GeneratorMaxAttempts: policy.GeneratorMaxAttempts}
	}
	return map[symphony.WorkRole]symphony.FollowUpPolicy{
		symphony.RoleBaselineVerification: convert(config.BaselineVerify),
		symphony.RoleIteration:            convert(config.Iteration),
		symphony.RoleIntegration:          convert(config.Integration),
	}
}

func runHook(ctx context.Context, args []string, stdin io.Reader, stdout io.Writer) int {
	if len(args) < 1 || len(args) > 2 || (args[0] != "codex" && args[0] != "cursor") || stdin == nil {
		return 0
	}
	providerKind := args[0]
	configuredEventName := ""
	if len(args) == 2 {
		configuredEventName = args[1]
	}
	contents, err := io.ReadAll(stdin)
	if err != nil || !json.Valid(contents) {
		return 0
	}
	var envelope struct {
		HookEventName      string `json:"hook_event_name"`
		HookEventNameCamel string `json:"hookEventName"`
	}
	if err := json.Unmarshal(contents, &envelope); err != nil {
		return 0
	}
	if envelope.HookEventName == "" {
		envelope.HookEventName = envelope.HookEventNameCamel
	}
	if configuredEventName != "" {
		envelope.HookEventName = configuredEventName
	}
	if providerKind == "codex" && envelope.HookEventName == "Stop" {
		_, _ = io.WriteString(stdout, "{}\n")
	}
	if providerKind == "cursor" {
		response := map[string]any{}
		switch envelope.HookEventName {
		case "sessionStart":
			if path := os.Getenv("PIKA_CURSOR_SYSTEM_PROMPT_PATH"); path != "" {
				if prompt, readErr := os.ReadFile(path); readErr == nil && len(prompt) != 0 {
					response["additional_context"] = string(prompt)
				}
			}
		case "beforeSubmitPrompt":
			response["continue"] = true
		case "beforeMCPExecution":
			var policy struct {
				MCPServerName      string `json:"mcp_server_name"`
				MCPServerNameCamel string `json:"mcpServerName"`
			}
			_ = json.Unmarshal(contents, &policy)
			if policy.MCPServerName == "" {
				policy.MCPServerName = policy.MCPServerNameCamel
			}
			if policy.MCPServerName == "pika_go" {
				response["permission"] = "allow"
			} else {
				response["permission"] = "deny"
				response["user_message"] = "Pika blocked a non-Pika MCP server for this Session."
				response["agent_message"] = "Pika only allows its Session-scoped MCP server."
			}
		}
		_ = json.NewEncoder(stdout).Encode(response)
	}
	socketPath, err := instance.ResolveSocketContext(ctx, "")
	if err != nil {
		return 0
	}
	_ = control.IngestProviderEvent(ctx, socketPath, providerKind, protocol.ProviderEventRequest{
		AgentSessionID: os.Getenv("PIKA_SESSION_ID"), HookEventName: configuredEventName, Event: contents,
	})
	return 0
}

func runApplyBestUpdate(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := newCommandFlagSet("apply-best-update", stderr)
	socketPath := flags.String("socket", "", "Unix socket `PATH`")
	message := flags.String("m", "", "Best commit `MESSAGE`")
	if ok, code := parseCommandFlags(flags, args); !ok {
		return code
	}
	if flags.NArg() != 1 {
		_, _ = fmt.Fprintln(stderr, "apply-best-update: exactly one INTENT_ID is required")
		return 2
	}
	resolvedSocket, err := instance.ResolveSocketContext(ctx, *socketPath)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "apply-best-update: %v\n", err)
		return 2
	}
	response, err := control.ApplyGitIntent(ctx, resolvedSocket, flags.Arg(0), protocol.ApplyGitIntentRequest{Message: *message})
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "apply-best-update: %v\n", err)
		return 1
	}
	if err := json.NewEncoder(stdout).Encode(response); err != nil {
		_, _ = fmt.Fprintf(stderr, "apply-best-update: encode output: %v\n", err)
		return 1
	}
	return 0
}

func runMCPProxy(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	flags := newCommandFlagSet("mcp-proxy", stderr)
	socketPath := flags.String("socket", "", "Unix socket `PATH`")
	if ok, code := parseCommandFlags(flags, args); !ok {
		return code
	}
	resolvedSocket, err := instance.ResolveSocketContext(ctx, *socketPath)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "mcp-proxy: %v\n", err)
		return 2
	}
	if err := mcp.RunProxy(ctx, resolvedSocket, os.Getenv("PIKA_MCP_GRANT"), stdin, stdout); err != nil {
		_, _ = fmt.Fprintf(stderr, "mcp-proxy: %v\n", err)
		return 1
	}
	return 0
}

func runEditInstruction(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	flags := newCommandFlagSet("edit-instruction", stderr)
	workspaceRoot := flags.String("workspace", "", "Optimization Workspace `PATH`")
	configRoot := flags.String("config-dir", "", "Plugin configuration directory `PATH`")
	instanceID := flags.String("instance", "", "Pika instance `ID`")
	socketPath := flags.String("socket", "", "Unix socket `PATH` used to derive the instance")
	if ok, code := parseCommandFlags(flags, args); !ok {
		return code
	}
	if flags.NArg() != 1 {
		_, _ = fmt.Fprintln(stderr, "edit-instruction: exactly one instruction name is required")
		return 2
	}
	instructionRoot := ""
	workspaceCandidate := *workspaceRoot
	if workspaceCandidate == "" {
		workspaceCandidate = os.Getenv("PIKA_GO_WORKSPACE")
	}
	if workspaceCandidate != "" {
		workspace, err := optimizationworkspace.Open(workspaceCandidate)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "edit-instruction: %v\n", err)
			return 2
		}
		instructionRoot = workspace.InstructionsRoot
	} else if workspace, err := optimizationworkspace.Discover(""); err == nil {
		instructionRoot = workspace.InstructionsRoot
	}
	root := *configRoot
	if root == "" {
		root = os.Getenv("HERDR_PLUGIN_CONFIG_DIR")
	}
	if instructionRoot == "" && (root == "" || !filepath.IsAbs(root)) {
		_, _ = fmt.Fprintln(stderr, "edit-instruction: an absolute plugin config directory is required")
		return 2
	}
	id := *instanceID
	if instructionRoot == "" && id == "" {
		id = os.Getenv("PIKA_GO_INSTANCE")
	}
	if id == "" {
		resolved, err := instance.ResolveSocketContext(ctx, *socketPath)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "edit-instruction: %v\n", err)
			return 2
		}
		id = strings.TrimSuffix(filepath.Base(resolved), filepath.Ext(resolved))
	}
	if instructionRoot == "" {
		if err := instance.ValidateID(id); err != nil {
			_, _ = fmt.Fprintf(stderr, "edit-instruction: %v\n", err)
			return 2
		}
		instructionRoot = filepath.Join(root, "instances", id, "instructions")
	}
	path, err := instructions.Path(instructionRoot, flags.Arg(0))
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "edit-instruction: %v\n", err)
		return 2
	}
	if _, err := os.Stat(path); err != nil {
		_, _ = fmt.Fprintf(stderr, "edit-instruction: %v\n", err)
		return 1
	}
	editor := os.Getenv("VISUAL")
	if editor == "" {
		editor = os.Getenv("EDITOR")
	}
	parts := strings.Fields(editor)
	if len(parts) == 0 {
		_, _ = fmt.Fprintln(stderr, "edit-instruction: VISUAL or EDITOR is required")
		return 2
	}
	command := exec.CommandContext(ctx, parts[0], append(parts[1:], path)...)
	command.Stdin, command.Stdout, command.Stderr = stdin, stdout, stderr
	if err := command.Run(); err != nil {
		_, _ = fmt.Fprintf(stderr, "edit-instruction: editor failed: %v\n", err)
		return 1
	}
	return 0
}

func runStatus(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := newCommandFlagSet("status", stderr)
	socketPath := flags.String("socket", "", "Unix socket `PATH`")
	jsonOutput := flags.Bool("json", false, "Print JSON")
	if ok, code := parseCommandFlags(flags, args); !ok {
		return code
	}
	resolvedSocket, err := instance.ResolveSocketContext(ctx, *socketPath)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "status: %v\n", err)
		return 2
	}
	view, statusErr := control.Status(ctx, resolvedSocket)
	if statusErr == nil {
		if *jsonOutput {
			if err := json.NewEncoder(stdout).Encode(view); err != nil {
				_, _ = fmt.Fprintf(stderr, "status: encode output: %v\n", err)
				return 1
			}
			return 0
		}
		_, _ = fmt.Fprintf(stdout, "optimization %s: %s (revision %d), scheduler: %s\n", view.Optimization.ID, view.Optimization.Status, view.Optimization.Revision, view.Scheduler.Status)
		if view.IterationCaseSet != nil {
			_, _ = fmt.Fprintf(stdout, "iteration cases: v%d, %d cases\n", view.IterationCaseSet.Version, len(view.IterationCaseSet.CaseIDs))
		}
		return 0
	}
	if !control.IsHTTPStatus(statusErr, http.StatusNotFound) {
		_, _ = fmt.Fprintf(stderr, "status: %v\n", statusErr)
		return 1
	}
	health, err := control.Health(ctx, resolvedSocket)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "status: %v\n", err)
		return 1
	}
	if *jsonOutput {
		if err := json.NewEncoder(stdout).Encode(health); err != nil {
			_, _ = fmt.Fprintf(stderr, "status: encode output: %v\n", err)
			return 1
		}
		return 0
	}
	_, _ = fmt.Fprintf(stdout, "pika-go daemon %s (protocol %d, version %s)\n", health.Status, health.ProtocolVersion, health.Version)
	return 0
}

func runInit(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	return runInitForPane(ctx, args, stdin, stdout, stderr, os.Getenv("HERDR_PANE_ID"))
}

func runInitForPane(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, callerPaneID string) int {
	flags := newCommandFlagSet("init", stderr)
	socketPath := flags.String("socket", "", "Unix socket `PATH`")
	repository := flags.String("repository", "", "Absolute repository `PATH`")
	requestID := flags.String("request-id", "", "Idempotency request `ID`")
	jsonOutput := flags.Bool("json", false, "Print JSON only")
	defaults := flags.Bool("defaults", false, "Use the all-Codex default configuration")
	configPath := flags.String("config", "", "Read the complete instance TOML from `PATH`")
	if ok, code := parseCommandFlags(flags, args); !ok {
		return code
	}
	if *repository == "" || !filepath.IsAbs(*repository) {
		_, _ = fmt.Fprintln(stderr, "init: repository must be an absolute path")
		return 2
	}
	info, err := os.Stat(*repository)
	if err != nil || !info.IsDir() {
		_, _ = fmt.Fprintf(stderr, "init: repository is not a directory: %s\n", *repository)
		return 2
	}
	resolvedRepository, err := filepath.EvalSymlinks(*repository)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "init: resolve repository: %v\n", err)
		return 2
	}
	*repository = resolvedRepository
	resolvedSocket, err := instance.ResolveSocketContext(ctx, *socketPath)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "init: %v\n", err)
		return 2
	}
	if *requestID == "" {
		*requestID = newRequestID()
	}
	if *defaults && *configPath != "" {
		_, _ = fmt.Fprintln(stderr, "init: --defaults and --config are mutually exclusive")
		return 2
	}
	options, err := control.InitOptions(ctx, resolvedSocket)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "init: load options: %v\n", err)
		return 1
	}
	var configurationTOML *string
	if options.ConfigurationExists {
		if *defaults || *configPath != "" {
			_, _ = fmt.Fprintln(stderr, "init: existing configuration is user-owned; --defaults and --config are not allowed")
			return 2
		}
	} else {
		var candidate string
		switch {
		case *defaults:
			candidate = configuration.RenderDefaults(*repository)
		case *configPath != "":
			contents, readErr := os.ReadFile(*configPath)
			if readErr != nil {
				_, _ = fmt.Fprintf(stderr, "init: read --config: %v\n", readErr)
				return 2
			}
			candidate = string(contents)
		case *jsonOutput || !isInteractiveInput(stdin):
			_, _ = fmt.Fprintln(stderr, "init: a new non-interactive instance requires exactly one of --defaults or --config PATH")
			return 2
		default:
			_, _ = fmt.Fprintln(stdout, configuration.DefaultSummary)
			candidate, err = promptAgentConfiguration(stdin, stdout, *repository, options)
			if err != nil {
				_, _ = fmt.Fprintf(stderr, "init: configure Agents: %v\n", err)
				return 2
			}
		}
		configurationTOML = &candidate
	}
	if !*jsonOutput && options.ConfigurationExists {
		_, _ = fmt.Fprintln(stdout, "Using existing user-owned instance configuration.")
	}
	receipt, err := control.Init(ctx, resolvedSocket, protocol.InitRequest{
		Mutation:          protocol.Mutation{RequestID: *requestID},
		Repository:        *repository,
		CallerPaneID:      callerPaneID,
		ConfigurationTOML: configurationTOML,
	})
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "init: %v\n", err)
		return 1
	}
	if *jsonOutput {
		if err := json.NewEncoder(stdout).Encode(receipt); err != nil {
			_, _ = fmt.Fprintf(stderr, "init: encode output: %v\n", err)
			return 1
		}
		return 0
	}
	if _, err := fmt.Fprintf(stdout, "initialized optimization at revision %d (receipt %s)\n", receipt.Revision, receipt.ID); err != nil {
		_, _ = fmt.Fprintf(stderr, "init: encode output: %v\n", err)
		return 1
	}
	return 0
}

func isInteractiveInput(input io.Reader) bool {
	if input == nil {
		return false
	}
	file, isFile := input.(*os.File)
	if !isFile {
		return true
	}
	info, err := file.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

func promptAgentConfiguration(input io.Reader, output io.Writer, repository string, options protocol.InitOptionsResponse) (string, error) {
	reader := bufio.NewReader(input)
	available := make(map[string]protocol.ProviderOption, len(options.Providers))
	providerChoices := make([]promptChoice, 0, len(options.Providers))
	for _, option := range options.Providers {
		state := "available"
		if option.Error != "" {
			state = option.Error
		} else {
			available[option.Kind] = option
			providerChoices = append(providerChoices, promptChoice{Value: option.Kind, Label: option.Kind})
		}
		_, _ = fmt.Fprintf(output, "Provider %s: %s %s (%s)\n", option.Kind, option.Executable, option.Version, state)
	}
	agents := configuration.DefaultAgents()
	registry := provider.DefaultRegistry()
	var iterationAgents []configuration.Agent
	for _, role := range configuration.AgentRoleOrder {
		iterationNumber := 1
		for {
			defaults := agents[role]
			heading := "agents." + role
			if role == "iteration" {
				heading = fmt.Sprintf("agents.iteration[%d]", iterationNumber)
			}
			_, _ = fmt.Fprintf(output, "\nConfigure %s\n", heading)
			kind, err := promptList(reader, output, "  backend", defaults.Kind, providerChoices)
			if err != nil {
				return "", err
			}
			providerOption := available[kind]
			modelChoices := make([]promptChoice, 0, len(providerOption.Models))
			for _, model := range providerOption.Models {
				label := model.ID
				if model.Default {
					label = model.DisplayName
					if label == "" {
						label = "default"
					}
					if label != "default" {
						label += " (default)"
					}
				} else if model.DisplayName != "" && model.DisplayName != model.ID {
					label = fmt.Sprintf("%s (%s)", model.DisplayName, model.ID)
				}
				modelChoices = append(modelChoices, promptChoice{Value: model.ID, Label: label})
			}
			defaultModel := defaults.Model
			if defaults.Kind != kind || !modelOptionExists(providerOption.Models, defaultModel) {
				defaultModel = providerDefaultModel(providerOption.Models)
			}
			model, err := promptList(reader, output, "  model", defaultModel, modelChoices)
			if err != nil {
				return "", err
			}
			var efforts []string
			for _, option := range providerOption.Models {
				if option.ID == model {
					efforts = option.ReasoningEfforts
					break
				}
			}
			effort := ""
			if len(efforts) == 0 {
				_, _ = fmt.Fprintln(output, "  reasoning effort: provider default")
			} else {
				effortChoices := make([]promptChoice, 0, len(efforts))
				for _, availableEffort := range efforts {
					effortChoices = append(effortChoices, promptChoice{Value: availableEffort, Label: availableEffort})
				}
				defaultEffort := defaults.ReasoningEffort
				if defaults.Kind != kind || defaults.Model != model || !choiceExists(effortChoices, defaultEffort) {
					defaultEffort = efforts[0]
				}
				effort, err = promptList(reader, output, "  reasoning effort", defaultEffort, effortChoices)
				if err != nil {
					return "", err
				}
			}
			agent := configuration.Agent{Kind: kind, Model: model, ReasoningEffort: effort}
			if kind == "cursor" {
				agent.Args, err = promptCursorArgs(reader, output)
				if err != nil {
					return "", err
				}
			}
			adapter, err := registry.Resolve(kind)
			if err != nil {
				return "", err
			}
			if err := adapter.Validate(agent); err != nil {
				return "", fmt.Errorf("%s: %w", heading, err)
			}
			agents[role] = agent
			if role != "iteration" {
				break
			}
			iterationAgents = append(iterationAgents, agent)
			more, err := promptList(reader, output, "  configure another Iteration Agent", "no", []promptChoice{
				{Value: "no", Label: "No"},
				{Value: "yes", Label: "Yes"},
			})
			if err != nil {
				return "", err
			}
			if more == "no" {
				break
			}
			iterationNumber++
		}
	}
	return configuration.RenderConfiguration(repository, agents, iterationAgents...)
}

func promptCursorArgs(reader *bufio.Reader, output io.Writer) ([]string, error) {
	commandApproval, err := promptList(reader, output, "  Cursor command approval", "force", []promptChoice{
		{Value: "force", Label: "Allow commands automatically (recommended for unattended optimization; --force)"},
		{Value: "auto-review", Label: "Let Cursor auto-review commands (--auto-review)"},
		{Value: "ask", Label: "Ask before commands (may pause the optimization)"},
	})
	if err != nil {
		return nil, err
	}
	mcpApproval, err := promptList(reader, output, "  Cursor MCP approval", "approve", []promptChoice{
		{Value: "approve", Label: "Approve configured MCP servers automatically (recommended; --approve-mcps)"},
		{Value: "ask", Label: "Ask before approving MCP servers (may pause the optimization)"},
	})
	if err != nil {
		return nil, err
	}
	workspaceTrust, err := promptList(reader, output, "  Cursor workspace trust", "keep", []promptChoice{
		{Value: "keep", Label: "Keep the current workspace trust setting (recommended)"},
		{Value: "trust", Label: "Trust this workspace persistently (--trust)"},
	})
	if err != nil {
		return nil, err
	}
	var args []string
	switch commandApproval {
	case "force":
		args = append(args, "--force")
	case "auto-review":
		args = append(args, "--auto-review")
	}
	if mcpApproval == "approve" {
		args = append(args, "--approve-mcps")
	}
	if workspaceTrust == "trust" {
		args = append(args, "--trust")
	}
	return args, nil
}

func modelOptionExists(models []protocol.ModelOption, id string) bool {
	for _, model := range models {
		if model.ID == id {
			return true
		}
	}
	return false
}

func providerDefaultModel(models []protocol.ModelOption) string {
	for _, model := range models {
		if model.Default {
			return model.ID
		}
	}
	if len(models) != 0 {
		return models[0].ID
	}
	return ""
}

func choiceExists(choices []promptChoice, value string) bool {
	for _, choice := range choices {
		if choice.Value == value {
			return true
		}
	}
	return false
}

type promptChoice struct {
	Value string
	Label string
}

func promptList(reader *bufio.Reader, output io.Writer, label, defaultValue string, choices []promptChoice) (string, error) {
	if len(choices) == 0 {
		return "", fmt.Errorf("%s has no available choices", strings.TrimSpace(label))
	}
	defaultIndex := 0
	for index, choice := range choices {
		if choice.Value == defaultValue {
			defaultIndex = index
			break
		}
	}
	_, _ = fmt.Fprintf(output, "%s:\n", label)
	for index, choice := range choices {
		_, _ = fmt.Fprintf(output, "    %d) %s\n", index+1, choice.Label)
	}
	for {
		_, _ = fmt.Fprintf(output, "  Select %s [%d]: ", strings.TrimSpace(label), defaultIndex+1)
		line, err := reader.ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return "", err
		}
		value := strings.TrimSpace(line)
		if value == "" && !errors.Is(err, io.EOF) {
			return choices[defaultIndex].Value, nil
		}
		if errors.Is(err, io.EOF) && line == "" {
			return "", io.ErrUnexpectedEOF
		}
		selected, parseErr := strconv.Atoi(value)
		if parseErr == nil && selected >= 1 && selected <= len(choices) {
			return choices[selected-1].Value, nil
		}
		_, _ = fmt.Fprintf(output, "  Enter a number from 1 to %d.\n", len(choices))
	}
}

func promptValue(reader *bufio.Reader, output io.Writer, label, defaultValue string) (string, error) {
	_, _ = fmt.Fprintf(output, "%s [%s]: ", label, defaultValue)
	line, err := reader.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	value := strings.TrimSpace(line)
	if value == "" {
		value = defaultValue
	}
	if errors.Is(err, io.EOF) && line == "" {
		return "", io.ErrUnexpectedEOF
	}
	return value, nil
}

func runDraftBaseline(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := newCommandFlagSet("draft-baseline", stderr)
	socketPath, requestID, expectedRevision := mutationFlags(flags)
	if ok, code := parseCommandFlags(flags, args); !ok {
		return code
	}
	resolvedSocket, mutation, code := resolveMutation(ctx, *socketPath, *requestID, *expectedRevision, stderr)
	if code != 0 {
		return code
	}
	receipt, err := control.DraftBaseline(ctx, resolvedSocket, protocol.DraftBaselineRequest{Mutation: mutation})
	return writeMutationResult("draft-baseline", receipt, err, stdout, stderr)
}

func runBackOff(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := newCommandFlagSet("back-off", stderr)
	socketPath, requestID, expectedRevision := mutationFlags(flags)
	message := flags.String("m", "", "Back-off `MESSAGE`")
	if ok, code := parseCommandFlags(flags, args); !ok {
		return code
	}
	if *message == "" {
		_, _ = fmt.Fprintln(stderr, "back-off: -m message is required")
		return 2
	}
	resolvedSocket, mutation, code := resolveMutation(ctx, *socketPath, *requestID, *expectedRevision, stderr)
	if code != 0 {
		return code
	}
	view, err := control.Status(ctx, resolvedSocket)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "back-off: %v\n", err)
		return 1
	}
	workID := ""
	for index := len(view.Works) - 1; index >= 0; index-- {
		work := view.Works[index]
		if work.Role == symphony.RoleIntegration && (work.Status == symphony.WorkPending || work.Status == symphony.WorkCancelled) {
			workID = work.ID
			break
		}
	}
	if workID == "" {
		for index := len(view.Works) - 1; index >= 0; index-- {
			work := view.Works[index]
			if work.Role == symphony.RoleBaselineVerification && (work.Status == symphony.WorkPending || work.Status == symphony.WorkCancelled) {
				workID = work.ID
				break
			}
		}
	}
	if workID == "" {
		_, _ = fmt.Fprintln(stderr, "back-off: no current Integration or Baseline Verification Work")
		return 1
	}
	receipt, err := control.BackOff(ctx, resolvedSocket, protocol.BackOffRequest{Mutation: mutation, WorkID: workID, Message: *message})
	return writeMutationResult("back-off", receipt, err, stdout, stderr)
}

func runCancelWork(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := newCommandFlagSet("cancel-work", stderr)
	socketPath, requestID, expectedRevision := mutationFlags(flags)
	if ok, code := parseCommandFlags(flags, args); !ok {
		return code
	}
	if flags.NArg() != 1 {
		_, _ = fmt.Fprintln(stderr, "cancel-work: exactly one WORK_ID is required")
		return 2
	}
	resolvedSocket, mutation, code := resolveMutation(ctx, *socketPath, *requestID, *expectedRevision, stderr)
	if code != 0 {
		return code
	}
	receipt, err := control.CancelWork(ctx, resolvedSocket, flags.Arg(0), protocol.CancelWorkRequest{Mutation: mutation})
	return writeMutationResult("cancel-work", receipt, err, stdout, stderr)
}

func runShutdown(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := newCommandFlagSet("shutdown", stderr)
	socketPath, requestID, expectedRevision := mutationFlags(flags)
	if ok, code := parseCommandFlags(flags, args); !ok {
		return code
	}
	resolvedSocket, mutation, code := resolveMutation(ctx, *socketPath, *requestID, *expectedRevision, stderr)
	if code != 0 {
		return code
	}
	receipt, err := control.Shutdown(ctx, resolvedSocket, protocol.ShutdownRequest{Mutation: mutation})
	return writeMutationResult("shutdown", receipt, err, stdout, stderr)
}

func runSchedulerControl(ctx context.Context, action string, args []string, stdout, stderr io.Writer) int {
	flags := newCommandFlagSet(action, stderr)
	socketPath, requestID, expectedRevision := mutationFlags(flags)
	if ok, code := parseCommandFlags(flags, args); !ok {
		return code
	}
	if flags.NArg() != 0 {
		_, _ = fmt.Fprintf(stderr, "%s: positional arguments are not supported\n", action)
		return 2
	}
	resolvedSocket, mutation, code := resolveMutation(ctx, *socketPath, *requestID, *expectedRevision, stderr)
	if code != 0 {
		return code
	}
	request := protocol.SchedulerControlRequest{Mutation: mutation}
	var response protocol.SchedulerControlResponse
	var err error
	if action == "pause" {
		response, err = control.PauseScheduler(ctx, resolvedSocket, request)
	} else {
		response, err = control.ResumeScheduler(ctx, resolvedSocket, request)
	}
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "%s: %v\n", action, err)
		return 1
	}
	if err := json.NewEncoder(stdout).Encode(response); err != nil {
		_, _ = fmt.Fprintf(stderr, "%s: encode output: %v\n", action, err)
		return 1
	}
	if response.Control.HasDeliveryFailure() {
		return 1
	}
	return 0
}

func runBackup(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := newCommandFlagSet("backup", stderr)
	socketPath := flags.String("socket", "", "Unix socket `PATH`")
	destination := flags.String("output", "", "Absolute destination `PATH` for the SQLite snapshot")
	if ok, code := parseCommandFlags(flags, args); !ok {
		return code
	}
	if *destination == "" || !filepath.IsAbs(*destination) {
		_, _ = fmt.Fprintln(stderr, "backup: --output must be an absolute path")
		return 2
	}
	resolvedSocket, err := instance.ResolveSocketContext(ctx, *socketPath)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "backup: %v\n", err)
		return 2
	}
	response, err := control.Backup(ctx, resolvedSocket, protocol.BackupRequest{Destination: filepath.Clean(*destination)})
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "backup: %v\n", err)
		return 1
	}
	if err := json.NewEncoder(stdout).Encode(response); err != nil {
		_, _ = fmt.Fprintf(stderr, "backup: encode output: %v\n", err)
		return 1
	}
	return 0
}

func mutationFlags(flags *flag.FlagSet) (socketPath, requestID *string, expectedRevision *int64) {
	return flags.String("socket", "", "Unix socket `PATH`"),
		flags.String("request-id", "", "Idempotency request `ID`"),
		flags.Int64("expected-revision", -1, "Expected domain revision `N`")
}

func resolveMutation(ctx context.Context, socketPath, requestID string, expectedRevision int64, stderr io.Writer) (string, protocol.Mutation, int) {
	resolvedSocket, err := instance.ResolveSocketContext(ctx, socketPath)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "resolve daemon: %v\n", err)
		return "", protocol.Mutation{}, 2
	}
	if requestID == "" {
		requestID = newRequestID()
	}
	mutation := protocol.Mutation{RequestID: requestID}
	if expectedRevision >= 0 {
		mutation.ExpectedRevision = &expectedRevision
	}
	return resolvedSocket, mutation, 0
}

func writeMutationResult(name string, receipt symphony.Receipt, err error, stdout, stderr io.Writer) int {
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "%s: %v\n", name, err)
		return 1
	}
	if err := json.NewEncoder(stdout).Encode(receipt); err != nil {
		_, _ = fmt.Fprintf(stderr, "%s: encode output: %v\n", name, err)
		return 1
	}
	return 0
}

func newRequestID() string {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		panic(fmt.Sprintf("generate request ID: %v", err))
	}
	return fmt.Sprintf("%x", value[:])
}
