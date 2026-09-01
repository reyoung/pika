package provider

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"syscall"
	"time"
)

const (
	providerTextBusyRetryLimit = 8
	providerTextBusyRetryDelay = 15 * time.Millisecond
)

// providerCombinedOutput tolerates the short ETXTBSY window some overlay and
// networked filesystems expose immediately after an executable is atomically
// published. It retries only that kernel error, with a small bounded budget;
// command failures and context cancellation remain observable unchanged.
func providerCombinedOutput(ctx context.Context, executable string, arguments ...string) ([]byte, error) {
	return providerCombinedOutputInDir(ctx, "", executable, arguments...)
}

func providerCombinedOutputInDir(ctx context.Context, directory, executable string, arguments ...string) ([]byte, error) {
	stdout, _, err := providerCommandOutput(ctx, directory, executable, arguments, func(command *exec.Cmd) ([]byte, []byte, error) {
		output, err := command.CombinedOutput()
		return output, nil, err
	})
	return stdout, err
}

// providerOutputInDir keeps stdout machine-readable when a provider emits
// human diagnostics on stderr. It retains the same bounded ETXTBSY handling as
// providerCombinedOutputInDir.
func providerOutputInDir(ctx context.Context, directory, executable string, arguments ...string) ([]byte, []byte, error) {
	return providerCommandOutput(ctx, directory, executable, arguments, func(command *exec.Cmd) ([]byte, []byte, error) {
		var stdout bytes.Buffer
		var stderr bytes.Buffer
		command.Stdout = &stdout
		command.Stderr = &stderr
		err := command.Run()
		return stdout.Bytes(), stderr.Bytes(), err
	})
}

type providerCommandCapture func(*exec.Cmd) ([]byte, []byte, error)

func providerCommandOutput(ctx context.Context, directory, executable string, arguments []string, capture providerCommandCapture) ([]byte, []byte, error) {
	for attempt := 0; ; attempt++ {
		command := exec.CommandContext(ctx, executable, arguments...)
		command.Dir = directory
		stdout, stderr, err := capture(command)
		if !errors.Is(err, syscall.ETXTBSY) || attempt == providerTextBusyRetryLimit-1 || ctx.Err() != nil {
			return stdout, stderr, err
		}
		timer := time.NewTimer(providerTextBusyRetryDelay)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return stdout, stderr, ctx.Err()
		case <-timer.C:
		}
	}
}
