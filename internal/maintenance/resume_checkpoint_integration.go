//go:build integration

package maintenance

import (
	"context"
	"sync"
)

type integrationResumeCheckpoint func(context.Context, string) error

var integrationResumeCheckpoints struct {
	sync.RWMutex
	callback integrationResumeCheckpoint
}

func InstallIntegrationResumeCheckpoint(checkpoint func(context.Context, string) error) func() {
	integrationResumeCheckpoints.Lock()
	integrationResumeCheckpoints.callback = integrationResumeCheckpoint(checkpoint)
	integrationResumeCheckpoints.Unlock()
	return func() {
		integrationResumeCheckpoints.Lock()
		integrationResumeCheckpoints.callback = nil
		integrationResumeCheckpoints.Unlock()
	}
}

func reachIntegrationResumeCheckpoint(ctx context.Context, point string) error {
	integrationResumeCheckpoints.RLock()
	checkpoint := integrationResumeCheckpoints.callback
	integrationResumeCheckpoints.RUnlock()
	if checkpoint == nil {
		return nil
	}
	return checkpoint(ctx, point)
}
