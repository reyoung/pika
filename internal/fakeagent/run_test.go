package fakeagent_test

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/reyoung/pika-go/internal/fakeagent"
)

func TestRunBecomesReadyAndAcceptsPrompts(t *testing.T) {
	input := strings.NewReader("investigate the kernel\n/exit\n")
	var output bytes.Buffer

	if code := fakeagent.Run(context.Background(), input, &output); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	want := "FAKE_AGENT_READY\nFAKE_AGENT_PROMPT \"investigate the kernel\"\n"
	if output.String() != want {
		t.Fatalf("output = %q, want %q", output.String(), want)
	}
}

func TestRunHonorsCancellableStartupDelay(t *testing.T) {
	t.Setenv("PIKA_GO_FAKE_AGENT_START_DELAY", "1h")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var output bytes.Buffer

	started := time.Now()
	if code := fakeagent.Run(ctx, strings.NewReader(""), &output); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("cancelled startup delay took %s", elapsed)
	}
	if output.Len() != 0 {
		t.Fatalf("cancelled Agent announced readiness: %q", output.String())
	}
}
