package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/reyoung/pika-go/internal/fakeagent"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(fakeagent.Run(ctx, os.Stdin, os.Stdout))
}
