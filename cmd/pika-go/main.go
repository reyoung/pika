package main

import (
	"context"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/reyoung/pika-go/internal/cli"
	"github.com/reyoung/pika-go/internal/provider"
)

var version = "dev"
var updateFailBeforeReadyVersion string
var updateFailAfterCommitVersion string

func main() {
	if filepath.Base(os.Args[0]) == "cursor-agent" {
		os.Exit(provider.RunCursorWrapper(os.Args[1:], os.Environ(), os.Stdin, os.Stdout, os.Stderr))
	}
	cli.Version = version
	cli.UpdateFailBeforeReadyVersion = updateFailBeforeReadyVersion
	cli.UpdateFailAfterCommitVersion = updateFailAfterCommitVersion
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(cli.Run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}
