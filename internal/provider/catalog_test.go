package provider_test

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/reyoung/pika-go/internal/provider"
)

func TestCodexModelCatalogUsesOnlyVisibleSupportedChoices(t *testing.T) {
	root := t.TempDir()
	cache := `{"models":[
		{"slug":"gpt-visible","display_name":"GPT Visible","visibility":"list","supported_reasoning_levels":[{"effort":"low"},{"effort":"high"},{"effort":"illegal"}]},
		{"slug":"gpt-hidden","display_name":"GPT Hidden","visibility":"hide","supported_reasoning_levels":[{"effort":"high"}]}
	]}`
	if err := os.WriteFile(filepath.Join(root, "models_cache.json"), []byte(cache), 0o600); err != nil {
		t.Fatal(err)
	}
	registry, err := provider.NewRegistry(provider.NewCodexAdapter())
	if err != nil {
		t.Fatal(err)
	}
	models, err := registry.Models(context.Background(), "codex", provider.ModelRequest{ConfigRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	want := []provider.Model{
		{ID: "", DisplayName: "default", ReasoningEfforts: []string{}, Default: true},
		{ID: "gpt-visible", DisplayName: "GPT Visible", ReasoningEfforts: []string{"low", "high"}},
	}
	if !reflect.DeepEqual(models, want) {
		t.Fatalf("models = %+v, want %+v", models, want)
	}
}

func TestCursorModelCatalogDerivesOnlyLaunchableBaseAndEffortPairs(t *testing.T) {
	executable := cursorFixtureExecutable(t, "model-catalog")
	registry, err := provider.NewRegistry(provider.NewCursorAdapter())
	if err != nil {
		t.Fatal(err)
	}
	models, err := registry.Models(context.Background(), "cursor", provider.ModelRequest{Executable: executable})
	if err != nil {
		t.Fatal(err)
	}
	want := []provider.Model{
		{ID: "auto", DisplayName: "auto-routing", ReasoningEfforts: []string{}, Default: true},
		{ID: "gpt-5.6-sol", ReasoningEfforts: []string{"low", "high"}},
		{ID: "gpt-5.6-terra", ReasoningEfforts: []string{"medium"}},
	}
	if !reflect.DeepEqual(models, want) {
		t.Fatalf("models = %+v, want %+v", models, want)
	}
}

func TestValidateCatalogMembershipRejectsSyntacticallyValidUnavailablePair(t *testing.T) {
	models := []provider.Model{
		{ID: "auto", Default: true},
		{ID: "gpt-5.6-sol", ReasoningEfforts: []string{"high"}},
	}
	for _, test := range []struct {
		name   string
		config provider.AgentConfiguration
		want   string
	}{
		{name: "default accepts no effort", config: provider.AgentConfiguration{Kind: "cursor", Model: "auto"}},
		{name: "unavailable model", config: provider.AgentConfiguration{Kind: "cursor", Model: "gpt-5.6-luna", ReasoningEffort: "medium"}, want: "not available"},
		{name: "unavailable effort", config: provider.AgentConfiguration{Kind: "cursor", Model: "gpt-5.6-sol", ReasoningEffort: "medium"}, want: "does not offer"},
		{name: "forged effort on default", config: provider.AgentConfiguration{Kind: "cursor", Model: "auto", ReasoningEffort: "medium"}, want: "does not accept"},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := provider.ValidateCatalogMembership(models, test.config)
			if test.want == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("ValidateCatalogMembership(%+v) = %v, want %q", test.config, err, test.want)
			}
		})
	}
}
