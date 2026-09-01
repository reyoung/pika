package configuration

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reyoung/pika-go/internal/provider"
)

type recordingProbeAdapter struct {
	kind     string
	requests *[]provider.ProbeRequest
}

func (a recordingProbeAdapter) Kind() string { return a.kind }
func (a recordingProbeAdapter) Validate(configuration provider.AgentConfiguration) error {
	return nil
}
func (a recordingProbeAdapter) Probe(_ context.Context, request provider.ProbeRequest) (provider.Capabilities, error) {
	*a.requests = append(*a.requests, request)
	return provider.Capabilities{
		Kind: a.kind, Authenticated: true, Journal: true, TurnStop: true, FollowUp: true,
		FullOutput: true, FreshSession: true, Interrupt: true, SkillInjection: request.RequireSkillInjection, Compatible: true,
	}, nil
}
func (a recordingProbeAdapter) PrepareSession(context.Context, provider.SessionActivation) (provider.Launch, error) {
	return provider.Launch{}, nil
}
func (a recordingProbeAdapter) Normalize(provider.SessionBinding, json.RawMessage) ([]provider.JournalEvent, error) {
	return nil, nil
}

func TestProviderProbeRequiresSkillInjectionOnlyForCodingRoles(t *testing.T) {
	t.Parallel()
	repository := t.TempDir()
	config := strings.Replace(RenderDefaults(repository), `[agents.follow_up]
kind = "codex"`, `[agents.follow_up]
kind = "cursor"`, 1)
	path := filepath.Join(t.TempDir(), "pika.toml")
	if err := os.WriteFile(path, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	var codexRequests, cursorRequests []provider.ProbeRequest
	registry, err := provider.NewRegistry(
		recordingProbeAdapter{kind: "codex", requests: &codexRequests},
		recordingProbeAdapter{kind: "cursor", requests: &cursorRequests},
	)
	if err != nil {
		t.Fatal(err)
	}
	kinds, err := validateConfigurationPath(path, repository, registry)
	if err != nil {
		t.Fatal(err)
	}
	codingKinds, err := configuredCodingProviderKinds(path, registry)
	if err != nil {
		t.Fatal(err)
	}
	if err := probeProviders(context.Background(), registry, kinds, codingKinds, nil); err != nil {
		t.Fatal(err)
	}
	if len(codexRequests) != 1 || !codexRequests[0].RequireSkillInjection {
		t.Fatalf("coding Codex probe requests = %+v", codexRequests)
	}
	if len(cursorRequests) != 1 || cursorRequests[0].RequireSkillInjection {
		t.Fatalf("Follow-up-only Cursor probe requests = %+v", cursorRequests)
	}
}
