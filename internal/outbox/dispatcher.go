package outbox

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/reyoung/pika-go/internal/symphony"
)

type Store interface {
	ClaimPendingEffects(context.Context, int) ([]symphony.RuntimeEffect, error)
	MarkEffectDispatched(context.Context, string) error
}

type Sink interface {
	Dispatch(context.Context, symphony.RuntimeEffect) error
}

type Dispatcher struct {
	Store Store
	Sink  Sink
}

func (d Dispatcher) DispatchPending(ctx context.Context) error {
	if d.Store == nil || d.Sink == nil {
		return errors.New("outbox store and sink are required")
	}
	for {
		effects, err := d.Store.ClaimPendingEffects(ctx, 100)
		if err != nil {
			return err
		}
		if len(effects) == 0 {
			return nil
		}
		for _, effect := range effects {
			if err := d.Sink.Dispatch(ctx, effect); err != nil {
				return fmt.Errorf("dispatch runtime effect %s: %w", effect.ID, err)
			}
			if err := d.Store.MarkEffectDispatched(ctx, effect.ID); err != nil {
				return err
			}
		}
	}
}

type Recorder struct {
	mu      sync.Mutex
	effects []symphony.RuntimeEffect
}

func (r *Recorder) Dispatch(_ context.Context, effect symphony.RuntimeEffect) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.effects = append(r.effects, effect)
	return nil
}

func (r *Recorder) Effects() []symphony.RuntimeEffect {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]symphony.RuntimeEffect(nil), r.effects...)
}
