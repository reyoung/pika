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
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/reyoung/pika-go/internal/activation"
	"github.com/reyoung/pika-go/internal/configuration"
	"github.com/reyoung/pika-go/internal/control"
	"github.com/reyoung/pika-go/internal/daemon"
	"github.com/reyoung/pika-go/internal/gitworkspace"
	"github.com/reyoung/pika-go/internal/herdr"
	"github.com/reyoung/pika-go/internal/instance"
	"github.com/reyoung/pika-go/internal/instructions"
	"github.com/reyoung/pika-go/internal/mcp"
	"github.com/reyoung/pika-go/internal/outbox"
	"github.com/reyoung/pika-go/internal/protocol"
	"github.com/reyoung/pika-go/internal/provider"
	"github.com/reyoung/pika-go/internal/symphony"
	"github.com/reyoung/pika-go/internal/toolapp"
	"github.com/reyoung/pika-go/internal/workruntime"
)

var Version = "dev"

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
	case "daemon":
		return runDaemon(ctx, args[1:], stderr)
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
	case "help", "--help", "-h":
		printUsage(stdout)
		return 0
	default:
		_, _ = fmt.Fprintf(stderr, "unknown command %q\n", args[0])
		printUsage(stderr)
		return 2
	}
}

func runDaemon(ctx context.Context, args []string, stderr io.Writer) int {
	flags := flag.NewFlagSet("daemon", flag.ContinueOnError)
	flags.SetOutput(stderr)
	socketPath := flags.String("socket", "", "Unix socket path")
	stateDir := flags.String("state-dir", "", "Plugin state directory")
	configDir := flags.String("config-dir", "", "Plugin configuration directory")
	instanceID := flags.String("instance", "", "Pika instance ID")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	paths, err := instance.ResolveRuntime(instance.RuntimeOptions{
		SocketPath: *socketPath,
		ConfigRoot: *configDir,
		StateRoot:  *stateDir,
		InstanceID: *instanceID,
	})
	if err != nil {
		writeDaemonLog(stderr, "error", "runtime.resolve_failed", err, nil)
		return 2
	}
	var engine *symphony.Engine
	var symphonyService symphony.Symphony
	var prepareInit func(context.Context, string, *string) (daemon.PreparedInit, error)
	var initOptions func(context.Context) (protocol.InitOptionsResponse, error)
	var recordInitFailure func(context.Context, string) error
	var afterListen func(context.Context) error
	var afterCommit func(context.Context)
	var mcpHandler http.Handler
	var applyGitIntent func(context.Context, string, string) (string, error)
	if paths.StateRoot != "" {
		worktreeRoot := filepath.Join(paths.StateRoot, "instances", paths.InstanceID, "worktrees")
		runtimeRoot := filepath.Join(paths.StateRoot, "instances", paths.InstanceID, "runtime")
		runtimeBin := filepath.Join(runtimeRoot, "bin")
		instanceConfigPath := filepath.Join(paths.ConfigRoot, "instances", paths.InstanceID, "config.toml")
		pikaExecutable, _ := os.Executable()
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
		if _, statErr := os.Stat(instanceConfigPath); statErr == nil {
			followUpConfig, configErr := configuration.LoadFollowUp(instanceConfigPath)
			if configErr != nil {
				writeDaemonLog(stderr, "error", "configuration.load_failed", configErr, nil)
				return 1
			}
			followUpInactivity = followUpConfig.PaneIdleTimeout
			followUpPolicies = symphonyFollowUpPolicies(followUpConfig)
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
		lock, err := instance.AcquireLock(paths.LockPath)
		if err != nil {
			writeDaemonLog(stderr, "error", "instance.lock_failed", err, map[string]any{"instance_id": paths.InstanceID})
			return 1
		}
		defer lock.Close()
		engine, err = symphony.Open(ctx, paths.DatabasePath, symphony.Options{FollowUpInactivity: followUpInactivity, FollowUpPolicies: followUpPolicies, Providers: providerRegistry})
		if err != nil {
			writeDaemonLog(stderr, "error", "database.open_failed", err, map[string]any{"instance_id": paths.InstanceID})
			return 1
		}
		defer engine.Close()
		activeSessions, activeErr := engine.ActiveAgentSessions(ctx)
		if activeErr != nil {
			writeDaemonLog(stderr, "error", "provider.reconciliation_failed", activeErr, nil)
			return 1
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
			ConfigRoot: paths.ConfigRoot, StateRoot: paths.StateRoot, InstanceID: paths.InstanceID,
			CodexHome: codexHome, CursorMCPPath: cursorMCPPath, CursorHooksPath: cursorHooksPath, PikaExecutable: pikaExecutable, CodexExecutable: codexExecutable, Providers: providerRegistry,
			RequireConfigurationTOML: true, ProbeProviders: true,
			ProviderExecutables: map[string]string{"codex": codexExecutable, "cursor": cursorExecutable},
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
			var followUpConfig configuration.FollowUp
			if configErr == nil {
				followUpConfig, configErr = configuration.LoadFollowUp(instanceConfigPath)
			}
			for _, role := range []string{"baseline", "baseline_verify", "iteration", "integration", "follow_up"} {
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
			return daemon.PreparedInit{Rollback: rollback, IterationConcurrency: schedulerConfig.IterationConcurrency, MaxPendingAttempts: schedulerConfig.MaxPendingAttempts}, nil
		}
		initOptions = func(optionsCtx context.Context) (protocol.InitOptionsResponse, error) {
			_, statErr := os.Stat(instanceConfigPath)
			response := protocol.InitOptionsResponse{ConfigurationExists: statErr == nil}
			if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
				return protocol.InitOptionsResponse{}, fmt.Errorf("inspect instance configuration: %w", statErr)
			}
			for _, kind := range providerRegistry.Kinds() {
				option := protocol.ProviderOption{Kind: kind}
				probeCtx, cancelProbe := context.WithTimeout(optionsCtx, 500*time.Millisecond)
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
					"full_output": capabilities.FullOutput, "fresh_session": capabilities.FreshSession,
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
				return engine.ObservePaneActivity(eventCtx, payload.Pane.PaneID)
			}
			runtimeAdapter.LaunchDir = filepath.Join(paths.StateRoot, "instances", paths.InstanceID, "runtime")
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
			if os.Getenv("PIKA_CODEX_BYPASS_HOOK_TRUST") == "1" {
				// Explicit automation escape hatch. Normal interactive Pika sessions
				// still require the user to review new or changed Codex hooks.
				activationEnvironment["PIKA_CODEX_BYPASS_HOOK_TRUST"] = "1"
			}
			preparer := activation.Preparer{Store: engine, InstructionRoot: filepath.Join(paths.ConfigRoot, "instances", paths.InstanceID, "instructions"), SocketPath: paths.SocketPath, Environment: activationEnvironment, AgentConfigPath: instanceConfigPath, Providers: providerRegistry}
			dispatcher := outbox.Dispatcher{Store: engine, Sink: workruntime.Sink{Store: engine, Runtime: runtimeAdapter, AgentKind: "codex", AgentConfigPath: instanceConfigPath, Providers: providerRegistry, ProviderRuntimeRoot: runtimeRoot, RequireProviderCapabilities: true, Preparer: preparer, WorkspacePreparer: gitworkspace.RuntimePreparer{Root: worktreeRoot}}}
			coordinator := workruntime.Coordinator{
				Reconciler:  workruntime.Reconciler{Store: engine, Runtime: runtimeAdapter, ProviderRuntimeRoot: runtimeRoot},
				Dispatcher:  dispatcher,
				EffectStore: engine,
				Runtime:     runtimeAdapter,
				OnWatchError: func(watchErr error) {
					writeDaemonLog(stderr, "warn", "herdr.watch_reconnecting", watchErr, nil)
				},
			}
			if err := engine.RetireActiveSessionsForRecovery(ctx); err != nil {
				writeDaemonLog(stderr, "error", "runtime.session_retirement_failed", err, nil)
				return 1
			}
			dispatchRequested := make(chan struct{}, 1)
			afterCommit = func(context.Context) {
				select {
				case dispatchRequested <- struct{}{}:
				default:
				}
			}
			afterListen = func(listenCtx context.Context) error {
				if err := coordinator.RecoverAndDispatch(listenCtx); err != nil {
					return fmt.Errorf("recover runtime after daemon listen: %w", err)
				}
				go func() {
					for {
						select {
						case <-ctx.Done():
							return
						case <-dispatchRequested:
							if err := coordinator.RecoverAndDispatch(ctx); err != nil {
								writeDaemonLog(stderr, "error", "runtime.dispatch_failed", err, nil)
							}
						}
					}
				}()
				go func() {
					ticker := time.NewTicker(time.Second)
					defer ticker.Stop()
					for {
						select {
						case <-ctx.Done():
							return
						case <-ticker.C:
							promoted, err := engine.PromoteDueFollowUps(ctx)
							if err != nil {
								writeDaemonLog(stderr, "error", "follow_up.promotion_failed", err, nil)
							} else if promoted {
								afterCommit(ctx)
							}
						}
					}
				}()
				go func() { _ = coordinator.Run(ctx) }()
				return nil
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
		mcpHandler = mcp.Handler{Application: toolapp.Application{Store: engine, WorktreeRoot: worktreeRoot}, AfterMutation: func() {
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
			workspace := gitworkspace.Workspace{Repository: view.Optimization.Repository, Root: worktreeRoot}
			return workspace.ApplyAuthorizedBestUpdate(ctx, intent.ID, intent.ExpectedBestSHA, intent.CandidateSHA, message)
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
	writeDaemonLog(stderr, "info", "daemon.starting", nil, map[string]any{"instance_id": paths.InstanceID, "protocol_version": protocol.Version, "version": Version})
	if err := daemon.Serve(ctx, daemon.Config{SocketPath: paths.SocketPath, Version: Version, InstanceID: paths.InstanceID, Symphony: symphonyService, PrepareInit: prepareInit, InitOptions: initOptions, RecordInitFailure: recordInitFailure, AfterListen: afterListen, AfterCommit: afterCommit, MCPHandler: mcpHandler, ApplyGitIntent: applyGitIntent, IngestProviderEvent: ingestProviderEvent, IngestProviderHookEvent: ingestProviderHookEvent, Backup: backup, DrainReady: drainReady}); err != nil {
		writeDaemonLog(stderr, "error", "daemon.stopped", err, map[string]any{"instance_id": paths.InstanceID})
		return 1
	}
	writeDaemonLog(stderr, "info", "daemon.stopped", nil, map[string]any{"instance_id": paths.InstanceID})
	return 0
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
	flags := flag.NewFlagSet("apply-best-update", flag.ContinueOnError)
	flags.SetOutput(stderr)
	socketPath := flags.String("socket", "", "Unix socket path")
	message := flags.String("m", "", "Best commit message")
	if err := flags.Parse(args); err != nil {
		return 2
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
	flags := flag.NewFlagSet("mcp-proxy", flag.ContinueOnError)
	flags.SetOutput(stderr)
	socketPath := flags.String("socket", "", "Unix socket path")
	if err := flags.Parse(args); err != nil {
		return 2
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
	flags := flag.NewFlagSet("edit-instruction", flag.ContinueOnError)
	flags.SetOutput(stderr)
	configRoot := flags.String("config-dir", "", "Plugin configuration directory")
	instanceID := flags.String("instance", "", "Pika instance ID")
	socketPath := flags.String("socket", "", "Unix socket path used to derive the instance")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 1 {
		_, _ = fmt.Fprintln(stderr, "edit-instruction: exactly one instruction name is required")
		return 2
	}
	root := *configRoot
	if root == "" {
		root = os.Getenv("HERDR_PLUGIN_CONFIG_DIR")
	}
	if root == "" || !filepath.IsAbs(root) {
		_, _ = fmt.Fprintln(stderr, "edit-instruction: an absolute plugin config directory is required")
		return 2
	}
	id := *instanceID
	if id == "" {
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
	if err := instance.ValidateID(id); err != nil {
		_, _ = fmt.Fprintf(stderr, "edit-instruction: %v\n", err)
		return 2
	}
	path, err := instructions.Path(filepath.Join(root, "instances", id, "instructions"), flags.Arg(0))
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
	flags := flag.NewFlagSet("status", flag.ContinueOnError)
	flags.SetOutput(stderr)
	socketPath := flags.String("socket", "", "Unix socket path")
	jsonOutput := flags.Bool("json", false, "Print JSON")
	if err := flags.Parse(args); err != nil {
		return 2
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
		_, _ = fmt.Fprintf(stdout, "optimization %s: %s (revision %d)\n", view.Optimization.ID, view.Optimization.Status, view.Optimization.Revision)
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
	flags := flag.NewFlagSet("init", flag.ContinueOnError)
	flags.SetOutput(stderr)
	socketPath := flags.String("socket", "", "Unix socket path")
	repository := flags.String("repository", "", "Absolute repository path")
	requestID := flags.String("request-id", "", "Idempotency request ID")
	jsonOutput := flags.Bool("json", false, "Print JSON only")
	defaults := flags.Bool("defaults", false, "Use the all-Codex default configuration")
	configPath := flags.String("config", "", "Read the complete instance TOML from PATH")
	if err := flags.Parse(args); err != nil {
		return 2
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
		CallerPaneID:      os.Getenv("HERDR_PANE_ID"),
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
	available := make(map[string]bool, len(options.Providers))
	for _, option := range options.Providers {
		available[option.Kind] = true
		state := "available"
		if option.Error != "" {
			state = option.Error
		}
		_, _ = fmt.Fprintf(output, "Provider %s: %s %s (%s)\n", option.Kind, option.Executable, option.Version, state)
	}
	agents := configuration.DefaultAgents()
	registry := provider.DefaultRegistry()
	for _, role := range configuration.AgentRoleOrder {
		defaults := agents[role]
		_, _ = fmt.Fprintf(output, "\nConfigure agents.%s\n", role)
		kind, err := promptValue(reader, output, "  kind", defaults.Kind)
		if err != nil {
			return "", err
		}
		if !available[kind] {
			return "", fmt.Errorf("provider %q was not returned by the daemon", kind)
		}
		model, err := promptValue(reader, output, "  model", defaults.Model)
		if err != nil {
			return "", err
		}
		effort, err := promptValue(reader, output, "  reasoning effort", defaults.ReasoningEffort)
		if err != nil {
			return "", err
		}
		agent := configuration.Agent{Kind: kind, Model: model, ReasoningEffort: effort}
		if kind == "cursor" {
			defaultArgs := []string{"--force", "--approve-mcps"}
			encodedDefaults, _ := json.Marshal(defaultArgs)
			encoded, promptErr := promptValue(reader, output, "  Cursor args JSON", string(encodedDefaults))
			if promptErr != nil {
				return "", promptErr
			}
			if err := json.Unmarshal([]byte(encoded), &agent.Args); err != nil {
				return "", fmt.Errorf("agents.%s Cursor args must be a JSON string array: %w", role, err)
			}
			trust, promptErr := promptValue(reader, output, "  append --trust (changes Cursor persistent workspace trust) y/N", "n")
			if promptErr != nil {
				return "", promptErr
			}
			if strings.EqualFold(trust, "y") || strings.EqualFold(trust, "yes") {
				agent.Args = append(agent.Args, "--trust")
			} else if !strings.EqualFold(trust, "n") && !strings.EqualFold(trust, "no") {
				return "", fmt.Errorf("agents.%s trust answer must be yes or no", role)
			}
		}
		adapter, err := registry.Resolve(kind)
		if err != nil {
			return "", err
		}
		if err := adapter.Validate(agent); err != nil {
			return "", fmt.Errorf("agents.%s: %w", role, err)
		}
		agents[role] = agent
	}
	return configuration.RenderConfiguration(repository, agents)
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
	flags := flag.NewFlagSet("draft-baseline", flag.ContinueOnError)
	flags.SetOutput(stderr)
	socketPath, requestID, expectedRevision := mutationFlags(flags)
	if err := flags.Parse(args); err != nil {
		return 2
	}
	resolvedSocket, mutation, code := resolveMutation(ctx, *socketPath, *requestID, *expectedRevision, stderr)
	if code != 0 {
		return code
	}
	receipt, err := control.DraftBaseline(ctx, resolvedSocket, protocol.DraftBaselineRequest{Mutation: mutation})
	return writeMutationResult("draft-baseline", receipt, err, stdout, stderr)
}

func runBackOff(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("back-off", flag.ContinueOnError)
	flags.SetOutput(stderr)
	socketPath, requestID, expectedRevision := mutationFlags(flags)
	message := flags.String("m", "", "Back-off message")
	if err := flags.Parse(args); err != nil {
		return 2
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
	flags := flag.NewFlagSet("cancel-work", flag.ContinueOnError)
	flags.SetOutput(stderr)
	socketPath, requestID, expectedRevision := mutationFlags(flags)
	if err := flags.Parse(args); err != nil {
		return 2
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
	flags := flag.NewFlagSet("shutdown", flag.ContinueOnError)
	flags.SetOutput(stderr)
	socketPath, requestID, expectedRevision := mutationFlags(flags)
	if err := flags.Parse(args); err != nil {
		return 2
	}
	resolvedSocket, mutation, code := resolveMutation(ctx, *socketPath, *requestID, *expectedRevision, stderr)
	if code != 0 {
		return code
	}
	receipt, err := control.Shutdown(ctx, resolvedSocket, protocol.ShutdownRequest{Mutation: mutation})
	return writeMutationResult("shutdown", receipt, err, stdout, stderr)
}

func runBackup(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("backup", flag.ContinueOnError)
	flags.SetOutput(stderr)
	socketPath := flags.String("socket", "", "Unix socket path")
	destination := flags.String("output", "", "Absolute destination path for the SQLite snapshot")
	if err := flags.Parse(args); err != nil {
		return 2
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

func printUsage(w io.Writer) {
	_, _ = fmt.Fprintln(w, "usage: pika-go <daemon|init|status|draft-baseline|back-off|cancel-work|shutdown|backup|edit-instruction|mcp-proxy|version> [options]")
}

func mutationFlags(flags *flag.FlagSet) (socketPath, requestID *string, expectedRevision *int64) {
	return flags.String("socket", "", "Unix socket path"),
		flags.String("request-id", "", "Idempotency request ID"),
		flags.Int64("expected-revision", -1, "Expected domain revision")
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
