package provider_test

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
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
	executable := filepath.Join(t.TempDir(), "cursor-agent")
	script := `#!/bin/sh
	if [ "$1" = --list-models ]; then
	  printf '%s\n' \
	    'Available models' \
	    'auto - Auto (default)' \
	    'gpt-5.6-sol-low - GPT-5.6 Sol Low' \
    'gpt-5.6-sol-high - GPT-5.6 Sol High' \
    'gpt-5.6-sol-high-fast - GPT-5.6 Sol High Fast' \
    'gpt-5.5-extra-high - GPT-5.5 Extra High' \
    'gpt-5.6-terra-medium - GPT-5.6 Terra Medium'
  exit 0
fi
exit 1
`
	if err := os.WriteFile(executable, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
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
