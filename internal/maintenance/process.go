// Package maintenance owns the daemon process-control state machine used for
// recoverable cold upgrades. It deliberately does not mutate domain state.
package maintenance

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

type State string

const (
	StateQuiescing State = "quiescing"
	StateReady     State = "ready"
	StateHolding   State = "holding"
	StateResumed   State = "resumed"
	StateFailed    State = "failed"
)

var ErrConflict = errors.New("maintenance request conflicts with current process state")

type Generation struct {
	Digest  string `json:"digest"`
	Version string `json:"version,omitempty"`
}

type Target struct {
	WorkID         string `json:"work_id"`
	SessionID      string `json:"session_id"`
	WorkGeneration int64  `json:"work_generation"`
	AgentName      string `json:"agent_name,omitempty"`
	AgentKind      string `json:"agent_kind,omitempty"`
	WorkspaceID    string `json:"workspace_id,omitempty"`
	TabID          string `json:"tab_id,omitempty"`
	PaneID         string `json:"pane_id,omitempty"`
	TerminalID     string `json:"terminal_id,omitempty"`
}

type Status struct {
	ID                string     `json:"id"`
	RequestID         string     `json:"request_id"`
	ResumeRequestID   string     `json:"resume_request_id,omitempty"`
	State             State      `json:"state"`
	FromGeneration    Generation `json:"from_generation"`
	ToGeneration      Generation `json:"to_generation"`
	HoldingGeneration Generation `json:"holding_generation,omitempty"`
	Targets           []Target   `json:"targets"`
	StartedAt         string     `json:"started_at"`
	UpdatedAt         string     `json:"updated_at"`
	ReadyAt           string     `json:"ready_at,omitempty"`
	HoldingAt         string     `json:"holding_at,omitempty"`
	ResumedAt         string     `json:"resumed_at,omitempty"`
	Failure           string     `json:"failure,omitempty"`
}

type PrepareRequest struct {
	RequestID    string     `json:"request_id"`
	ToGeneration Generation `json:"to_generation"`
}

type ResumeRequest struct {
	RequestID string `json:"request_id"`
}

type RuntimeGate interface {
	Quiesce(func() error) error
}

type Domain interface {
	ActiveMaintenanceTargets(context.Context) ([]Target, error)
	MaintenanceTargetsTerminal(context.Context, []Target) (bool, error)
}

type targetIdentityValidator interface {
	ValidateMaintenanceTargets(context.Context, []Target) error
}

type recoveryIdentityValidator interface {
	ValidateMaintenanceRecovery(context.Context, Status) error
}

type Store interface {
	Read() (Status, error)
	Write(Status) error
}

// AuthorizeGeneration validates a process binary before it may mutate or open
// a Workspace with active maintenance state.
func AuthorizeGeneration(store Store, generation Generation) (Status, error) {
	if store == nil {
		return Status{}, errors.New("maintenance state store is required")
	}
	if err := ValidateGeneration(generation); err != nil {
		return Status{}, err
	}
	status, err := store.Read()
	if errors.Is(err, os.ErrNotExist) {
		return Status{}, nil
	}
	if err != nil {
		return Status{}, err
	}
	allowed := false
	switch status.State {
	case StateQuiescing, StateFailed:
		allowed = generation.Digest == status.FromGeneration.Digest
	case StateReady:
		allowed = generation.Digest == status.FromGeneration.Digest || generation.Digest == status.ToGeneration.Digest
	case StateHolding:
		allowed = generation.Digest == status.FromGeneration.Digest ||
			generation.Digest == status.ToGeneration.Digest ||
			generation.Digest == status.HoldingGeneration.Digest
	case StateResumed:
		allowed = true
	}
	if !allowed {
		return Status{}, fmt.Errorf("%w: generation %s is not allowed while maintenance %s is %s", ErrConflict, generation.Digest, status.ID, status.State)
	}
	return status, nil
}

// AuthorizeDirectMutation is stricter than generation authorization: an
// ordinary out-of-process writer may mutate only outside maintenance or after
// maintenance has durably resumed.
func AuthorizeDirectMutation(store Store, generation Generation) (Status, error) {
	status, err := AuthorizeGeneration(store, generation)
	if err != nil {
		return Status{}, err
	}
	if status.State != "" && status.State != StateResumed {
		return status, fmt.Errorf("%w: direct Workspace mutation is forbidden while maintenance %s is %s", ErrConflict, status.ID, status.State)
	}
	return status, nil
}

// AuthorizeHoldingWebUI permits the reviewed target holder to serve the
// read-only WebUI during Holding without granting ordinary mutation access.
func AuthorizeHoldingWebUI(store Store, generation Generation) (Status, error) {
	status, err := AuthorizeGeneration(store, generation)
	if err != nil {
		return Status{}, err
	}
	if status.State == "" || status.State == StateResumed {
		return status, nil
	}
	if status.State == StateHolding &&
		generation.Digest == status.ToGeneration.Digest &&
		generation.Digest == status.HoldingGeneration.Digest {
		return status, nil
	}
	return status, fmt.Errorf("%w: WebUI is forbidden while maintenance %s is %s for generation %s", ErrConflict, status.ID, status.State, generation.Digest)
}

type Process struct {
	Store               Store
	Runtime             RuntimeGate
	Domain              Domain
	RequireInitialized  func(context.Context) error
	CurrentGeneration   func() (Generation, error)
	HotUpdateInProgress func() bool
	ResumeRuntime       func(context.Context, []Target) error
	Now                 func() time.Time
	Arbitration         sync.Locker
	StateActivation     func(context.Context, func() error) error

	mu             sync.Mutex
	runtimeStarted bool
}

func (p *Process) Prepare(ctx context.Context, request PrepareRequest) (Status, error) {
	if p.StateActivation == nil {
		return p.prepare(ctx, request)
	}
	var result Status
	err := p.StateActivation(ctx, func() error {
		var prepareErr error
		result, prepareErr = p.prepare(ctx, request)
		return prepareErr
	})
	return result, err
}

func (p *Process) prepare(ctx context.Context, request PrepareRequest) (Status, error) {
	if p.Arbitration != nil {
		p.Arbitration.Lock()
		defer p.Arbitration.Unlock()
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.Store == nil || p.Runtime == nil || p.Domain == nil {
		return Status{}, errors.New("maintenance process dependencies are incomplete")
	}
	if strings.TrimSpace(request.RequestID) == "" {
		return Status{}, errors.New("request_id is required")
	}
	if err := ValidateGeneration(request.ToGeneration); err != nil {
		return Status{}, fmt.Errorf("to_generation: %w", err)
	}
	if p.RequireInitialized != nil {
		if err := p.RequireInitialized(ctx); err != nil {
			return Status{}, fmt.Errorf("maintenance requires an initialized Workspace: %w", err)
		}
	}
	if p.HotUpdateInProgress != nil && p.HotUpdateInProgress() {
		return Status{}, fmt.Errorf("%w: daemon hot update is active", ErrConflict)
	}
	var result Status
	err := p.Runtime.Quiesce(func() error {
		current, err := p.Store.Read()
		switch {
		case err == nil:
			if current.RequestID == request.RequestID && current.ToGeneration == request.ToGeneration &&
				(current.State == StateQuiescing || current.State == StateReady) {
				result = current
				return nil
			}
			if current.State != StateResumed && current.State != StateFailed {
				return fmt.Errorf("%w: maintenance %s is %s", ErrConflict, current.ID, current.State)
			}
		case !errors.Is(err, os.ErrNotExist):
			return err
		}
		if p.CurrentGeneration == nil {
			return errors.New("current daemon generation is unavailable")
		}
		from, err := p.CurrentGeneration()
		if err != nil {
			return fmt.Errorf("read current daemon generation: %w", err)
		}
		if err := ValidateGeneration(from); err != nil {
			return fmt.Errorf("from_generation: %w", err)
		}
		targets, err := p.Domain.ActiveMaintenanceTargets(ctx)
		if err != nil {
			return fmt.Errorf("capture active maintenance targets: %w", err)
		}
		now := p.timestamp()
		result = Status{
			ID: newID(), RequestID: request.RequestID, State: StateQuiescing,
			FromGeneration: from, ToGeneration: request.ToGeneration,
			Targets: append([]Target(nil), targets...), StartedAt: now, UpdatedAt: now,
		}
		return p.Store.Write(result)
	})
	return result, err
}

func (p *Process) Status() (Status, error) {
	if p.Store == nil {
		return Status{}, errors.New("maintenance state store is required")
	}
	return p.Store.Read()
}

// Recover restores the irreversible quiesce gate after a same-generation
// crash. Its stop result tells the server to exit once frozen Work is terminal.
func (p *Process) Recover(ctx context.Context) (Status, bool, error) {
	return p.recover(ctx, nil)
}

// RecoverWithGeneration performs cold-open state handling after migrations.
func (p *Process) RecoverWithGeneration(ctx context.Context, generation Generation) (Status, bool, error) {
	return p.recover(ctx, &generation)
}

func (p *Process) recover(ctx context.Context, generation *Generation) (Status, bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.Store == nil {
		return Status{}, false, errors.New("maintenance state store is required")
	}
	status, err := p.Store.Read()
	if err != nil {
		return Status{}, false, err
	}
	if validator, ok := p.Domain.(recoveryIdentityValidator); ok &&
		status.State != StateResumed && status.State != StateFailed {
		if err := validator.ValidateMaintenanceRecovery(ctx, status); err != nil {
			return Status{}, false, err
		}
	} else if validator, ok := p.Domain.(targetIdentityValidator); ok &&
		status.State != StateResumed && status.State != StateFailed {
		if err := validator.ValidateMaintenanceTargets(ctx, status.Targets); err != nil {
			return Status{}, false, err
		}
	}
	switch status.State {
	case StateQuiescing:
		if p.Runtime == nil {
			return Status{}, false, errors.New("maintenance runtime gate is required")
		}
		if err := p.Runtime.Quiesce(func() error { return nil }); err != nil {
			return Status{}, false, err
		}
		return p.pollLocked(ctx, status)
	case StateReady:
		if generation == nil {
			return status, true, nil
		}
		if err := ValidateGeneration(*generation); err != nil {
			return Status{}, false, err
		}
		if generation.Digest != status.ToGeneration.Digest {
			return Status{}, false, fmt.Errorf("%w: cold generation digest does not match maintenance target", ErrConflict)
		}
		status.State = StateHolding
		status.HoldingGeneration = *generation
		status.HoldingAt = p.timestamp()
		status.UpdatedAt = status.HoldingAt
		if err := p.Store.Write(status); err != nil {
			return Status{}, false, err
		}
		return status, false, nil
	case StateHolding:
		if generation != nil {
			accepted, takeover, err := holdingTakeover(status, *generation)
			if err != nil {
				return Status{}, false, err
			}
			if takeover {
				status.HoldingGeneration = accepted
				status.HoldingAt = p.timestamp()
				status.UpdatedAt = status.HoldingAt
				if err := p.Store.Write(status); err != nil {
					return Status{}, false, err
				}
			}
		}
		return status, false, nil
	case StateResumed, StateFailed:
		return status, false, nil
	default:
		return Status{}, false, errors.New("unsupported maintenance state")
	}
}

func holdingTakeover(status Status, generation Generation) (Generation, bool, error) {
	if err := ValidateGeneration(generation); err != nil {
		return Generation{}, false, err
	}
	switch generation.Digest {
	case status.HoldingGeneration.Digest, status.ToGeneration.Digest, status.FromGeneration.Digest:
		return generation, status.HoldingGeneration.Digest != generation.Digest, nil
	default:
		return Generation{}, false, fmt.Errorf("%w: holding generation is not the target or original from generation", ErrConflict)
	}
}

func (p *Process) Poll(ctx context.Context) (Status, bool, error) {
	if p.Arbitration != nil {
		p.Arbitration.Lock()
		defer p.Arbitration.Unlock()
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	status, err := p.Store.Read()
	if err != nil {
		return Status{}, false, err
	}
	return p.pollLocked(ctx, status)
}

func (p *Process) pollLocked(ctx context.Context, status Status) (Status, bool, error) {
	if status.State == StateReady {
		return status, true, nil
	}
	if status.State != StateQuiescing {
		return status, false, nil
	}
	if p.Domain == nil {
		return Status{}, false, errors.New("maintenance domain observer is required")
	}
	terminal, err := p.Domain.MaintenanceTargetsTerminal(ctx, status.Targets)
	if err != nil {
		failed, failErr := p.persistFailed(status, "frozen Work identity is inconsistent")
		if failErr != nil {
			return Status{}, false, failErr
		}
		return failed, false, err
	}
	if !terminal {
		return status, false, nil
	}
	status.State = StateReady
	status.ReadyAt = p.timestamp()
	status.UpdatedAt = status.ReadyAt
	if err := p.Store.Write(status); err != nil {
		return Status{}, false, err
	}
	return status, true, nil
}

func (p *Process) persistFailed(status Status, reason string) (Status, error) {
	status.State = StateFailed
	status.Failure = reason
	status.UpdatedAt = p.timestamp()
	if err := p.Store.Write(status); err != nil {
		return Status{}, err
	}
	return status, nil
}

func (p *Process) Resume(ctx context.Context, request ResumeRequest) (Status, error) {
	if p.Arbitration != nil {
		p.Arbitration.Lock()
		defer p.Arbitration.Unlock()
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if strings.TrimSpace(request.RequestID) == "" {
		return Status{}, errors.New("request_id is required")
	}
	status, err := p.Store.Read()
	if err != nil {
		return Status{}, err
	}
	if status.State == StateResumed && status.ResumeRequestID == request.RequestID {
		return status, nil
	}
	if status.State != StateHolding {
		return Status{}, fmt.Errorf("%w: resume requires holding state", ErrConflict)
	}
	if status.ResumeRequestID != "" && status.ResumeRequestID != request.RequestID {
		return Status{}, fmt.Errorf("%w: a different resume request is active", ErrConflict)
	}
	if p.ResumeRuntime == nil {
		return Status{}, errors.New("runtime resume operation is unavailable")
	}
	if status.ResumeRequestID != request.RequestID {
		status.ResumeRequestID = request.RequestID
		status.UpdatedAt = p.timestamp()
		if err := p.Store.Write(status); err != nil {
			return Status{}, err
		}
	}
	if validator, ok := p.Domain.(recoveryIdentityValidator); ok {
		if err := validator.ValidateMaintenanceRecovery(ctx, status); err != nil {
			return Status{}, fmt.Errorf("validate resume intent identity: %w", err)
		}
	}
	if err := reachIntegrationResumeCheckpoint(ctx, "intent_persisted"); err != nil {
		return Status{}, fmt.Errorf("resume checkpoint intent_persisted: %w", err)
	}
	if !p.runtimeStarted {
		if err := p.ResumeRuntime(ctx, status.Targets); err != nil {
			return Status{}, fmt.Errorf("resume runtime: %w", err)
		}
		p.runtimeStarted = true
	}
	if validator, ok := p.Domain.(recoveryIdentityValidator); ok {
		if err := validator.ValidateMaintenanceRecovery(ctx, status); err != nil {
			return Status{}, fmt.Errorf("validate started runtime identity: %w", err)
		}
	}
	if err := reachIntegrationResumeCheckpoint(ctx, "runtime_started"); err != nil {
		return Status{}, fmt.Errorf("resume checkpoint runtime_started: %w", err)
	}
	status.State = StateResumed
	status.ResumeRequestID = request.RequestID
	status.ResumedAt = p.timestamp()
	status.UpdatedAt = status.ResumedAt
	if err := p.Store.Write(status); err != nil {
		return Status{}, err
	}
	return status, nil
}

func (p *Process) timestamp() string {
	now := time.Now()
	if p.Now != nil {
		now = p.Now()
	}
	return now.UTC().Format(time.RFC3339Nano)
}

func newID() string {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return fmt.Sprintf("maintenance-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(value[:])
}
