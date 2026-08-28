package fakeagent_test

import (
	"bytes"
	"context"
	"os"
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

func TestRunConsumesCodexInitialPrompt(t *testing.T) {
	t.Setenv("PIKA_CODEX_INITIAL_PROMPT", "start the assigned work")
	var output bytes.Buffer

	if code := fakeagent.Run(context.Background(), strings.NewReader("/exit\n"), &output); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if !strings.Contains(output.String(), `FAKE_AGENT_PROMPT "start the assigned work"`) {
		t.Fatalf("initial prompt was not consumed: %q", output.String())
	}
}

func TestRunInterruptKeepsSessionAliveForResumePrompt(t *testing.T) {
	interrupts := make(chan os.Signal, 1)
	interrupts <- os.Interrupt
	input := strings.NewReader("继续\n/exit\n")
	var output bytes.Buffer
	if code := fakeagent.RunWithInterrupts(context.Background(), input, &output, interrupts); code != 0 {
		t.Fatalf("exit code=%d", code)
	}
	if !strings.Contains(output.String(), "FAKE_AGENT_INTERRUPTED") || !strings.Contains(output.String(), `FAKE_AGENT_PROMPT "继续"`) {
		t.Fatalf("output=%q", output.String())
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
