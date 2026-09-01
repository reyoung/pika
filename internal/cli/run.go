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
	"github.com/reyoung/pika-go/internal/evidence"
	"github.com/reyoung/pika-go/internal/gitworkspace"
	"github.com/reyoung/pika-go/internal/herdr"
	"github.com/reyoung/pika-go/internal/instance"
	"github.com/reyoung/pika-go/internal/instructions"
	"github.com/reyoung/pika-go/internal/maintenance"
	"github.com/reyoung/pika-go/internal/mcp"
	"github.com/reyoung/pika-go/internal/optimizationworkspace"
	"github.com/reyoung/pika-go/internal/outbox"
	"github.com/reyoung/pika-go/internal/plugininstall"
	"github.com/reyoung/pika-go/internal/protocol"
	"github.com/reyoung/pika-go/internal/provider"
	"github.com/reyoung/pika-go/internal/scheduler"
	"github.com/reyoung/pika-go/internal/skillsnapshot"
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

// newSkillSnapshotManager is an internal dependency seam. Production always
// returns the manager with its fixed allowlisted HTTPS sources; only package
// tests replace the factory with a Git transport that maps those remotes to
// committed local fixtures.
var newSkillSnapshotManager = func() skillsnapshot.Manager { return skillsnapshot.Manager{} }

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
	case "maintenance":
		return runMaintenance(ctx, args[1:], stdout, stderr)
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
	startupLease, err := optimizationworkspace.AcquireSharedMutationLease(ctx, workspace.Root)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "webui: acquire Workspace mutation lease: %v\n", err)
		return 1
	}
	defer startupLease.Close()
	var maintenanceStatus maintenance.Status
	authorizeWorkspace := func() bool {
		_, status, authorizeErr := authorizeWorkspaceHoldingWebUI(workspace.Root)
		if authorizeErr != nil {
			_, _ = fmt.Fprintf(stderr, "webui: maintenance state rejected: %v\n", authorizeErr)
			return false
		}
		maintenanceStatus = status
		return true
	}
	if !authorizeWorkspace() {
		return 1
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
	if !authorizeWorkspace() {
		return 1
	}
	holdingSafe := maintenanceStatus.State == maintenance.StateHolding
	if holdingSafe && *rotateToken {
		_, _ = fmt.Fprintln(stderr, "webui: --rotate-token is forbidden during maintenance Holding")
		return 1
	}
	if holdingSafe {
		err = workspace.ValidateLayout()
	} else {
		err = workspace.EnsureLayout()
	}
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "webui: validate Workspace layout: %v\n", err)
		return 1
	}
	var token, tokenPath string
	if holdingSafe {
		token, tokenPath, err = webuiapp.ReadToken(workspace.RuntimeRoot)
	} else {
		token, tokenPath, err = webuiapp.EnsureToken(workspace.RuntimeRoot, *rotateToken)
	}
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
	if !authorizeWorkspace() {
		return 1
	}
	if err := os.WriteFile(pidPath, []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600); err != nil {
		_, _ = fmt.Fprintf(stderr, "webui: write PID: %v\n", err)
		return 1
	}
	defer func() {
		cleanupLease, leaseErr := optimizationworkspace.AcquireSharedMutationLease(context.Background(), workspace.Root)
		if leaseErr == nil {
			if _, authorizeErr := authorizeWorkspaceExecutable(workspace.Root); authorizeErr == nil {
				if contents, readErr := os.ReadFile(pidPath); readErr == nil &&
					string(contents) == strconv.Itoa(os.Getpid())+"\n" {
					_ = os.Remove(pidPath)
				}
			}
		}
		if cleanupLease != nil {
			_ = cleanupLease.Close()
		}
	}()
	if err := startupLease.Close(); err != nil {
		_, _ = fmt.Fprintf(stderr, "webui: release Workspace mutation lease: %v\n", err)
		return 1
	}
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
	mutationLease, err := optimizationworkspace.AcquireSharedMutationLease(ctx, workspace.Root)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "update: acquire Workspace mutation lease: %v\n", err)
		return 1
	}
	defer mutationLease.Close()
	authorizeUpdate := func() bool {
		_, maintenanceStatus, authorizeErr := authorizeWorkspaceDirectMutation(workspace.Root)
		if authorizeErr != nil {
			if maintenanceStatus.State != "" {
				_, _ = fmt.Fprintf(stderr, "update: hot update is forbidden during maintenance state %s: %v\n", maintenanceStatus.State, authorizeErr)
			} else {
				_, _ = fmt.Fprintf(stderr, "update: maintenance generation rejected: %v\n", authorizeErr)
			}
			return false
		}
		return true
	}
	if !authorizeUpdate() {
		return 1
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
	if !authorizeUpdate() {
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

func authorizeWorkspaceExecutable(workspaceRoot string) (maintenance.Generation, error) {
	generation, _, err := authorizeWorkspaceExecutableWithStatus(workspaceRoot)
	return generation, err
}

func authorizeWorkspaceDirectMutation(workspaceRoot string) (maintenance.Generation, maintenance.Status, error) {
	executable, err := os.Executable()
	if err != nil {
		return maintenance.Generation{}, maintenance.Status{}, err
	}
	digest, err := daemonupdate.FileDigest(executable)
	if err != nil {
		return maintenance.Generation{}, maintenance.Status{}, err
	}
	generation := maintenance.Generation{Digest: digest, Version: Version}
	status, err := maintenance.AuthorizeDirectMutation(maintenance.NewFileStore(workspaceRoot), generation)
	return generation, status, err
}

func authorizeWorkspaceHoldingWebUI(workspaceRoot string) (maintenance.Generation, maintenance.Status, error) {
	executable, err := os.Executable()
	if err != nil {
		return maintenance.Generation{}, maintenance.Status{}, err
	}
	digest, err := daemonupdate.FileDigest(executable)
	if err != nil {
		return maintenance.Generation{}, maintenance.Status{}, err
	}
	generation := maintenance.Generation{Digest: digest, Version: Version}
	status, err := maintenance.AuthorizeHoldingWebUI(maintenance.NewFileStore(workspaceRoot), generation)
	return generation, status, err
}

func authorizeWorkspaceExecutableWithStatus(workspaceRoot string) (maintenance.Generation, maintenance.Status, error) {
	executable, err := os.Executable()
	if err != nil {
		return maintenance.Generation{}, maintenance.Status{}, fmt.Errorf("resolve current executable: %w", err)
	}
	digest, err := daemonupdate.FileDigest(executable)
	if err != nil {
		return maintenance.Generation{}, maintenance.Status{}, fmt.Errorf("digest current executable: %w", err)
	}
	generation := maintenance.Generation{Digest: digest, Version: Version}
	status, err := maintenance.AuthorizeGeneration(maintenance.NewFileStore(workspaceRoot), generation)
	if err != nil {
		return maintenance.Generation{}, maintenance.Status{}, err
	}
	return generation, status, nil
}

type daemonAuthorizationBarrier struct {
	reached chan<- struct{}
	release <-chan struct{}
}

type daemonAuthorizationBarrierContextKey struct{}

func waitForDaemonAuthorizationBarrier(ctx context.Context) error {
	barrier, ok := ctx.Value(daemonAuthorizationBarrierContextKey{}).(daemonAuthorizationBarrier)
	if !ok {
		return nil
	}
	select {
	case barrier.reached <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-barrier.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

type daemonMaintenanceAdmission struct {
	statusPresent bool
	status        maintenance.Status
	active        bool
	bindingBytes  []byte
	activeAgents  []symphony.ActiveAgentSession
	herdrSnapshot func(context.Context) (herdr.Snapshot, error)
}

func readMaintenanceDaemonAdmission(
	ctx context.Context,
	workspace optimizationworkspace.Workspace,
	socketPath string,
	store *maintenance.FileStore,
) (daemonMaintenanceAdmission, error) {
	status, err := store.Read()
	if errors.Is(err, os.ErrNotExist) {
		return daemonMaintenanceAdmission{}, nil
	}
	if err != nil {
		return daemonMaintenanceAdmission{}, err
	}
	admission := daemonMaintenanceAdmission{statusPresent: true, status: status}
	switch status.State {
	case maintenance.StateQuiescing, maintenance.StateReady, maintenance.StateHolding, maintenance.StateFailed:
		admission.active = true
	default:
		return admission, nil
	}
	admission.bindingBytes, err = os.ReadFile(workspace.HerdrBindingPath)
	if err != nil {
		return daemonMaintenanceAdmission{}, fmt.Errorf("read frozen Herdr binding bytes: %w", err)
	}
	binding, err := workspace.ReadHerdrBinding()
	if err != nil {
		return daemonMaintenanceAdmission{}, fmt.Errorf("read frozen Herdr binding: %w", err)
	}
	if binding.SocketPath == "" || binding.WorkspaceID == "" || binding.TabID == "" ||
		binding.ControlPane == "" || binding.DaemonPane == "" || binding.DaemonSocket == "" {
		return daemonMaintenanceAdmission{}, errors.New("frozen Herdr binding is incomplete")
	}
	if os.Getenv("HERDR_SOCKET_PATH") != binding.SocketPath {
		return daemonMaintenanceAdmission{}, errors.New("HERDR_SOCKET_PATH does not match frozen binding")
	}
	if os.Getenv("HERDR_PANE_ID") != binding.DaemonPane {
		return daemonMaintenanceAdmission{}, errors.New("HERDR_PANE_ID does not match frozen daemon pane")
	}
	if filepath.Clean(socketPath) != filepath.Clean(binding.DaemonSocket) {
		return daemonMaintenanceAdmission{}, errors.New("daemon socket does not match frozen binding")
	}
	client := herdr.NewClient(binding.SocketPath)
	snapshot, err := client.Snapshot(ctx)
	if err != nil {
		return daemonMaintenanceAdmission{}, fmt.Errorf("read live Herdr snapshot: %w", err)
	}
	if err := validateMaintenanceOpenSnapshot(snapshot, binding, nil); err != nil {
		return daemonMaintenanceAdmission{}, err
	}
	admission.activeAgents, err = symphony.ReadActiveAgentSessions(ctx, workspace.DatabasePath)
	if err != nil {
		return daemonMaintenanceAdmission{}, fmt.Errorf("read durable active Agent Sessions: %w", err)
	}
	if err := validateMaintenanceActiveSet(status, admission.activeAgents, snapshot); err != nil {
		return daemonMaintenanceAdmission{}, err
	}
	latestBinding, err := os.ReadFile(workspace.HerdrBindingPath)
	if err != nil || !reflect.DeepEqual(latestBinding, admission.bindingBytes) {
		return daemonMaintenanceAdmission{}, errors.New("frozen Herdr binding changed during exact admission")
	}
	latestStatus, err := store.Read()
	if err != nil || !reflect.DeepEqual(latestStatus, status) {
		return daemonMaintenanceAdmission{}, errors.New("maintenance status changed during exact admission")
	}
	admission.herdrSnapshot = client.Snapshot
	return admission, nil
}

func sameDaemonMaintenanceAdmission(before, after daemonMaintenanceAdmission) bool {
	return before.statusPresent == after.statusPresent &&
		before.active == after.active &&
		reflect.DeepEqual(before.status, after.status) &&
		reflect.DeepEqual(before.bindingBytes, after.bindingBytes) &&
		reflect.DeepEqual(before.activeAgents, after.activeAgents)
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
	resumeCheckpointFlags := addIntegrationResumeCheckpointFlags(flags)
	if ok, code := parseCommandFlags(flags, args); !ok {
		return code
	}
	var checkpointCleanup func()
	ctx, checkpointCleanup, err := resumeCheckpointFlags.decorate(ctx)
	if err != nil {
		writeDaemonLog(stderr, "error", "maintenance.checkpoint_invalid", err, nil)
		return 2
	}
	defer checkpointCleanup()
	paths, err := instance.ResolveRuntime(instance.RuntimeOptions{
		SocketPath: *socketPath, WorkspaceRoot: *workspaceRoot,
		ConfigRoot: *configDir, StateRoot: *stateDir, InstanceID: *instanceID,
	})
	if err != nil {
		writeDaemonLog(stderr, "error", "runtime.resolve_failed", err, nil)
		return 2
	}
	var startupMutationLease *optimizationworkspace.MutationLease
	if paths.Workspace != nil {
		startupMutationLease, err = optimizationworkspace.AcquireSharedMutationLease(ctx, paths.Workspace.Root)
		if err != nil {
			writeDaemonLog(stderr, "error", "workspace.mutation_lease_failed", err, nil)
			return 1
		}
		defer startupMutationLease.Close()
	}
	var launchGeneration maintenance.Generation
	if paths.Workspace != nil {
		launchGeneration, err = authorizeWorkspaceExecutable(paths.Workspace.Root)
		if err != nil {
			writeDaemonLog(stderr, "error", "maintenance.generation_rejected", err, nil)
			return 1
		}
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
	var maintenanceStore *maintenance.FileStore
	var maintenanceCoordinator *workruntime.Coordinator
	var maintenanceHerdrSnapshot func(context.Context) (herdr.Snapshot, error)
	var daemonMutationGate daemon.MutationGate
	var resumeRuntime func(context.Context, []maintenance.Target) error
	var resumePreflight func(context.Context) error
	maintenanceArbitration := &sync.Mutex{}
	maintenanceActive := false
	var preLockMaintenanceAdmission daemonMaintenanceAdmission
	if paths.Workspace != nil {
		maintenanceStore = maintenance.NewFileStore(paths.Workspace.Root)
		preLockMaintenanceAdmission, err = readMaintenanceDaemonAdmission(
			ctx, *paths.Workspace, paths.SocketPath, maintenanceStore,
		)
		if err != nil {
			writeDaemonLog(stderr, "error", "maintenance.herdr_admission_failed", err, nil)
			return 1
		}
		maintenanceActive = preLockMaintenanceAdmission.active
		maintenanceHerdrSnapshot = preLockMaintenanceAdmission.herdrSnapshot
	}
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
		codexHome := os.Getenv("CODEX_HOME")
		if codexHome == "" {
			if userHome, homeErr := os.UserHomeDir(); homeErr == nil {
				codexHome = filepath.Join(userHome, ".codex")
			}
		}
		providerModelRequests := map[string]provider.ModelRequest{
			"codex":  {Executable: codexExecutable, ConfigRoot: codexHome},
			"cursor": {Executable: cursorExecutable},
		}
		var followUpInactivity time.Duration
		var followUpPolicies map[symphony.WorkRole]symphony.FollowUpPolicy
		var configuredIterationAgentCount int64
		if _, statErr := os.Stat(instanceConfigPath); statErr == nil {
			resumePreflight = func(preflightCtx context.Context) error {
				return configuration.ProbeConfiguredProviders(preflightCtx, instanceConfigPath, providerRegistry,
					map[string]string{"codex": codexExecutable, "cursor": cursorExecutable}, providerModelRequests)
			}
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
			if !maintenanceActive {
				if configErr := resumePreflight(ctx); configErr != nil {
					writeDaemonLog(stderr, "error", "provider.preflight_failed", configErr, nil)
					return 1
				}
			}
		}
		if override := os.Getenv("PIKA_GO_FOLLOWUP_INACTIVITY"); override != "" {
			followUpInactivity, err = time.ParseDuration(override)
			if err != nil || followUpInactivity <= 0 {
				writeDaemonLog(stderr, "error", "configuration.override_invalid", fmt.Errorf("invalid PIKA_GO_FOLLOWUP_INACTIVITY %q", override), nil)
				return 2
			}
		}
		if err := waitForDaemonAuthorizationBarrier(ctx); err != nil {
			writeDaemonLog(stderr, "error", "maintenance.authorization_barrier_failed", err, nil)
			return 1
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
		if paths.Workspace != nil {
			launchGeneration, err = authorizeWorkspaceExecutable(paths.Workspace.Root)
			if err != nil {
				writeDaemonLog(stderr, "error", "maintenance.generation_rejected", err, nil)
				return 1
			}
			postLockAdmission, admissionErr := readMaintenanceDaemonAdmission(
				ctx, *paths.Workspace, paths.SocketPath, maintenanceStore,
			)
			if admissionErr != nil {
				writeDaemonLog(stderr, "error", "maintenance.herdr_admission_failed", admissionErr, nil)
				return 1
			}
			if !sameDaemonMaintenanceAdmission(preLockMaintenanceAdmission, postLockAdmission) {
				writeDaemonLog(stderr, "error", "maintenance.admission_changed",
					errors.New("maintenance state or exact durable/live identity changed while waiting for the instance lock"), nil)
				return 1
			}
			maintenanceActive = postLockAdmission.active
			maintenanceHerdrSnapshot = postLockAdmission.herdrSnapshot
			var layoutErr error
			if maintenanceActive {
				layoutErr = paths.Workspace.ValidateLayout()
			} else {
				layoutErr = paths.Workspace.EnsureLayout()
			}
			if layoutErr != nil {
				writeDaemonLog(stderr, "error", "workspace.layout_failed", layoutErr, nil)
				return 1
			}
		}
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
			} else if !maintenanceActive {
				if reconfigureErr := engine.ReconfigureIterationAgents(ctx, configuredIterationAgentCount); reconfigureErr != nil {
					writeDaemonLog(stderr, "error", "configuration.iteration_agents_failed", reconfigureErr, nil)
					return 1
				}
			}
		}
		if paths.Workspace != nil {
			identity := paths.Workspace.Identity
			expectedIdentity := symphony.WorkspaceIdentity{
				ID: identity.ID, Root: identity.Root, SourceRepository: identity.SourceRepository, GitCommonDir: identity.GitCommonDir,
				GitCommonDirDevice: identity.GitCommonDirDevice, GitCommonDirInode: identity.GitCommonDirInode, InitialSHA: identity.InitialSHA,
			}
			baseWorkspace := gitworkspace.Workspace{Repository: assignedRepository, Root: worktreeRoot, Namespace: branchNamespace}
			baseSHA, headErr := baseWorkspace.SourceHEAD(ctx)
			if headErr != nil {
				writeDaemonLog(stderr, "error", "workspace.base_failed", headErr, nil)
				return 1
			}
			expectedBase := symphony.GitWorktreeRecord{
				Role: "base", Branch: paths.Workspace.BaseBranch(), Repository: assignedRepository, HeadSHA: baseSHA, State: "active",
			}
			if maintenanceActive {
				if err := engine.ValidateWorkspaceIdentity(ctx, expectedIdentity); err != nil {
					writeDaemonLog(stderr, "error", "workspace.database_identity_failed", err, map[string]any{"workspace": paths.Workspace.Root})
					return 1
				}
				if err := engine.ValidateGitWorktree(ctx, expectedBase); err != nil {
					writeDaemonLog(stderr, "error", "workspace.registry_failed", err, nil)
					return 1
				}
			} else if err := engine.EnsureWorkspaceIdentity(ctx, expectedIdentity); err != nil {
				writeDaemonLog(stderr, "error", "workspace.database_identity_failed", err, map[string]any{"workspace": paths.Workspace.Root})
				return 1
			} else if err := engine.UpsertGitWorktree(ctx, expectedBase); err != nil {
				writeDaemonLog(stderr, "error", "workspace.registry_failed", err, nil)
				return 1
			}
		}
		if !maintenanceActive {
			if err := validateFlowV2Evidence(ctx, engine, paths.EvidenceRoot); err != nil {
				writeDaemonLog(stderr, "error", "workspace.evidence_failed", err, nil)
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
		if !maintenanceActive {
			if reconcileErr := provider.ReconcileCursorSessions(runtimeRoot, activeSessionIDs); reconcileErr != nil {
				writeDaemonLog(stderr, "error", "provider.reconciliation_failed", reconcileErr, nil)
				return 1
			}
		}
		symphonyService = engine
		workbenchService = workbench.New(engine, func(observeCtx context.Context) (workruntime.Snapshot, error) {
			if observeRuntime == nil {
				return workruntime.Snapshot{}, nil
			}
			return observeRuntime(observeCtx)
		})
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
			ProviderExecutables:   map[string]string{"codex": codexExecutable, "cursor": cursorExecutable},
			ProviderModelRequests: providerModelRequests,
		}
		if paths.Workspace != nil && !handoffCandidate && !maintenanceActive {
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
				if paths.Workspace != nil {
					if _, authorizeErr := authorizeWorkspaceExecutable(paths.Workspace.Root); authorizeErr != nil {
						return daemon.PreparedInit{}, fmt.Errorf("maintenance generation rejected before Herdr reload: %w", authorizeErr)
					}
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
			snapshotRoot := paths.StateRoot
			if paths.Workspace != nil {
				snapshotRoot = paths.Workspace.Root
			} else {
				snapshotRoot = filepath.Join(paths.StateRoot, "instances", paths.InstanceID)
			}
			var snapshot skillsnapshot.Snapshot
			view, viewErr := engine.Inspect(initCtx, symphony.Status{})
			if viewErr == nil && view.SkillSnapshot != nil {
				// An Init replay must use the published manifest byte-for-byte.
				// resolved_at is part of snapshot_id, so re-resolving sources here
				// would turn an otherwise idempotent request into a conflict.
				input, inputErr := skillsnapshot.InputFromView(*view.SkillSnapshot)
				if inputErr != nil {
					if rollbackErr := rollback(); rollbackErr != nil {
						return daemon.PreparedInit{}, fmt.Errorf("read frozen KDA skills for Init replay: %v; rollback: %w", inputErr, rollbackErr)
					}
					return daemon.PreparedInit{}, fmt.Errorf("read frozen KDA skills for Init replay: %w", inputErr)
				}
				snapshot.Input = input
			} else {
				if viewErr != nil {
					var domainErr *symphony.DomainError
					if !errors.As(viewErr, &domainErr) || domainErr.Code != symphony.CodeNotInitialized {
						if rollbackErr := rollback(); rollbackErr != nil {
							return daemon.PreparedInit{}, fmt.Errorf("inspect Optimization before frozen KDA skills: %v; rollback: %w", viewErr, rollbackErr)
						}
						return daemon.PreparedInit{}, fmt.Errorf("inspect Optimization before frozen KDA skills: %w", viewErr)
					}
				}
				snapshotManager := newSkillSnapshotManager()
				var snapshotErr error
				snapshot, snapshotErr = snapshotManager.Prepare(initCtx, skillsnapshot.Request{WorkspaceRoot: snapshotRoot})
				if snapshotErr != nil {
					if rollbackErr := rollback(); rollbackErr != nil {
						return daemon.PreparedInit{}, fmt.Errorf("prepare frozen KDA skills: %v; rollback: %w", snapshotErr, rollbackErr)
					}
					return daemon.PreparedInit{}, fmt.Errorf("prepare frozen KDA skills: %w", snapshotErr)
				}
			}
			configurationRollback := rollback
			rollback = func() error {
				snapshotErr := snapshot.RollbackPublication()
				configurationErr := configurationRollback()
				if snapshotErr != nil && configurationErr != nil {
					return fmt.Errorf("skill snapshot rollback: %v; configuration rollback: %w", snapshotErr, configurationErr)
				}
				if snapshotErr != nil {
					return snapshotErr
				}
				return configurationErr
			}
			return daemon.PreparedInit{Rollback: rollback, IterationConcurrency: int64(len(iterationAgents)), MaxPendingAttempts: schedulerConfig.MaxPendingAttempts, IterationHistoryLimit: contextConfig.IterationHistoryLimit,
				FlowVersion: symphony.FlowVersion2, SkillSnapshot: &snapshot.Input}, nil
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
			maintenanceHerdrSnapshot = client.Snapshot
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
			if !maintenanceActive {
				if err := herdr.NewRuntime(client).ReportInstance(ctx, pane.WorkspaceID, paths.InstanceID); err != nil {
					writeDaemonLog(stderr, "error", "herdr.instance_report_failed", err, map[string]any{"workspace_id": pane.WorkspaceID})
					return 1
				}
			}
			pathEntries := []string{runtimeBin, os.Getenv("PATH")}
			if prefix := os.Getenv("PIKA_GO_AGENT_PATH_PREFIX"); prefix != "" {
				pathEntries = append([]string{prefix}, pathEntries...)
			}
			activationEnvironment := map[string]string{"PATH": strings.Join(pathEntries, string(os.PathListSeparator))}
			preparer := activation.Preparer{Store: engine, InstructionRoot: paths.InstructionsRoot, ContextsRoot: paths.ContextsRoot, EvidenceRoot: paths.EvidenceRoot, SocketPath: paths.SocketPath, Environment: activationEnvironment, AgentConfigPath: instanceConfigPath, Providers: providerRegistry}
			runtimeSink := workruntime.Sink{Store: engine, Runtime: runtimeAdapter, AgentKind: "codex", AgentConfigPath: instanceConfigPath, Providers: providerRegistry, ProviderRuntimeRoot: runtimeRoot, RequireProviderCapabilities: true, Preparer: preparer, WorkspacePreparer: gitworkspace.RuntimePreparer{Repository: assignedRepository, Root: worktreeRoot, Namespace: branchNamespace, Recorder: engine, CheckpointInitializer: engine}}
			dispatcher := outbox.Dispatcher{Store: engine, Sink: runtimeSink}
			coordinator := &workruntime.Coordinator{
				Reconciler:  workruntime.Reconciler{Store: engine, Runtime: runtimeAdapter, ProviderRuntimeRoot: runtimeRoot},
				Dispatcher:  dispatcher,
				EffectStore: engine,
				Runtime:     runtimeAdapter,
				OnWatchError: func(watchErr error) {
					writeDaemonLog(stderr, "warn", "herdr.watch_reconnecting", watchErr, nil)
				},
			}
			maintenanceCoordinator = coordinator
			if !handoffCandidate && !maintenanceActive {
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
				Store: engine, Dispatcher: dispatcher, Exclusive: coordinator,
				After: func() { afterCommit(ctx) },
			}
			schedulerControl = schedulerController.Apply
			var runtimeStartMu sync.Mutex
			runtimeStarted := false
			startRuntime := func(listenCtx context.Context) error {
				runtimeStartMu.Lock()
				defer runtimeStartMu.Unlock()
				if runtimeStarted {
					return nil
				}
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
							promoted, err := coordinator.Promote(func() (bool, error) {
								return engine.PromoteDueFollowUps(runtimeCtx)
							})
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
				runtimeStarted = true
				return nil
			}
			resumeRuntime = func(resumeCtx context.Context, targets []maintenance.Target) error {
				if resumePreflight != nil {
					if err := resumePreflight(resumeCtx); err != nil {
						return err
					}
				}
				terminal, err := (maintenanceDomain{engine: engine, herdrSnapshot: maintenanceHerdrSnapshot}).MaintenanceTargetsTerminal(resumeCtx, targets)
				if err != nil {
					return err
				}
				if !terminal {
					return errors.New("frozen maintenance targets are not terminal")
				}
				if err := runtimeSink.RetireMaintenanceTargets(resumeCtx, targets); err != nil {
					return err
				}
				if err := startRuntime(resumeCtx); err != nil {
					return err
				}
				return nil
			}
			if handoffCandidate {
				afterHandoffCommit = startRuntime
			} else if !maintenanceActive {
				afterListen = startRuntime
			}
		} else {
			dispatcher := outbox.Dispatcher{Store: engine, Sink: &outbox.Recorder{}}
			coordinator := &workruntime.Coordinator{
				Reconciler:  workruntime.Reconciler{Store: engine},
				Dispatcher:  dispatcher,
				EffectStore: engine,
			}
			maintenanceCoordinator = coordinator
			var runtimeStartMu sync.Mutex
			runtimeStarted := false
			startRuntime := func(listenCtx context.Context) error {
				runtimeStartMu.Lock()
				defer runtimeStartMu.Unlock()
				if runtimeStarted {
					return nil
				}
				if err := coordinator.RecoverEffectsAndDispatch(listenCtx); err != nil {
					return err
				}
				runtimeStarted = true
				return nil
			}
			afterCommit = func(dispatchCtx context.Context) {
				runtimeStartMu.Lock()
				started := runtimeStarted
				runtimeStartMu.Unlock()
				if started {
					_ = coordinator.RecoverEffectsAndDispatch(dispatchCtx)
				}
			}
			resumeSink := workruntime.Sink{Store: engine}
			resumeRuntime = func(resumeCtx context.Context, targets []maintenance.Target) error {
				if resumePreflight != nil {
					if err := resumePreflight(resumeCtx); err != nil {
						return err
					}
				}
				terminal, err := (maintenanceDomain{engine: engine, herdrSnapshot: maintenanceHerdrSnapshot}).MaintenanceTargetsTerminal(resumeCtx, targets)
				if err != nil {
					return err
				}
				if !terminal {
					return errors.New("frozen maintenance targets are not terminal")
				}
				if err := resumeSink.RetireMaintenanceTargets(resumeCtx, targets); err != nil {
					return err
				}
				if err := startRuntime(resumeCtx); err != nil {
					return err
				}
				return nil
			}
			if !maintenanceActive {
				if err := startRuntime(ctx); err != nil {
					writeDaemonLog(stderr, "error", "outbox.recovery_failed", err, nil)
					return 1
				}
			}
		}
		mutationGate := maintenanceMutationGate{arbitration: maintenanceArbitration, store: maintenanceStore, engine: engine}
		daemonMutationGate = mutationGate
		mcpHandler = mcp.Handler{Application: toolapp.Application{Store: engine, WorktreeRoot: worktreeRoot, Repository: assignedRepository, BranchNamespace: branchNamespace, WorktreeRecorder: engine,
			EvidenceRoot: paths.EvidenceRoot, AuthorizeInvocation: mutationGate.AuthorizeInvocation}, AfterMutation: func() {
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
		backup = func(backupCtx context.Context, destination string) error {
			view, inspectErr := engine.Inspect(backupCtx, symphony.Status{})
			if inspectErr == nil && view.Optimization.FlowVersion == symphony.FlowVersion2 {
				if view.SkillSnapshot == nil {
					return errors.New("flow v2 backup has no frozen skill snapshot provenance")
				}
				if verifyErr := skillsnapshot.VerifyView(*view.SkillSnapshot); verifyErr != nil {
					return fmt.Errorf("validate frozen skill snapshot provenance before backup: %w", verifyErr)
				}
			} else if inspectErr != nil {
				var domainErr *symphony.DomainError
				if !errors.As(inspectErr, &domainErr) || domainErr.Code != symphony.CodeNotInitialized {
					return fmt.Errorf("inspect Optimization before backup: %w", inspectErr)
				}
			}
			if err := validateFlowV2Evidence(backupCtx, engine, paths.EvidenceRoot); err != nil {
				return fmt.Errorf("validate durable evidence before backup: %w", err)
			}
			return engine.Backup(backupCtx, destination)
		}
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

	binaryDigest := launchGeneration.Digest
	if paths.Workspace != nil {
		if handoffCandidate {
			_, err = daemonupdate.ReadCurrent(paths.Workspace.Root)
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
	var maintenanceProcess *maintenance.Process
	if paths.Workspace != nil && engine != nil && maintenanceCoordinator != nil {
		maintenanceProcess = &maintenance.Process{
			Store: maintenanceStore, Runtime: maintenanceCoordinator,
			Domain: maintenanceDomain{engine: engine, herdrSnapshot: maintenanceHerdrSnapshot},
			RequireInitialized: func(checkCtx context.Context) error {
				_, inspectErr := engine.Inspect(checkCtx, symphony.Status{})
				return inspectErr
			},
			CurrentGeneration: func() (maintenance.Generation, error) {
				current, currentErr := daemonupdate.ReadCurrent(paths.Workspace.Root)
				if currentErr != nil {
					return maintenance.Generation{}, currentErr
				}
				return maintenance.Generation{Digest: current.Digest, Version: current.Version}, nil
			},
			HotUpdateInProgress: func() bool {
				updateMu.Lock()
				defer updateMu.Unlock()
				return pending != nil || !handoffCommitted.Load()
			},
			ResumeRuntime: resumeRuntime,
			Arbitration:   maintenanceArbitration,
			StateActivation: func(activationCtx context.Context, activate func() error) error {
				lease, leaseErr := optimizationworkspace.AcquireExclusiveMutationLease(activationCtx, paths.Workspace.Root)
				if leaseErr != nil {
					return leaseErr
				}
				defer lease.Close()
				return activate()
			},
		}
		if maintenanceActive {
			status, statusErr := maintenanceStore.Read()
			if statusErr != nil {
				writeDaemonLog(stderr, "error", "maintenance.recovery_failed", statusErr, nil)
				return 1
			}
			current := maintenance.Generation{Digest: binaryDigest, Version: Version}
			if status.State == maintenance.StateQuiescing || (status.State == maintenance.StateReady && status.FromGeneration.Digest == binaryDigest) {
				_, _, statusErr = maintenanceProcess.Recover(ctx)
			} else {
				_, _, statusErr = maintenanceProcess.RecoverWithGeneration(ctx, current)
			}
			if statusErr != nil {
				writeDaemonLog(stderr, "error", "maintenance.recovery_failed", statusErr, nil)
				return 1
			}
		}
	}
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
			maintenanceArbitration.Lock()
			defer maintenanceArbitration.Unlock()
			updateMu.Lock()
			defer updateMu.Unlock()
			if maintenanceStore != nil {
				if status, statusErr := maintenanceStore.Read(); statusErr == nil &&
					(status.State == maintenance.StateQuiescing || status.State == maintenance.StateReady || status.State == maintenance.StateHolding || status.State == maintenance.StateFailed) {
					return daemonupdate.Status{}, fmt.Errorf("maintenance %s is %s", status.ID, status.State)
				} else if statusErr != nil && !errors.Is(statusErr, os.ErrNotExist) {
					return daemonupdate.Status{}, statusErr
				}
			}
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
	var maintenanceService daemon.Maintenance
	if maintenanceProcess != nil {
		maintenanceService = maintenanceProcess
	}
	if startupMutationLease != nil {
		if err := startupMutationLease.Close(); err != nil {
			writeDaemonLog(stderr, "error", "workspace.mutation_lease_release_failed", err, nil)
			return 1
		}
	}
	writeDaemonLog(stderr, "info", "daemon.starting", nil, map[string]any{"instance_id": paths.InstanceID, "protocol_version": protocol.Version, "version": Version})
	serveErr := daemon.Serve(ctx, daemon.Config{SocketPath: paths.SocketPath, Listener: listener, Version: Version, InstanceID: paths.InstanceID, Symphony: symphonyService, PrepareInit: prepareInit, InitOptions: initOptions, RecordInitFailure: recordInitFailure, AfterListen: afterListen, AfterCommit: afterCommit, MCPHandler: mcpHandler, ApplyGitIntent: applyGitIntent, IngestProviderEvent: ingestProviderEvent, IngestProviderHookEvent: ingestProviderHookEvent, Backup: backup, DrainReady: drainReady, SchedulerControl: schedulerControl, PrepareUpdate: prepareUpdate, UpdateAccepted: updateAccepted, BinaryDigest: binaryDigest, Ready: generationReady.Load, Workbench: workbenchService, Maintenance: maintenanceService, MutationGate: daemonMutationGate})
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

func validateFlowV2Evidence(ctx context.Context, engine *symphony.Engine, evidenceRoot string) error {
	view, err := engine.Inspect(ctx, symphony.Status{})
	if err != nil {
		var domainErr *symphony.DomainError
		if errors.As(err, &domainErr) && domainErr.Code == symphony.CodeNotInitialized {
			return nil
		}
		return err
	}
	if view.Optimization.FlowVersion != symphony.FlowVersion2 {
		return nil
	}
	for _, work := range view.Works {
		artifacts, err := engine.EvidenceArtifacts(ctx, work.ID)
		if err != nil {
			return err
		}
		if len(artifacts) == 0 {
			continue
		}
		root := ""
		switch work.Role {
		case symphony.RoleDiagnosis:
			root, err = evidence.EnsureWorkRoot(evidenceRoot, evidence.DiagnosisScope, work.ID)
		case symphony.RoleIteration:
			root, err = evidence.EnsureWorkRoot(evidenceRoot, evidence.IterationScope, work.ID)
		default:
			runtimeWork, err := engine.RuntimeWork(ctx, work.ID)
			if err != nil {
				return fmt.Errorf("resolve evidence Work %s: %w", work.ID, err)
			}
			root = runtimeWork.Repository
		}
		if err != nil {
			return fmt.Errorf("resolve evidence root for Work %s: %w", work.ID, err)
		}
		for _, recorded := range artifacts {
			_, actual, err := evidence.ReadStable(root, recorded.RelativePath, recorded.ByteSize)
			if err != nil {
				return fmt.Errorf("verify evidence artifact %s for Work %s: %w", recorded.ID, work.ID, err)
			}
			if actual.RelativePath != recorded.RelativePath || actual.ByteSize != recorded.ByteSize || actual.ContentSHA256 != recorded.ContentSHA256 || actual.ContractVersion != recorded.ContractVersion {
				return fmt.Errorf("evidence artifact %s for Work %s differs from its durable receipt", recorded.ID, work.ID)
			}
		}
	}
	return nil
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
	instructionWorkspace := ""
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
		instructionWorkspace = workspace.Root
	} else if workspace, err := optimizationworkspace.Discover(""); err == nil {
		instructionRoot = workspace.InstructionsRoot
		instructionWorkspace = workspace.Root
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
	var instructionLease *optimizationworkspace.MutationLease
	if instructionWorkspace != "" {
		var err error
		instructionLease, err = optimizationworkspace.AcquireSharedMutationLease(ctx, instructionWorkspace)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "edit-instruction: acquire Workspace mutation lease: %v\n", err)
			return 1
		}
		defer instructionLease.Close()
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
	if instructionWorkspace != "" {
		if _, _, err := authorizeWorkspaceDirectMutation(instructionWorkspace); err != nil {
			_, _ = fmt.Fprintf(stderr, "edit-instruction: direct Workspace mutation rejected: %v\n", err)
			return 1
		}
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
		_, _ = fmt.Fprintf(stdout, "optimization %s: %s (flow v%d, revision %d), scheduler: %s\n", view.Optimization.ID, view.Optimization.Status, view.Optimization.FlowVersion, view.Optimization.Revision, view.Scheduler.Status)
		if view.SkillSnapshot != nil {
			commits := make([]string, 0, len(view.SkillSnapshot.Entries))
			for _, entry := range view.SkillSnapshot.Entries {
				commits = append(commits, entry.Name+"@"+entry.CommitSHA)
			}
			_, _ = fmt.Fprintf(stdout, "skill snapshot: %s (%s)\n", view.SkillSnapshot.SnapshotID, strings.Join(commits, ", "))
		}
		for _, diagnosis := range view.Diagnoses {
			_, _ = fmt.Fprintf(stdout, "diagnosis %s: %s, %d hypotheses\n", diagnosis.ID, diagnosis.Status, diagnosis.HypothesisCount)
		}
		for _, round := range view.IterationRounds {
			count := 0
			for _, experiment := range view.IterationExperiments {
				if experiment.AttemptID == round.AttemptID && experiment.IterationRound == round.Round {
					count++
				}
			}
			_, _ = fmt.Fprintf(stdout, "round %s/%d: %d experiments, checkpoint %s\n", round.AttemptID, round.Round, count, round.CurrentCheckpointSHA)
		}
		_, _ = fmt.Fprintf(stdout, "knowledge: verified=%d provisional=%d negative=%d inconclusive=%d\n", view.Knowledge.Verified, view.Knowledge.Provisional, view.Knowledge.Negative, view.Knowledge.Inconclusive)
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
