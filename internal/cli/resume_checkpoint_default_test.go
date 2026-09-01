//go:build !integration

package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestDefaultDaemonBuildDoesNotExposeResumeCheckpoints(t *testing.T) {
	var help bytes.Buffer
	if code := runDaemon(context.Background(), []string{"--help"}, &help); code != 0 {
		t.Fatalf("daemon --help exit=%d output=%s", code, help.String())
	}
	if strings.Contains(help.String(), "maintenance-resume-checkpoint") {
		t.Fatalf("production help exposes integration checkpoint:\n%s", help.String())
	}

	var rejected bytes.Buffer
	if code := runDaemon(context.Background(), []string{"--maintenance-resume-checkpoint", "intent_persisted"}, &rejected); code != 2 {
		t.Fatalf("production checkpoint flag exit=%d output=%s", code, rejected.String())
	}
	if !strings.Contains(rejected.String(), "flag provided but not defined") {
		t.Fatalf("production checkpoint flag was not rejected:\n%s", rejected.String())
	}
}
