package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/reyoung/pika-go/internal/fakeagent"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM)
	defer stop()
	interrupts := make(chan os.Signal, 1)
	signal.Notify(interrupts, os.Interrupt)
	defer signal.Stop(interrupts)
	os.Exit(fakeagent.RunWithInterrupts(ctx, os.Stdin, os.Stdout, interrupts))
}
