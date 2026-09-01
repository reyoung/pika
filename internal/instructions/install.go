package instructions

import (
	"embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

//go:embed defaults/*.md
var defaultFiles embed.FS

var catalog = map[string]string{
	"baseline":                  "baseline.md",
	"baseline-verify":           "baseline-verify.md",
	"diagnosis":                 "diagnosis.md",
	"iteration":                 "iteration.md",
	"integration":               "integration.md",
	"follow-up/baseline-verify": "follow-up-baseline-verify.md",
	"follow-up/diagnosis":       "follow-up-diagnosis.md",
	"follow-up/iteration":       "follow-up-iteration.md",
	"follow-up/integration":     "follow-up-integration.md",
}

func Install(root string) error {
	if root == "" || !filepath.IsAbs(root) {
		return errors.New("absolute instruction root is required")
	}
	for logicalName, embeddedName := range catalog {
		target := filepath.Join(root, filepath.FromSlash(logicalName)+".md")
		if _, err := os.Stat(target); err == nil {
			continue
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("inspect instruction %s: %w", logicalName, err)
		}
		contents, err := defaultFiles.ReadFile("defaults/" + embeddedName)
		if err != nil {
			return fmt.Errorf("read default instruction %s: %w", logicalName, err)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return fmt.Errorf("create instruction directory: %w", err)
		}
		if err := os.WriteFile(target, contents, 0o600); err != nil {
			return fmt.Errorf("install instruction %s: %w", logicalName, err)
		}
	}
	return nil
}

func Path(root, logicalName string) (string, error) {
	if _, ok := catalog[logicalName]; !ok {
		return "", fmt.Errorf("unknown instruction %q", logicalName)
	}
	return filepath.Join(root, filepath.FromSlash(logicalName)+".md"), nil
}

func LogicalNames() []string {
	names := make([]string, 0, len(catalog))
	for name := range catalog {
		names = append(names, name)
	}
	return names
}
