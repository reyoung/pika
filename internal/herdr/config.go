package herdr

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func ResolveConfigPath() (string, error) {
	path := os.Getenv("HERDR_CONFIG_PATH")
	if path == "" {
		if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
			path = filepath.Join(xdg, "herdr", "config.toml")
		} else {
			home, err := os.UserHomeDir()
			if err != nil {
				return "", fmt.Errorf("resolve Herdr config directory: %w", err)
			}
			path = filepath.Join(home, ".config", "herdr", "config.toml")
		}
	}
	if !filepath.IsAbs(path) {
		absolute, err := filepath.Abs(path)
		if err != nil {
			return "", fmt.Errorf("resolve Herdr config path: %w", err)
		}
		path = absolute
	}
	return filepath.Clean(path), nil
}

func ValidateFreshSessionConfiguration(path string) error {
	if path == "" || !filepath.IsAbs(path) {
		return errors.New("Herdr config path must be absolute")
	}
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("read Herdr configuration %s: %w", path, err)
	}
	defer file.Close()
	section := ""
	found := false
	scanner := bufio.NewScanner(file)
	for lineNumber := 1; scanner.Scan(); lineNumber++ {
		line := strings.TrimSpace(strings.SplitN(scanner.Text(), "#", 2)[0])
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(line, "["), "]"))
			continue
		}
		if section != "session" {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(key) != "resume_agents_on_restore" {
			continue
		}
		if found {
			return fmt.Errorf("Herdr configuration %s has duplicate session.resume_agents_on_restore", path)
		}
		found = true
		if strings.TrimSpace(value) != "false" {
			return fmt.Errorf("Herdr session.resume_agents_on_restore must be false in %s", path)
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read Herdr configuration %s: %w", path, err)
	}
	if !found {
		return fmt.Errorf("Herdr configuration %s must explicitly contain [session] resume_agents_on_restore = false", path)
	}
	return nil
}

func RequireFreshSessions(ctx context.Context, client *Client, configPath string) error {
	if client == nil {
		return errors.New("Herdr client is required")
	}
	if err := ValidateFreshSessionConfiguration(configPath); err != nil {
		return err
	}
	var result struct {
		Status      string   `json:"status"`
		Diagnostics []string `json:"diagnostics"`
	}
	if err := client.Call(ctx, "server.reload_config", map[string]any{}, &result); err != nil {
		return fmt.Errorf("reload Herdr configuration: %w", err)
	}
	if result.Status != "applied" {
		return fmt.Errorf("reload Herdr configuration returned %q: %s", result.Status, strings.Join(result.Diagnostics, "; "))
	}
	return nil
}
