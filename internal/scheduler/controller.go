package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/reyoung/pika-go/internal/protocol"
	"github.com/reyoung/pika-go/internal/symphony"
)

type ExclusiveRunner interface {
	Exclusive(func() error) error
}

type EffectDispatcher interface {
	DispatchEffect(context.Context, string) error
}

type Store interface {
	symphony.Symphony
	SchedulerControlCycle(context.Context, string) (symphony.SchedulerControlCycleView, error)
}

// Controller is the single seam for a Scheduler state transition plus its
// synchronous runtime control cycle. Callers do not need to coordinate the
// domain store, runtime watcher, outbox priority, or receipt replay rules.
type Controller struct {
	Store      Store
	Dispatcher EffectDispatcher
	Exclusive  ExclusiveRunner
	After      func()
}

func (c Controller) Apply(ctx context.Context, command symphony.Command) (protocol.SchedulerControlResponse, error) {
	if c.Store == nil || c.Dispatcher == nil || c.Exclusive == nil {
		return protocol.SchedulerControlResponse{}, errors.New("Scheduler store, dispatcher, and exclusive runner are required")
	}
	var result protocol.SchedulerControlResponse
	committed := false
	err := c.Exclusive.Exclusive(func() error {
		receipt, err := c.Store.Apply(ctx, command)
		if err != nil {
			return err
		}
		result.Receipt = receipt
		committed = true
		var commandResult struct {
			Status   symphony.SchedulerStatus `json:"scheduler_status"`
			Epoch    int64                    `json:"epoch"`
			CycleID  string                   `json:"cycle_id"`
			EffectID string                   `json:"effect_id"`
			Noop     bool                     `json:"noop"`
		}
		if err := json.Unmarshal(receipt.Result, &commandResult); err != nil {
			return fmt.Errorf("decode Scheduler receipt: %w", err)
		}
		if commandResult.Noop {
			result.Control = symphony.SchedulerControlCycleView{Epoch: commandResult.Epoch, Action: schedulerCommandAction(command), Status: "noop"}
			return nil
		}
		if commandResult.CycleID == "" {
			return errors.New("Scheduler receipt is missing its control cycle")
		}
		// Replaying the same request ID is observational. Never use it to resend
		// an interrupt or resume prompt whose transport result may be uncertain.
		if !receipt.Replayed {
			if commandResult.EffectID == "" {
				return errors.New("Scheduler receipt is missing its control effect")
			}
			if err := c.Dispatcher.DispatchEffect(ctx, commandResult.EffectID); err != nil {
				return err
			}
		}
		cycle, err := c.Store.SchedulerControlCycle(context.WithoutCancel(ctx), commandResult.CycleID)
		if err != nil {
			return err
		}
		result.Control = cycle
		return nil
	})
	if committed && c.After != nil {
		c.After()
	}
	return result, err
}

func schedulerCommandAction(command symphony.Command) string {
	switch command.(type) {
	case symphony.PauseScheduler:
		return "pause"
	case symphony.ResumeScheduler:
		return "resume"
	default:
		return ""
	}
}
