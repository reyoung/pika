package daemonupdate

import (
	"context"
	"fmt"
	"os"
	"time"
)

const ActivationDeadline = 15 * time.Second

type Handoff struct {
	Child        *Child
	Status       Status
	ListenerKeep *os.File
	LockKeep     *os.File
}

type SpawnFunc func(path string, listenerKeep, lockKeep *os.File) (*Child, error)
type HealthFunc func(context.Context) (string, error)

type Outcome string

const (
	OutcomeCommitted  Outcome = "committed"
	OutcomeRolledBack Outcome = "rolled_back"
)

// Activate owns the update transaction after the old HTTP server is
// quiescent. It either commits the candidate or restores and starts the prior
// generation using the retained listener and lock descriptors.
func Activate(workspaceRoot string, handoff *Handoff, spawn SpawnFunc, health HealthFunc) (Outcome, error) {
	if handoff == nil || handoff.Child == nil || spawn == nil || health == nil {
		return "", fmt.Errorf("complete daemon handoff is required")
	}
	status := handoff.Status
	status.State = StateActivating
	_ = WriteStatus(workspaceRoot, status)
	activationErr := handoff.Child.Control.Send(MessageActivate, nil)
	if activationErr == nil {
		activationErr = handoff.Child.Control.Expect(MessageReady, ActivationDeadline)
	}
	if activationErr == nil {
		_, activationErr = CommitCurrent(workspaceRoot, Candidate{
			Path: status.To.Path, Digest: status.To.Digest, Probe: CurrentProbe(status.To.Version),
		}, time.Now())
	}
	if activationErr == nil {
		status.State = StateStabilizing
		_ = WriteStatus(workspaceRoot, status)
		activationErr = handoff.Child.Control.Send(MessageCommit, nil)
	}
	if activationErr == nil {
		activationErr = waitForDigest(status.To.Digest, health)
	}
	if activationErr == nil {
		status.State = StateCommitted
		_ = WriteStatus(workspaceRoot, status)
		return OutcomeCommitted, nil
	}

	status.State, status.Failure = StateRollingBack, activationErr.Error()
	_ = WriteStatus(workspaceRoot, status)
	stopChild(handoff.Child)
	if err := RestoreCurrent(workspaceRoot, status.From); err != nil {
		return failStatus(workspaceRoot, status, fmt.Errorf("%v; restore current generation: %w", activationErr, err))
	}
	rollback, err := spawn(status.From.Path, handoff.ListenerKeep, handoff.LockKeep)
	if err == nil {
		err = rollback.Control.Expect(MessagePrepared, 75*time.Second)
	}
	if err == nil {
		err = rollback.Control.Send(MessageActivate, nil)
	}
	if err == nil {
		err = rollback.Control.Expect(MessageReady, ActivationDeadline)
	}
	if err == nil {
		err = rollback.Control.Send(MessageCommit, nil)
	}
	if err == nil {
		err = waitForDigest(status.From.Digest, health)
	}
	if err != nil {
		stopChild(rollback)
		return failStatus(workspaceRoot, status, fmt.Errorf("%v; automatic rollback: %w", activationErr, err))
	}
	_ = rollback.Control.Close()
	status.State = StateRolledBack
	_ = WriteStatus(workspaceRoot, status)
	return OutcomeRolledBack, nil
}

func (h *Handoff) Close() {
	if h == nil {
		return
	}
	if h.ListenerKeep != nil {
		_ = h.ListenerKeep.Close()
	}
	if h.LockKeep != nil {
		_ = h.LockKeep.Close()
	}
	if h.Child != nil && h.Child.Control != nil {
		_ = h.Child.Control.Close()
	}
}

func waitForDigest(want string, health HealthFunc) error {
	deadline := time.Now().Add(ActivationDeadline)
	var lastDigest string
	var lastErr error
	for {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		lastDigest, lastErr = health(ctx)
		cancel()
		if lastErr == nil && lastDigest == want {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("successor did not stabilize within 15s: health=%v digest=%q", lastErr, lastDigest)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func stopChild(child *Child) {
	if child == nil || child.Command == nil || child.Command.Process == nil {
		return
	}
	_ = child.Command.Process.Kill()
	_, _ = child.Command.Process.Wait()
	if child.Control != nil {
		_ = child.Control.Close()
	}
}

func failStatus(workspaceRoot string, status Status, err error) (Outcome, error) {
	status.State, status.Failure = StateFailed, err.Error()
	_ = WriteStatus(workspaceRoot, status)
	return "", err
}
