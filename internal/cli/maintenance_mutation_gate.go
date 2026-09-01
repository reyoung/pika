package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"

	"github.com/reyoung/pika-go/internal/daemon"
	"github.com/reyoung/pika-go/internal/maintenance"
	"github.com/reyoung/pika-go/internal/symphony"
	"github.com/reyoung/pika-go/internal/toolapp"
)

type maintenanceMutationGate struct {
	arbitration sync.Locker
	store       *maintenance.FileStore
	engine      *symphony.Engine
}

func (gate maintenanceMutationGate) Enter(_ context.Context, kind daemon.MutationKind, agentSessionID string) (func(), error) {
	if gate.arbitration == nil || gate.store == nil {
		return func() {}, nil
	}
	gate.arbitration.Lock()
	release := func() { gate.arbitration.Unlock() }
	status, err := gate.store.Read()
	if errors.Is(err, os.ErrNotExist) || (err == nil && (status.State == "" || status.State == maintenance.StateResumed)) {
		return release, nil
	}
	if err != nil {
		release()
		return nil, err
	}
	if status.State == maintenance.StateQuiescing {
		switch kind {
		case daemon.MutationMCP:
			return release, nil
		case daemon.MutationProvider:
			for _, target := range status.Targets {
				if target.SessionID == agentSessionID {
					return release, nil
				}
			}
		}
	}
	release()
	return nil, fmt.Errorf("mutation %s is forbidden while maintenance %s is %s", kind, status.ID, status.State)
}

func (gate maintenanceMutationGate) AuthorizeInvocation(ctx context.Context, grant symphony.AgentGrant, _ toolapp.Call) error {
	if gate.store == nil {
		return nil
	}
	status, err := gate.store.Read()
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if status.State == "" || status.State == maintenance.StateResumed {
		return nil
	}
	if status.State != maintenance.StateQuiescing {
		return fmt.Errorf("MCP mutation is forbidden while maintenance %s is %s", status.ID, status.State)
	}
	var frozen *maintenance.Target
	for index := range status.Targets {
		target := &status.Targets[index]
		if target.SessionID == grant.AgentSessionID && target.WorkID == grant.WorkID && target.WorkGeneration == grant.Generation {
			frozen = target
			break
		}
	}
	if frozen == nil {
		return errors.New("MCP mutation does not belong to a frozen maintenance Target")
	}
	if gate.engine == nil {
		return errors.New("maintenance MCP validation requires the domain engine")
	}
	work, err := gate.engine.RuntimeWork(ctx, frozen.WorkID)
	if err != nil {
		return err
	}
	if work.Work.Generation != frozen.WorkGeneration || work.Work.Role != grant.Role {
		return errors.New("MCP mutation role or Work generation drifted from the frozen maintenance Target")
	}
	return nil
}
