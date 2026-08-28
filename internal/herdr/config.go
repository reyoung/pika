package herdr

import (
	"bufio"
	"bytes"
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
	return reloadConfiguration(ctx, client)
}

// ConfigureFreshSessions makes the one Herdr setting required by Pika explicit,
// reloads the running server, and restores the exact original file if reload fails.
func ConfigureFreshSessions(ctx context.Context, client *Client, configPath string) error {
	if client == nil {
		return errors.New("Herdr client is required")
	}
	if configPath == "" || !filepath.IsAbs(configPath) {
		return errors.New("Herdr config path must be absolute")
	}
	original, mode, existed, err := readConfiguration(configPath)
	if err != nil {
		return err
	}
	configured, err := renderFreshSessionConfiguration(original)
	if err != nil {
		return fmt.Errorf("update Herdr configuration %s: %w", configPath, err)
	}
	changed := !bytes.Equal(original, configured)
	if changed {
		if err := writeConfigurationAtomically(configPath, configured, mode); err != nil {
			return fmt.Errorf("update Herdr configuration %s: %w", configPath, err)
		}
	}
	if err := RequireFreshSessions(ctx, client, configPath); err == nil {
		return nil
	} else if !changed {
		return err
	} else {
		restoreErr := restoreConfiguration(configPath, original, mode, existed)
		reloadErr := reloadConfiguration(ctx, client)
		if restoreErr != nil || reloadErr != nil {
			return fmt.Errorf("%v; restore original configuration: %v; reload restored configuration: %v", err, restoreErr, reloadErr)
		}
		return fmt.Errorf("%w; original Herdr configuration restored", err)
	}
}

func reloadConfiguration(ctx context.Context, client *Client) error {
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

func readConfiguration(path string) ([]byte, os.FileMode, bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, 0o600, false, nil
	}
	if err != nil {
		return nil, 0, false, fmt.Errorf("inspect Herdr configuration %s: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, 0, false, fmt.Errorf("Herdr configuration %s is a symbolic link; update it manually", path)
	}
	if !info.Mode().IsRegular() {
		return nil, 0, false, fmt.Errorf("Herdr configuration %s is not a regular file", path)
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		return nil, 0, false, fmt.Errorf("read Herdr configuration %s: %w", path, err)
	}
	return contents, info.Mode().Perm(), true, nil
}

func renderFreshSessionConfiguration(contents []byte) ([]byte, error) {
	newline := "\n"
	if bytes.Contains(contents, []byte("\r\n")) {
		newline = "\r\n"
	}
	lines := strings.Split(string(contents), newline)
	section := ""
	sessionHeader := -1
	sessionEnd := len(lines)
	settingIndex := -1
	for index, line := range lines {
		semantic := strings.TrimSpace(strings.SplitN(line, "#", 2)[0])
		if strings.HasPrefix(semantic, "[") && strings.HasSuffix(semantic, "]") {
			if section == "session" && sessionEnd == len(lines) {
				sessionEnd = index
			}
			section = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(semantic, "["), "]"))
			if section == "session" {
				if sessionHeader >= 0 {
					return nil, errors.New("duplicate [session] sections require a manual update")
				}
				sessionHeader = index
			}
			continue
		}
		if section != "session" || semantic == "" {
			continue
		}
		key, _, found := strings.Cut(semantic, "=")
		if !found || strings.TrimSpace(key) != "resume_agents_on_restore" {
			continue
		}
		if settingIndex >= 0 {
			return nil, errors.New("duplicate session.resume_agents_on_restore settings require a manual update")
		}
		settingIndex = index
	}
	const setting = "resume_agents_on_restore = false"
	if settingIndex >= 0 {
		line := lines[settingIndex]
		indent := line[:len(line)-len(strings.TrimLeft(line, " \t"))]
		comment := ""
		if commentIndex := strings.IndexByte(line, '#'); commentIndex >= 0 {
			comment = " " + strings.TrimSpace(line[commentIndex:])
		}
		lines[settingIndex] = indent + setting + comment
		return []byte(strings.Join(lines, newline)), nil
	}
	if sessionHeader >= 0 {
		insertAt := sessionEnd
		for insertAt > sessionHeader+1 && strings.TrimSpace(lines[insertAt-1]) == "" {
			insertAt--
		}
		lines = insertLine(lines, insertAt, setting)
		return []byte(strings.Join(lines, newline)), nil
	}
	result := string(contents)
	if result != "" && !strings.HasSuffix(result, newline) {
		result += newline
	}
	if result != "" && !strings.HasSuffix(result, newline+newline) {
		result += newline
	}
	result += "[session]" + newline + setting + newline
	return []byte(result), nil
}

func insertLine(lines []string, index int, value string) []string {
	lines = append(lines, "")
	copy(lines[index+1:], lines[index:])
	lines[index] = value
	return lines
}

func writeConfigurationAtomically(path string, contents []byte, mode os.FileMode) error {
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(directory, ".pika-herdr-config-*.tmp")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer func() {
		_ = temporary.Close()
		_ = os.Remove(temporaryPath)
	}()
	if err := temporary.Chmod(mode); err != nil {
		return err
	}
	if _, err := temporary.Write(contents); err != nil {
		return err
	}
	if err := temporary.Sync(); err != nil {
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, path)
}

func restoreConfiguration(path string, contents []byte, mode os.FileMode, existed bool) error {
	if !existed {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	return writeConfigurationAtomically(path, contents, mode)
}
