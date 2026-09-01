package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/reyoung/pika-go/internal/control"
	"github.com/reyoung/pika-go/internal/herdr"
	"github.com/reyoung/pika-go/internal/instance"
	"github.com/reyoung/pika-go/internal/maintenance"
	"github.com/reyoung/pika-go/internal/optimizationworkspace"
	"github.com/reyoung/pika-go/internal/protocol"
	"github.com/reyoung/pika-go/internal/symphony"
)

func runMaintenance(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		_, _ = fmt.Fprintln(stderr, "maintenance: prepare, status, or resume is required")
		return 2
	}
	action := args[0]
	flags := newCommandFlagSet("maintenance", stderr)
	socketPath := flags.String("socket", "", "Unix socket `PATH`")
	workspaceRoot := flags.String("workspace", "", "Optimization Workspace `PATH`")
	requestID := flags.String("request-id", "", "Idempotency request `ID`")
	toDigest := flags.String("to-digest", "", "Expected cold target generation SHA-256 `DIGEST`")
	toVersion := flags.String("to-version", "", "Expected cold target version `VERSION`")
	jsonOutput := flags.Bool("json", false, "Print JSON only")
	if ok, code := parseCommandFlags(flags, args[1:]); !ok {
		return code
	}
	if flags.NArg() != 0 {
		_, _ = fmt.Fprintln(stderr, "maintenance: positional arguments are not supported")
		return 2
	}
	if *socketPath != "" && *workspaceRoot != "" {
		_, _ = fmt.Fprintln(stderr, "maintenance: --socket and --workspace cannot be used together")
		return 2
	}
	if action == "status" {
		if *requestID != "" || *toDigest != "" || *toVersion != "" {
			_, _ = fmt.Fprintln(stderr, "maintenance status: request and target options are not supported")
			return 2
		}
		status, err := readMaintenanceStatus(ctx, *socketPath, *workspaceRoot)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "maintenance status: %v\n", err)
			return 1
		}
		return writeMaintenanceStatus(status, *jsonOutput, stdout, stderr)
	}
	if action != "prepare" && action != "resume" {
		_, _ = fmt.Fprintf(stderr, "maintenance: unknown action %q\n", action)
		return 2
	}
	if *workspaceRoot != "" {
		workspace, err := optimizationworkspace.Open(*workspaceRoot)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "maintenance %s: %v\n", action, err)
			return 2
		}
		binding, err := workspace.ReadHerdrBinding()
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "maintenance %s: %v\n", action, err)
			return 1
		}
		*socketPath = binding.DaemonSocket
	}
	resolvedSocket, err := instance.ResolveSocketContext(ctx, *socketPath)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "maintenance %s: %v\n", action, err)
		return 2
	}
	if *requestID == "" {
		*requestID = newRequestID()
	}
	var status maintenance.Status
	switch action {
	case "prepare":
		if *toDigest == "" {
			_, _ = fmt.Fprintln(stderr, "maintenance prepare: --to-digest is required")
			return 2
		}
		if err := maintenance.ValidateDigest(*toDigest); err != nil {
			_, _ = fmt.Fprintf(stderr, "maintenance prepare: %v\n", err)
			return 2
		}
		status, err = control.MaintenancePrepare(ctx, resolvedSocket, protocol.MaintenancePrepareRequest{
			RequestID: *requestID, ToGeneration: protocol.MaintenanceGeneration{Digest: *toDigest, Version: *toVersion},
		})
	case "resume":
		if *toDigest != "" || *toVersion != "" {
			_, _ = fmt.Fprintln(stderr, "maintenance resume: target options are not supported")
			return 2
		}
		status, err = control.MaintenanceResume(ctx, resolvedSocket, protocol.MaintenanceResumeRequest{RequestID: *requestID})
	}
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "maintenance %s: %v\n", action, err)
		return 1
	}
	return writeMaintenanceStatus(status, *jsonOutput, stdout, stderr)
}

type maintenanceDomain struct {
	engine        *symphony.Engine
	herdrSnapshot func(context.Context) (herdr.Snapshot, error)
}

var errLiveAgentNotReady = errors.New("live Herdr Agent identity is not fully observable yet")

func (d maintenanceDomain) ActiveMaintenanceTargets(ctx context.Context) ([]maintenance.Target, error) {
	sessions, err := d.engine.ActiveAgentSessions(ctx)
	if err != nil {
		return nil, err
	}
	targets := make([]maintenance.Target, 0, len(sessions))
	for _, session := range sessions {
		if session.Session.AgentName == "" || session.Session.AgentKind == "" {
			return nil, fmt.Errorf("active Session %s has incomplete Agent identity", session.Session.ID)
		}
		if session.Binding.WorkspaceID == "" || session.Binding.TabID == "" ||
			session.Binding.PaneID == "" || session.Binding.TerminalID == "" {
			return nil, fmt.Errorf("active Session %s has no complete durable Herdr pane binding", session.Session.ID)
		}
		targets = append(targets, maintenance.Target{
			WorkID: session.Session.WorkID, SessionID: session.Session.ID, WorkGeneration: session.Session.Generation,
			AgentName: session.Session.AgentName, AgentKind: session.Session.AgentKind,
			WorkspaceID: session.Binding.WorkspaceID, TabID: session.Binding.TabID,
			PaneID: session.Binding.PaneID, TerminalID: session.Binding.TerminalID,
		})
	}
	if err := d.validateLiveAgentSet(ctx, sessions, targets); err != nil {
		return nil, err
	}
	return targets, nil
}

func (d maintenanceDomain) MaintenanceTargetsTerminal(ctx context.Context, targets []maintenance.Target) (bool, error) {
	if err := d.ValidateMaintenanceTargets(ctx, targets); err != nil {
		return false, err
	}
	for _, target := range targets {
		work, err := d.engine.RuntimeWork(ctx, target.WorkID)
		if err != nil {
			return false, err
		}
		if work.Work.Generation != target.WorkGeneration {
			return false, fmt.Errorf("frozen Work %s generation changed from %d to %d", target.WorkID, target.WorkGeneration, work.Work.Generation)
		}
		if work.Work.Status == symphony.WorkPending {
			return false, nil
		}
		if work.Work.Status != symphony.WorkCompleted {
			return false, fmt.Errorf("frozen Work %s ended as %s instead of completed", target.WorkID, work.Work.Status)
		}
		history, err := d.engine.AgentSessionHistory(ctx, target.WorkID)
		if err != nil {
			return false, err
		}
		found := false
		for _, session := range history {
			if session.Generation != target.WorkGeneration {
				continue
			}
			if session.ID != target.SessionID {
				return false, fmt.Errorf("frozen Work %s generation %d has replacement Session %s", target.WorkID, target.WorkGeneration, session.ID)
			}
			if session.WorkID != target.WorkID || session.Status == symphony.AgentSessionLost {
				return false, fmt.Errorf("frozen Session %s identity is inconsistent", target.SessionID)
			}
			found = true
		}
		if !found {
			return false, fmt.Errorf("frozen Session %s was not found for Work %s generation %d", target.SessionID, target.WorkID, target.WorkGeneration)
		}
		if _, found, err := d.engine.WorkRoleTerminalReceipt(ctx, target.WorkID, work.Work.Role); err != nil {
			return false, err
		} else if !found {
			return false, fmt.Errorf("frozen Work %s completed without its %s terminal receipt", target.WorkID, work.Work.Role)
		}
	}
	return true, nil
}

func (d maintenanceDomain) ValidateMaintenanceTargets(ctx context.Context, targets []maintenance.Target) error {
	var snapshot herdr.Snapshot
	if d.herdrSnapshot != nil {
		var err error
		snapshot, err = d.herdrSnapshot(ctx)
		if err != nil {
			return fmt.Errorf("read Herdr snapshot for frozen targets: %w", err)
		}
	}
	for _, target := range targets {
		session, err := d.engine.ReadAgentSession(ctx, target.SessionID)
		if err != nil {
			return err
		}
		binding, found, err := d.engine.AgentSessionBinding(ctx, target.SessionID)
		if err != nil {
			return err
		}
		if target.AgentName == "" || target.AgentKind == "" {
			return fmt.Errorf("frozen Session %s has incomplete persisted Agent identity", target.SessionID)
		}
		if session.ID != target.SessionID || session.WorkID != target.WorkID ||
			session.Generation != target.WorkGeneration || session.AgentName != target.AgentName || session.AgentKind != target.AgentKind {
			return fmt.Errorf("frozen Session %s Agent identity drifted", target.SessionID)
		}
		persistedBinding := target.WorkspaceID != "" || target.TabID != "" || target.PaneID != "" || target.TerminalID != ""
		completeBinding := target.WorkspaceID != "" && target.TabID != "" && target.PaneID != "" && target.TerminalID != ""
		if persistedBinding != completeBinding || (d.herdrSnapshot != nil && !completeBinding) {
			return fmt.Errorf("frozen Session %s has incomplete persisted Herdr identity", target.SessionID)
		}
		if found != completeBinding {
			return fmt.Errorf("frozen Session %s Herdr pane binding presence drifted", target.SessionID)
		}
		if completeBinding && (binding.WorkspaceID != target.WorkspaceID || binding.TabID != target.TabID ||
			binding.PaneID != target.PaneID || binding.TerminalID != target.TerminalID) {
			return fmt.Errorf("frozen Session %s Herdr Agent identity drifted", target.SessionID)
		}
		if d.herdrSnapshot != nil && session.Status != symphony.AgentSessionExited {
			found := false
			for _, agent := range snapshot.Agents {
				if agent.Name != nil && agent.Agent != nil &&
					*agent.Name == target.AgentName && *agent.Agent == target.AgentKind &&
					agent.WorkspaceID == target.WorkspaceID && agent.TabID == target.TabID &&
					agent.PaneID == target.PaneID && agent.TerminalID == target.TerminalID {
					found = true
					break
				}
			}
			if !found {
				return fmt.Errorf("frozen Session %s is missing or drifted in the live Herdr snapshot", target.SessionID)
			}
		}
	}
	return nil
}

func (d maintenanceDomain) ValidateMaintenanceRecovery(ctx context.Context, status maintenance.Status) error {
	active, err := d.engine.ActiveAgentSessions(ctx)
	if err != nil {
		return err
	}
	if status.ResumeRequestID == "" {
		if len(active) != len(status.Targets) {
			return fmt.Errorf("durable active Session count %d does not match %d frozen maintenance Targets", len(active), len(status.Targets))
		}
		activeByID := make(map[string]symphony.ActiveAgentSession, len(active))
		for _, record := range active {
			activeByID[record.Session.ID] = record
		}
		for _, target := range status.Targets {
			record, found := activeByID[target.SessionID]
			if !found || record.Session.WorkID != target.WorkID || record.Session.Generation != target.WorkGeneration {
				return fmt.Errorf("frozen Session %s is not the exact durable active Session", target.SessionID)
			}
		}
		if err := d.ValidateMaintenanceTargets(ctx, status.Targets); err != nil {
			return err
		}
		return d.validateLiveAgentSet(ctx, active, status.Targets)
	}
	var lastErr error
	deadline := time.Now().Add(3 * time.Second)
	for {
		if err := d.validateLiveAgentSet(ctx, active, status.Targets); err == nil {
			return nil
		} else {
			lastErr = err
			if !errors.Is(err, errLiveAgentNotReady) {
				return err
			}
		}
		if time.Now().After(deadline) {
			return lastErr
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func (d maintenanceDomain) validateLiveAgentSet(
	ctx context.Context,
	active []symphony.ActiveAgentSession,
	scope []maintenance.Target,
) error {
	if len(active) > 0 && d.herdrSnapshot == nil {
		return errors.New("live Herdr observation is required for active Agent Sessions")
	}
	if d.herdrSnapshot == nil {
		return nil
	}
	snapshot, err := d.herdrSnapshot(ctx)
	if err != nil {
		return fmt.Errorf("read Herdr snapshot for active Agent Sessions: %w", err)
	}
	return validateLiveAgentSnapshot(snapshot, active, scope)
}

func validateLiveAgentSnapshot(
	snapshot herdr.Snapshot,
	active []symphony.ActiveAgentSession,
	scope []maintenance.Target,
) error {
	scopeWorkspaces := make(map[string]bool)
	for _, target := range scope {
		if target.WorkspaceID != "" {
			scopeWorkspaces[target.WorkspaceID] = true
		}
	}
	expected := make(map[string]symphony.ActiveAgentSession, len(active))
	for _, record := range active {
		if record.Session.AgentName == "" || record.Session.AgentKind == "" ||
			record.Binding.WorkspaceID == "" || record.Binding.TabID == "" ||
			record.Binding.PaneID == "" || record.Binding.TerminalID == "" {
			return fmt.Errorf("active Session %s has incomplete durable Agent binding", record.Session.ID)
		}
		if len(scopeWorkspaces) > 0 && !scopeWorkspaces[record.Binding.WorkspaceID] {
			return fmt.Errorf("active Session %s moved outside the frozen Herdr Workspace", record.Session.ID)
		}
		if previous, duplicate := expected[record.Session.AgentName]; duplicate {
			return fmt.Errorf(
				"durable active Sessions %s and %s share Agent name %s",
				previous.Session.ID, record.Session.ID, record.Session.AgentName,
			)
		}
		expected[record.Session.AgentName] = record
	}
	observed := make(map[string]herdr.Agent)
	for _, agent := range snapshot.Agents {
		if agent.Name == nil || !scopeWorkspaces[agent.WorkspaceID] {
			continue
		}
		if _, expectedName := expected[*agent.Name]; !expectedName && !strings.HasPrefix(*agent.Name, "pika-") {
			continue
		}
		if _, duplicate := observed[*agent.Name]; duplicate {
			return fmt.Errorf("live Herdr has duplicate Pika Agent name %s", *agent.Name)
		}
		observed[*agent.Name] = agent
	}
	if len(observed) != len(expected) {
		return fmt.Errorf("live Pika Agent count %d does not match durable active Session count %d", len(observed), len(expected))
	}
	for name, record := range expected {
		agent, found := observed[name]
		if found && agent.Agent == nil &&
			agent.WorkspaceID == record.Binding.WorkspaceID && agent.TabID == record.Binding.TabID &&
			agent.PaneID == record.Binding.PaneID && agent.TerminalID == record.Binding.TerminalID {
			return fmt.Errorf("%w: Session %s", errLiveAgentNotReady, record.Session.ID)
		}
		if !found || agent.Agent == nil || *agent.Agent != record.Session.AgentKind ||
			agent.WorkspaceID != record.Binding.WorkspaceID || agent.TabID != record.Binding.TabID ||
			agent.PaneID != record.Binding.PaneID || agent.TerminalID != record.Binding.TerminalID {
			return fmt.Errorf(
				"active Session %s live Herdr Agent identity is missing or drifted: expected name=%s kind=%s workspace=%s tab=%s pane=%s terminal=%s observed=%+v",
				record.Session.ID, record.Session.AgentName, record.Session.AgentKind,
				record.Binding.WorkspaceID, record.Binding.TabID, record.Binding.PaneID, record.Binding.TerminalID,
				agent,
			)
		}
	}
	return nil
}

func validateMaintenanceActiveSet(
	status maintenance.Status,
	active []symphony.ActiveAgentSession,
	snapshot herdr.Snapshot,
) error {
	if status.ResumeRequestID == "" {
		if len(active) != len(status.Targets) {
			return fmt.Errorf("durable active Session count %d does not match %d frozen maintenance Targets", len(active), len(status.Targets))
		}
		activeByID := make(map[string]symphony.ActiveAgentSession, len(active))
		for _, record := range active {
			activeByID[record.Session.ID] = record
		}
		for _, target := range status.Targets {
			record, found := activeByID[target.SessionID]
			if !found || record.Session.WorkID != target.WorkID || record.Session.Generation != target.WorkGeneration ||
				record.Session.AgentName != target.AgentName || record.Session.AgentKind != target.AgentKind ||
				record.Binding.WorkspaceID != target.WorkspaceID || record.Binding.TabID != target.TabID ||
				record.Binding.PaneID != target.PaneID || record.Binding.TerminalID != target.TerminalID {
				return fmt.Errorf("frozen Session %s is not the exact durable active Agent identity", target.SessionID)
			}
		}
	}
	return validateLiveAgentSnapshot(snapshot, active, status.Targets)
}

func readMaintenanceStatus(ctx context.Context, socketPath, workspaceRoot string) (maintenance.Status, error) {
	if workspaceRoot != "" {
		workspace, err := optimizationworkspace.Open(workspaceRoot)
		if err != nil {
			return maintenance.Status{}, err
		}
		return maintenance.NewFileStore(workspace.Root).Read()
	}
	if socketPath != "" {
		return control.MaintenanceStatus(ctx, socketPath)
	}
	resolvedSocket, err := instance.ResolveSocketContext(ctx, "")
	if err == nil {
		status, requestErr := control.MaintenanceStatus(ctx, resolvedSocket)
		if requestErr == nil {
			return status, nil
		}
		if !control.IsHTTPStatus(requestErr, 404) {
			return maintenance.Status{}, requestErr
		}
	}
	workspace, discoverErr := optimizationworkspace.Discover("")
	if discoverErr != nil {
		if err != nil {
			return maintenance.Status{}, err
		}
		return maintenance.Status{}, discoverErr
	}
	return maintenance.NewFileStore(workspace.Root).Read()
}

func writeMaintenanceStatus(status maintenance.Status, jsonOutput bool, stdout, stderr io.Writer) int {
	if jsonOutput {
		if err := json.NewEncoder(stdout).Encode(status); err != nil {
			_, _ = fmt.Fprintf(stderr, "maintenance: encode status: %v\n", err)
			return 1
		}
		return 0
	}
	_, _ = fmt.Fprintf(stdout, "maintenance %s: %s\n", status.ID, status.State)
	_, _ = fmt.Fprintf(stdout, "  request: %s\n", status.RequestID)
	_, _ = fmt.Fprintf(stdout, "  from: %s (%s)\n", status.FromGeneration.Version, shortDigest(status.FromGeneration.Digest))
	_, _ = fmt.Fprintf(stdout, "  to:   %s (%s)\n", status.ToGeneration.Version, shortDigest(status.ToGeneration.Digest))
	if status.HoldingGeneration.Digest != "" {
		_, _ = fmt.Fprintf(stdout, "  holding generation: %s (%s)\n", status.HoldingGeneration.Version, shortDigest(status.HoldingGeneration.Digest))
	}
	if status.Failure != "" {
		_, _ = fmt.Fprintf(stdout, "  error: %s\n", status.Failure)
	}
	return 0
}
