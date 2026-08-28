package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/reyoung/pika-go/internal/cli"
)

var version = "dev"
var updateFailBeforeReadyVersion string
var updateFailAfterCommitVersion string

func main() {
	cli.Version = version
	cli.UpdateFailBeforeReadyVersion = updateFailBeforeReadyVersion
	cli.UpdateFailAfterCommitVersion = updateFailAfterCommitVersion
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(cli.Run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}
