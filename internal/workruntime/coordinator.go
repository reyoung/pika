package workruntime

import (
	"context"
	"errors"
	"sync"
	"time"
)

type Dispatcher interface {
	DispatchPending(context.Context) error
}

type UncertainEffectStore interface {
	RecoverUncertainEffects(context.Context) error
}

type ChangeWatcher interface {
	Watch(context.Context, func(context.Context, Snapshot) error) error
}

type Coordinator struct {
	Reconciler   Reconciler
	Dispatcher   Dispatcher
	EffectStore  UncertainEffectStore
	Runtime      Runtime
	RetryDelay   time.Duration
	OnWatchError func(error)
	mu           sync.Mutex
}

func (c *Coordinator) RecoverAndDispatch(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.Dispatcher == nil || c.EffectStore == nil {
		return errors.New("runtime dispatcher and uncertain-effect store are required")
	}
	if err := c.EffectStore.RecoverUncertainEffects(ctx); err != nil {
		return err
	}
	if err := c.Reconciler.Reconcile(ctx); err != nil {
		return err
	}
	return c.Dispatcher.DispatchPending(ctx)
}

func (c *Coordinator) Run(ctx context.Context) error {
	watcher, ok := c.Runtime.(ChangeWatcher)
	if !ok {
		return errors.New("work runtime does not support change watching")
	}
	delay := c.RetryDelay
	if delay <= 0 {
		delay = 500 * time.Millisecond
	}
	for {
		err := watcher.Watch(ctx, func(callbackCtx context.Context, snapshot Snapshot) error {
			c.mu.Lock()
			defer c.mu.Unlock()
			// A runtime change can resolve an interactive or otherwise partial
			// effect (for example, the user accepting an Agent trust prompt).
			// Reclassify every prior dispatch as uncertain before reconciling the
			// new snapshot so the idempotent Sink can finish it without requiring
			// a daemon restart.
			if err := c.EffectStore.RecoverUncertainEffects(callbackCtx); err != nil {
				return err
			}
			if err := c.Reconciler.ReconcileSnapshot(callbackCtx, snapshot); err != nil {
				return err
			}
			return c.Dispatcher.DispatchPending(callbackCtx)
		})
		if ctx.Err() != nil {
			return nil
		}
		if c.OnWatchError != nil {
			c.OnWatchError(err)
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return nil
		case <-timer.C:
		}
	}
}
