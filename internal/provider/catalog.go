package provider

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type Model struct {
	ID               string
	DisplayName      string
	ReasoningEfforts []string
	Default          bool
}

type ModelRequest struct {
	Executable string
	ConfigRoot string
}

type modelLister interface {
	ListModels(context.Context, ModelRequest) ([]Model, error)
}

// ValidateCatalogMembership verifies a concrete model/effort pair against a
// freshly obtained provider catalog. Adapter.Validate intentionally checks
// only provider-independent syntax; it cannot establish that an account can
// launch a particular catalog entry.
func ValidateCatalogMembership(models []Model, configuration AgentConfiguration) error {
	for _, model := range models {
		if model.ID != configuration.Model {
			continue
		}
		if len(model.ReasoningEfforts) == 0 {
			if configuration.ReasoningEffort == "" {
				return nil
			}
			return fmt.Errorf("model %q uses provider-selected reasoning and does not accept reasoning_effort %q", configuration.Model, configuration.ReasoningEffort)
		}
		for _, effort := range model.ReasoningEfforts {
			if effort == configuration.ReasoningEffort {
				return nil
			}
		}
		return fmt.Errorf("model %q does not offer reasoning_effort %q in the provider catalog", configuration.Model, configuration.ReasoningEffort)
	}
	return fmt.Errorf("model %q is not available in the provider catalog", configuration.Model)
}

func (CodexAdapter) ListModels(_ context.Context, request ModelRequest) ([]Model, error) {
	if request.ConfigRoot == "" || !filepath.IsAbs(request.ConfigRoot) {
		return nil, errors.New("Codex config root must be absolute")
	}
	contents, err := os.ReadFile(filepath.Join(request.ConfigRoot, "models_cache.json"))
	if err != nil {
		return nil, fmt.Errorf("read Codex model catalog: %w", err)
	}
	var cache struct {
		Models []struct {
			Slug                     string `json:"slug"`
			DisplayName              string `json:"display_name"`
			Visibility               string `json:"visibility"`
			SupportedReasoningLevels []struct {
				Effort string `json:"effort"`
			} `json:"supported_reasoning_levels"`
		} `json:"models"`
	}
	if err := json.Unmarshal(contents, &cache); err != nil {
		return nil, fmt.Errorf("decode Codex model catalog: %w", err)
	}
	models := []Model{{ID: "", DisplayName: "default", ReasoningEfforts: []string{}, Default: true}}
	for _, cached := range cache.Models {
		if cached.Visibility != "" && cached.Visibility != "list" {
			continue
		}
		model := Model{ID: cached.Slug, DisplayName: cached.DisplayName}
		for _, level := range cached.SupportedReasoningLevels {
			candidate := AgentConfiguration{Kind: "codex", Model: cached.Slug, ReasoningEffort: level.Effort}
			if (CodexAdapter{}).Validate(candidate) == nil {
				model.ReasoningEfforts = appendUnique(model.ReasoningEfforts, level.Effort)
			}
		}
		if len(model.ReasoningEfforts) != 0 {
			models = append(models, model)
		}
	}
	return models, nil
}

func (adapter CursorAdapter) ListModels(ctx context.Context, request ModelRequest) ([]Model, error) {
	executable := request.Executable
	if executable == "" {
		executable = adapter.options.Executable
	}
	if executable == "" {
		var err error
		executable, err = exec.LookPath("cursor-agent")
		if err != nil {
			return nil, fmt.Errorf("resolve Cursor executable: %w", err)
		}
	}
	output, err := providerCombinedOutput(ctx, executable, "--list-models")
	if err != nil {
		return nil, fmt.Errorf("list Cursor models: %s", strings.TrimSpace(string(output)))
	}
	modelsByID := map[string]*Model{}
	var order []string
	scanner := bufio.NewScanner(strings.NewReader(string(output)))
	for scanner.Scan() {
		id, _, found := strings.Cut(strings.TrimSpace(scanner.Text()), " - ")
		if !found || strings.HasSuffix(id, "-fast") || strings.HasSuffix(id, "-extra-high") {
			continue
		}
		if id == "auto" {
			if modelsByID[id] == nil {
				modelsByID[id] = &Model{ID: id, DisplayName: "auto-routing", ReasoningEfforts: []string{}, Default: true}
				order = append(order, id)
			}
			continue
		}
		base, effort, found := splitParameterizedCursorModel(id)
		if !found {
			continue
		}
		candidate := AgentConfiguration{Kind: "cursor", Model: base, ReasoningEffort: effort}
		if adapter.Validate(candidate) != nil {
			continue
		}
		model := modelsByID[base]
		if model == nil {
			model = &Model{ID: base}
			modelsByID[base] = model
			order = append(order, base)
		}
		model.ReasoningEfforts = appendUnique(model.ReasoningEfforts, effort)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read Cursor model catalog: %w", err)
	}
	models := make([]Model, 0, len(order))
	if defaultModel := modelsByID["auto"]; defaultModel != nil {
		models = append(models, *defaultModel)
	}
	for _, id := range order {
		if id == "auto" {
			continue
		}
		models = append(models, *modelsByID[id])
	}
	if len(models) == 0 {
		return nil, errors.New("Cursor model catalog has no selectable models")
	}
	return models, nil
}

func splitParameterizedCursorModel(id string) (string, string, bool) {
	for _, effort := range []string{"low", "medium", "high", "xhigh", "max"} {
		suffix := "-" + effort
		if strings.HasSuffix(id, suffix) && len(id) > len(suffix) {
			return strings.TrimSuffix(id, suffix), effort, true
		}
	}
	return "", "", false
}

func appendUnique(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}
