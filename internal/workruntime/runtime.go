package workruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/reyoung/pika-go/internal/configuration"
	"github.com/reyoung/pika-go/internal/provider"
	"github.com/reyoung/pika-go/internal/symphony"
)

type Observation struct {
	AgentName   string
	AgentKind   string
	WorkspaceID string
	TabID       string
	PaneID      string
	TerminalID  string
	Status      string
}

type Snapshot struct {
	Sessions []Observation
}

type StartSpec struct {
	AgentName        string
	AgentKind        string
	Repository       string
	PreferredPaneID  string
	PaneLabel        string
	PaneLabelNeedsID bool
	Environment      map[string]string
	StartupTimeout   time.Duration
	ReturnOnLaunch   bool
}

type Preparation struct {
	Prompt         string
	Environment    map[string]string
	EphemeralPath  string
	StartupTimeout time.Duration
	ReturnOnLaunch bool
	Cleanup        func() error
}

type Preparer interface {
	Prepare(context.Context, symphony.AgentSession, symphony.RuntimeWork) (Preparation, error)
}

type WorkspacePreparer interface {
	PrepareWork(context.Context, symphony.RuntimeWork) (symphony.RuntimeWork, error)
}

type Runtime interface {
	Snapshot(context.Context) (Snapshot, error)
	Start(context.Context, StartSpec) (Observation, error)
	Prompt(context.Context, string, string) error
	Close(context.Context, string) error
}

type ProviderPrompter interface {
	PromptForProvider(context.Context, string, string, string) error
}

type AgentKeySender interface {
	SendAgentKeys(context.Context, string, []string) error
}

type Store interface {
	RuntimeWork(context.Context, string) (symphony.RuntimeWork, error)
	EnsureAgentSession(context.Context, symphony.AgentSession) error
	BindPane(context.Context, string, symphony.PaneBinding) error
	CurrentAgentSession(context.Context, string) (symphony.AgentSession, symphony.PaneBinding, bool, error)
	MarkAgentSessionEnded(context.Context, string, symphony.AgentSessionStatus) error
	ActiveAgentSessions(context.Context) ([]symphony.ActiveAgentSession, error)
	ReplaceLostAgentSession(context.Context, string) (string, error)
	ReadAgentSession(context.Context, string) (symphony.AgentSession, error)
	BeginFollowUpDelivery(context.Context, string, string) (string, string, bool, error)
	FinishFollowUpDelivery(context.Context, string, bool) error
	PendingSchedulerControlActions(context.Context, string) ([]symphony.SchedulerControlActionView, error)
	BeginSchedulerControlAction(context.Context, string, string) (bool, error)
	FinishSchedulerControlAction(context.Context, string, symphony.SchedulerControlActionStatus, string) error
	CompleteSchedulerControlCycle(context.Context, string) error
}

type Sink struct {
	Store                       Store
	Runtime                     Runtime
	AgentKind                   string
	AgentConfigPath             string
	Providers                   *provider.Registry
	ProviderRuntimeRoot         string
	RequireProviderCapabilities bool
	Preparer                    Preparer
	WorkspacePreparer           WorkspacePreparer
}

func (s Sink) Dispatch(ctx context.Context, effect symphony.RuntimeEffect) error {
	if s.Store == nil || s.Runtime == nil {
		return errors.New("work runtime store and adapter are required")
	}
	switch effect.Type {
	case "work.start_requested":
		return s.start(ctx, effect)
	case "work.close_requested":
		return s.close(ctx, effect)
	case "session.close_requested":
		return s.closeSession(ctx, effect)
	case "followup.deliver_requested":
		return s.deliverFollowUp(ctx, effect)
	case "scheduler.control_requested":
		return s.controlScheduler(ctx, effect)
	default:
		return fmt.Errorf("unsupported runtime effect %q", effect.Type)
	}
}

func (s Sink) controlScheduler(ctx context.Context, effect symphony.RuntimeEffect) error {
	var payload struct {
		CycleID string `json:"cycle_id"`
	}
	if err := json.Unmarshal(effect.Payload, &payload); err != nil || payload.CycleID == "" {
		return fmt.Errorf("decode Scheduler control effect %s: cycle_id is required", effect.ID)
	}
	actions, err := s.Store.PendingSchedulerControlActions(ctx, payload.CycleID)
	if err != nil {
		return err
	}
	snapshot, snapshotErr := s.Runtime.Snapshot(ctx)
	registry := s.Providers
	if registry == nil {
		registry = provider.DefaultRegistry()
	}
	semaphore := make(chan struct{}, 32)
	errorsFound := make(chan error, len(actions))
	var group sync.WaitGroup
	for _, action := range actions {
		action := action
		group.Add(1)
		go func() {
			defer group.Done()
			select {
			case semaphore <- struct{}{}:
				defer func() { <-semaphore }()
			case <-ctx.Done():
				durableCtx := context.WithoutCancel(ctx)
				begun, beginErr := s.Store.BeginSchedulerControlAction(durableCtx, action.ID, "unknown")
				if beginErr != nil {
					errorsFound <- beginErr
					return
				}
				if begun {
					if finishErr := s.Store.FinishSchedulerControlAction(durableCtx, action.ID, symphony.SchedulerActionFailed, "Scheduler control deadline exceeded before delivery"); finishErr != nil {
						errorsFound <- finishErr
					}
				}
				return
			}
			actionCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
			defer cancel()
			durableCtx := context.WithoutCancel(ctx)
			observed := "unknown"
			observation, found := findObservation(snapshot, action.AgentName, "")
			if found {
				observed = strings.ToLower(observation.Status)
			}
			begun, beginErr := s.Store.BeginSchedulerControlAction(durableCtx, action.ID, observed)
			if beginErr != nil {
				errorsFound <- beginErr
				return
			}
			if !begun {
				return
			}
			finish := func(status symphony.SchedulerControlActionStatus, message string) {
				if finishErr := s.Store.FinishSchedulerControlAction(durableCtx, action.ID, status, message); finishErr != nil {
					errorsFound <- finishErr
				}
			}
			if snapshotErr != nil {
				finish(symphony.SchedulerActionFailed, "snapshot runtime: "+snapshotErr.Error())
				return
			}
			if !found {
				finish(symphony.SchedulerActionSkipped, "Agent Session is no longer present in the runtime")
				return
			}
			session, readErr := s.Store.ReadAgentSession(durableCtx, action.AgentSessionID)
			if readErr != nil || (session.Status != symphony.AgentSessionStarting && session.Status != symphony.AgentSessionRunning) {
				finish(symphony.SchedulerActionSkipped, "Agent Session is no longer active")
				return
			}
			if action.Action == "resume" {
				work, workErr := s.Store.RuntimeWork(durableCtx, action.WorkID)
				if workErr != nil || work.Work.Status != symphony.WorkPending || observed == "done" || observed == "exited" {
					finish(symphony.SchedulerActionSkipped, "Agent Session or Work is no longer resumable")
					return
				}
				if observed == "unknown" || observed == "" {
					finish(symphony.SchedulerActionFailed, "runtime Agent status is unknown")
					return
				}
				// An interrupt acknowledgement only proves key delivery. If resume
				// immediately follows pause, wait for the interrupted turn to leave
				// working before injecting the continuation; otherwise a provider TUI
				// may acknowledge and silently discard the prompt during teardown.
				if observed == "working" {
					observation, found, observed, readErr = waitForPromptableAgent(actionCtx, s.Runtime, action.AgentName)
					if readErr != nil {
						finish(symphony.SchedulerActionFailed, readErr.Error())
						return
					}
					if !found || observed == "done" || observed == "exited" {
						finish(symphony.SchedulerActionSkipped, "Agent Session is no longer resumable")
						return
					}
				}
				if promptErr := promptProvider(actionCtx, s.Runtime, observation.PaneID, action.Message, action.AgentKind); promptErr != nil {
					finish(symphony.SchedulerActionFailed, promptErr.Error())
					return
				}
				finish(symphony.SchedulerActionSent, "")
				return
			}
			if action.Action != "pause" {
				finish(symphony.SchedulerActionFailed, "unsupported Scheduler control action")
				return
			}
			switch observed {
			case "idle", "done", "exited":
				finish(symphony.SchedulerActionSkipped, "Agent has no active turn")
				return
			case "working", "blocked":
			default:
				finish(symphony.SchedulerActionFailed, "runtime Agent status is unknown")
				return
			}
			keys, keysErr := registry.InterruptKeys(action.AgentKind)
			if keysErr != nil {
				finish(symphony.SchedulerActionFailed, keysErr.Error())
				return
			}
			sender, ok := s.Runtime.(AgentKeySender)
			if !ok {
				finish(symphony.SchedulerActionFailed, "runtime does not support Agent key delivery")
				return
			}
			if sendErr := sender.SendAgentKeys(actionCtx, action.AgentName, keys); sendErr != nil {
				finish(symphony.SchedulerActionFailed, sendErr.Error())
				return
			}
			finish(symphony.SchedulerActionSent, "")
		}()
	}
	group.Wait()
	close(errorsFound)
	for actionErr := range errorsFound {
		if actionErr != nil {
			return actionErr
		}
	}
	return s.Store.CompleteSchedulerControlCycle(context.WithoutCancel(ctx), payload.CycleID)
}

func waitForPromptableAgent(ctx context.Context, runtime Runtime, agentName string) (Observation, bool, string, error) {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		snapshot, err := runtime.Snapshot(ctx)
		if err != nil {
			return Observation{}, false, "unknown", fmt.Errorf("snapshot runtime while waiting for interrupted Agent: %w", err)
		}
		observation, found := findObservation(snapshot, agentName, "")
		if !found {
			return Observation{}, false, "", nil
		}
		status := strings.ToLower(observation.Status)
		switch status {
		case "idle", "blocked", "done", "exited":
			return observation, true, status, nil
		case "working":
		default:
			return Observation{}, true, status, fmt.Errorf("runtime Agent status became %q while waiting for interrupt", status)
		}
		select {
		case <-ctx.Done():
			return Observation{}, true, status, fmt.Errorf("wait for interrupted Agent to become promptable: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

func (s Sink) closeSession(ctx context.Context, effect symphony.RuntimeEffect) error {
	var payload struct {
		AgentSessionID string `json:"agent_session_id"`
		AgentName      string `json:"agent_name"`
		PaneID         string `json:"pane_id"`
		TerminalID     string `json:"terminal_id"`
	}
	if err := json.Unmarshal(effect.Payload, &payload); err != nil || payload.AgentSessionID == "" || payload.AgentName == "" {
		return fmt.Errorf("decode Session close effect %s: complete Session identity is required", effect.ID)
	}
	snapshot, err := s.Runtime.Snapshot(ctx)
	if err != nil {
		return fmt.Errorf("snapshot runtime before Session close: %w", err)
	}
	if observation, found := findObservation(snapshot, payload.AgentName, payload.TerminalID); found {
		if err := s.Runtime.Close(ctx, observation.PaneID); err != nil {
			return err
		}
	} else if payload.PaneID != "" {
		if err := s.Runtime.Close(ctx, payload.PaneID); err != nil {
			return err
		}
	}
	session, err := s.Store.ReadAgentSession(ctx, payload.AgentSessionID)
	if err != nil {
		return err
	}
	return cleanupSessionResources(s.ProviderRuntimeRoot, session)
}

func (s Sink) deliverFollowUp(ctx context.Context, effect symphony.RuntimeEffect) error {
	var payload struct {
		RequestID    string `json:"request_id"`
		TargetWorkID string `json:"target_work_id"`
		Message      string `json:"message"`
		DeliveryID   string `json:"delivery_id"`
	}
	if err := json.Unmarshal(effect.Payload, &payload); err != nil || payload.RequestID == "" || payload.TargetWorkID == "" || payload.Message == "" || payload.DeliveryID == "" {
		return fmt.Errorf("decode Follow-up delivery effect %s: complete delivery identity is required", effect.ID)
	}
	targetWorkID, paneID, deliver, err := s.Store.BeginFollowUpDelivery(ctx, payload.RequestID, payload.DeliveryID)
	if err != nil {
		return err
	}
	if !deliver {
		return nil
	}
	if targetWorkID != payload.TargetWorkID {
		return errors.New("Follow-up delivery target changed")
	}
	targetSession, targetBinding, found, err := s.Store.CurrentAgentSession(ctx, targetWorkID)
	if err != nil {
		return err
	}
	if !found || targetBinding.PaneID != paneID {
		return errors.New("Follow-up target Agent Session changed before delivery")
	}
	if err := promptProvider(ctx, s.Runtime, paneID, payload.Message, targetSession.AgentKind); err != nil {
		if markErr := s.Store.FinishFollowUpDelivery(ctx, payload.RequestID, false); markErr != nil {
			return fmt.Errorf("deliver Follow-up: %v; mark delivery unknown: %w", err, markErr)
		}
		// The transport outcome is uncertain. Persist it and acknowledge the outbox
		// effect so recovery never sends a blind duplicate prompt.
		return nil
	}
	return s.Store.FinishFollowUpDelivery(ctx, payload.RequestID, true)
}

func (s Sink) start(ctx context.Context, effect symphony.RuntimeEffect) error {
	var payload struct {
		WorkID          string `json:"work_id"`
		PreferredPaneID string `json:"preferred_pane_id"`
	}
	if err := json.Unmarshal(effect.Payload, &payload); err != nil || payload.WorkID == "" {
		return fmt.Errorf("decode start effect %s: work_id is required", effect.ID)
	}
	work, err := s.Store.RuntimeWork(ctx, payload.WorkID)
	if err != nil {
		return err
	}
	if s.WorkspacePreparer != nil {
		work, err = s.WorkspacePreparer.PrepareWork(ctx, work)
		if err != nil {
			return fmt.Errorf("prepare Work workspace %s: %w", payload.WorkID, err)
		}
	}
	kind := s.AgentKind
	if kind == "" {
		kind = "codex"
	}
	if s.AgentConfigPath != "" {
		providers := s.Providers
		if providers == nil {
			providers = provider.DefaultRegistry()
		}
		configured, err := configuration.LoadAgentWithRegistry(s.AgentConfigPath, agentConfigRole(work.Work.Role), providers)
		if err != nil {
			return fmt.Errorf("load Agent configuration for %s: %w", work.Work.Role, err)
		}
		kind = configured.Kind
	}
	registry := s.Providers
	if registry == nil {
		registry = provider.DefaultRegistry()
	}
	capabilities, probed := registry.Capabilities(kind)
	if s.RequireProviderCapabilities && !probed {
		return fmt.Errorf("provider %s was not successfully probed before Session start", kind)
	}
	capabilitiesJSON := json.RawMessage(nil)
	if probed {
		capabilitiesJSON, err = json.Marshal(capabilities)
		if err != nil {
			return fmt.Errorf("encode %s provider capabilities: %w", kind, err)
		}
	}
	session := symphony.AgentSession{
		ID:                   effect.ID,
		WorkID:               work.Work.ID,
		Generation:           work.Work.Generation,
		Role:                 work.Work.Role,
		AgentKind:            kind,
		AgentName:            agentName(effect.ID),
		ProviderVersion:      capabilities.Version,
		ProviderCapabilities: capabilitiesJSON,
		Status:               symphony.AgentSessionStarting,
	}
	if err := s.Store.EnsureAgentSession(ctx, session); err != nil {
		return err
	}
	snapshot, err := s.Runtime.Snapshot(ctx)
	if err != nil {
		return fmt.Errorf("snapshot runtime before start: %w", err)
	}
	if observation, found := findObservation(snapshot, session.AgentName, ""); found {
		return s.bind(ctx, session.ID, observation)
	}
	var preparation Preparation
	if s.Preparer != nil {
		preparation, err = s.Preparer.Prepare(ctx, session, work)
		if err != nil {
			return fmt.Errorf("prepare agent session %s: %w", session.ID, err)
		}
	}
	observation, err := s.Runtime.Start(ctx, StartSpec{
		AgentName:        session.AgentName,
		AgentKind:        session.AgentKind,
		Repository:       work.Repository,
		PreferredPaneID:  payload.PreferredPaneID,
		PaneLabel:        workPaneLabel(work),
		PaneLabelNeedsID: work.Work.Role == symphony.RoleIteration,
		Environment:      preparation.Environment,
		StartupTimeout:   preparation.StartupTimeout,
		ReturnOnLaunch:   preparation.ReturnOnLaunch,
	})
	if err != nil {
		return fmt.Errorf("start agent session %s: %w", session.ID, err)
	}
	if err := s.bind(ctx, session.ID, observation); err != nil {
		return err
	}
	if preparation.Prompt != "" {
		if err := promptProvider(ctx, s.Runtime, observation.PaneID, preparation.Prompt, session.AgentKind); err != nil {
			return fmt.Errorf("send activation prompt: %w", err)
		}
	}
	return nil
}

func workPaneLabel(work symphony.RuntimeWork) string {
	withBaselineNumber := func(label string) string {
		if work.BaselineNumber > 1 {
			return fmt.Sprintf("%s %d", label, work.BaselineNumber)
		}
		return label
	}
	switch work.Work.Role {
	case symphony.RoleBaselineDraft:
		return withBaselineNumber("Baseline")
	case symphony.RoleBaselineVerification:
		return withBaselineNumber("Verify")
	case symphony.RoleIteration:
		if work.Work.IterationRound > 0 {
			return fmt.Sprintf("Iteration R%d", work.Work.IterationRound)
		}
		return "Iteration"
	case symphony.RoleIntegration:
		return "Integration"
	case symphony.RoleFollowUp:
		return "Follow-up"
	default:
		return ""
	}
}

func promptProvider(ctx context.Context, runtime Runtime, target, message, providerKind string) error {
	if prompter, ok := runtime.(ProviderPrompter); ok {
		return prompter.PromptForProvider(ctx, target, message, providerKind)
	}
	return runtime.Prompt(ctx, target, message)
}

func agentConfigRole(role symphony.WorkRole) string {
	switch role {
	case symphony.RoleBaselineDraft:
		return "baseline"
	case symphony.RoleBaselineVerification:
		return "baseline_verify"
	case symphony.RoleIteration:
		return "iteration"
	case symphony.RoleIntegration:
		return "integration"
	default:
		return string(role)
	}
}

func (s Sink) close(ctx context.Context, effect symphony.RuntimeEffect) error {
	var payload struct {
		WorkID string `json:"work_id"`
	}
	if err := json.Unmarshal(effect.Payload, &payload); err != nil || payload.WorkID == "" {
		return fmt.Errorf("decode close effect %s: work_id is required", effect.ID)
	}
	session, binding, found, err := s.Store.CurrentAgentSession(ctx, payload.WorkID)
	if err != nil || !found {
		return err
	}
	if binding.PaneID != "" {
		if err := s.Runtime.Close(ctx, binding.PaneID); err != nil {
			return fmt.Errorf("close agent pane %s: %w", binding.PaneID, err)
		}
	}
	if err := cleanupSessionResources(s.ProviderRuntimeRoot, session); err != nil {
		return err
	}
	return s.Store.MarkAgentSessionEnded(ctx, session.ID, symphony.AgentSessionExited)
}

func (s Sink) bind(ctx context.Context, sessionID string, observation Observation) error {
	if observation.WorkspaceID == "" || observation.TabID == "" || observation.PaneID == "" || observation.TerminalID == "" {
		return errors.New("runtime returned an incomplete pane identity")
	}
	return s.Store.BindPane(ctx, sessionID, symphony.PaneBinding{
		WorkspaceID: observation.WorkspaceID,
		TabID:       observation.TabID,
		PaneID:      observation.PaneID,
		TerminalID:  observation.TerminalID,
	})
}

type Reconciler struct {
	Store               Store
	Runtime             Runtime
	LossConfirmation    time.Duration
	ProviderRuntimeRoot string
}

func (r Reconciler) Reconcile(ctx context.Context) error {
	if r.Store == nil || r.Runtime == nil {
		return errors.New("work runtime store and adapter are required")
	}
	snapshot, err := r.Runtime.Snapshot(ctx)
	if err != nil {
		return fmt.Errorf("snapshot runtime for reconciliation: %w", err)
	}
	return r.ReconcileSnapshot(ctx, snapshot)
}

func (r Reconciler) ReconcileSnapshot(ctx context.Context, snapshot Snapshot) error {
	if r.Store == nil {
		return errors.New("work runtime store is required")
	}
	records, err := r.Store.ActiveAgentSessions(ctx)
	if err != nil {
		return err
	}
	for _, record := range records {
		observation, found := findObservation(snapshot, record.Session.AgentName, record.Binding.TerminalID)
		if found {
			if err := r.Store.BindPane(ctx, record.Session.ID, symphony.PaneBinding{
				WorkspaceID: observation.WorkspaceID,
				TabID:       observation.TabID,
				PaneID:      observation.PaneID,
				TerminalID:  observation.TerminalID,
			}); err != nil {
				return err
			}
			continue
		}
		if record.Session.Status == symphony.AgentSessionStarting {
			continue
		}
		delay := r.LossConfirmation
		if delay <= 0 {
			delay = time.Second
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return ctx.Err()
		case <-timer.C:
		}
		confirmed, err := r.Runtime.Snapshot(ctx)
		if err != nil {
			return fmt.Errorf("confirm missing agent session: %w", err)
		}
		if observation, found := findObservation(confirmed, record.Session.AgentName, record.Binding.TerminalID); found {
			if err := r.Store.BindPane(ctx, record.Session.ID, symphony.PaneBinding{
				WorkspaceID: observation.WorkspaceID, TabID: observation.TabID, PaneID: observation.PaneID, TerminalID: observation.TerminalID,
			}); err != nil {
				return err
			}
			continue
		}
		work, err := r.Store.RuntimeWork(ctx, record.Session.WorkID)
		if err != nil {
			return err
		}
		if work.Work.Status != symphony.WorkPending {
			if err := cleanupSessionResources(r.ProviderRuntimeRoot, record.Session); err != nil {
				return err
			}
			if err := r.Store.MarkAgentSessionEnded(ctx, record.Session.ID, symphony.AgentSessionExited); err != nil {
				return err
			}
			continue
		}
		if _, err := r.Store.ReplaceLostAgentSession(ctx, record.Session.ID); err != nil {
			return err
		}
	}
	return nil
}

func cleanupSessionResources(runtimeRoot string, session symphony.AgentSession) error {
	if runtimeRoot == "" || session.AgentKind != "cursor" {
		return nil
	}
	return provider.CleanupCursorSession(runtimeRoot, session.ID)
}

func findObservation(snapshot Snapshot, agentName, terminalID string) (Observation, bool) {
	for _, observation := range snapshot.Sessions {
		if agentName != "" && observation.AgentName == agentName {
			return observation, true
		}
	}
	for _, observation := range snapshot.Sessions {
		if terminalID != "" && observation.TerminalID == terminalID {
			return observation, true
		}
	}
	return Observation{}, false
}

func agentName(effectID string) string {
	var builder strings.Builder
	builder.WriteString("pika-")
	for _, character := range strings.ToLower(effectID) {
		if unicode.IsLetter(character) || unicode.IsDigit(character) || character == '-' || character == '_' {
			builder.WriteRune(character)
		} else {
			builder.WriteByte('-')
		}
		if builder.Len() == 32 {
			break
		}
	}
	return strings.TrimRight(builder.String(), "-_")
}
