//go:build !integration

package cli

import (
	"context"
	"flag"
)

type integrationResumeCheckpointFlags struct{}

func addIntegrationResumeCheckpointFlags(*flag.FlagSet) integrationResumeCheckpointFlags {
	return integrationResumeCheckpointFlags{}
}

func (integrationResumeCheckpointFlags) decorate(ctx context.Context) (context.Context, func(), error) {
	return ctx, func() {}, nil
}
