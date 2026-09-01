//go:build !integration

package maintenance

import "context"

func reachIntegrationResumeCheckpoint(context.Context, string) error {
	return nil
}
