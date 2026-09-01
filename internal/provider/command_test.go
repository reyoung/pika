package provider

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestProviderCombinedOutputRetriesTransientExecutableTextBusy(t *testing.T) {
	executable := filepath.Join(t.TempDir(), "provider-probe")
	if err := os.WriteFile(executable, []byte("#!/bin/sh\nprintf ready\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	writer, err := os.OpenFile(executable, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	closed := make(chan error, 1)
	go func() {
		time.Sleep(30 * time.Millisecond)
		closed <- writer.Close()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	output, err := providerCombinedOutput(ctx, executable)
	if closeErr := <-closed; closeErr != nil {
		t.Fatal(closeErr)
	}
	if err != nil || string(output) != "ready" {
		t.Fatalf("provider probe output=%q err=%v", output, err)
	}
}

func TestProviderOutputInDirRetriesTransientExecutableTextBusy(t *testing.T) {
	executable := filepath.Join(t.TempDir(), "provider-probe")
	if err := os.WriteFile(executable, []byte("#!/bin/sh\nprintf ready\nprintf warning >&2\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	writer, err := os.OpenFile(executable, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	closed := make(chan error, 1)
	go func() {
		time.Sleep(30 * time.Millisecond)
		closed <- writer.Close()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	stdout, stderr, err := providerOutputInDir(ctx, "", executable)
	if closeErr := <-closed; closeErr != nil {
		t.Fatal(closeErr)
	}
	if err != nil || string(stdout) != "ready" || string(stderr) != "warning" {
		t.Fatalf("provider probe stdout=%q stderr=%q err=%v", stdout, stderr, err)
	}
}

func TestProviderOutputInDirKeepsFailureStreamsSeparate(t *testing.T) {
	executable := filepath.Join(t.TempDir(), "provider-probe")
	if err := os.WriteFile(executable, []byte("#!/bin/sh\nprintf machine\nprintf diagnostic >&2\nexit 7\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, err := providerOutputInDir(context.Background(), "", executable)
	if err == nil || string(stdout) != "machine" || string(stderr) != "diagnostic" {
		t.Fatalf("provider probe stdout=%q stderr=%q err=%v", stdout, stderr, err)
	}
}
