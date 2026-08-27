package configuration

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type Agent struct {
	Kind            string `json:"kind"`
	Model           string `json:"model"`
	ReasoningEffort string `json:"reasoning_effort"`
}

type Scheduler struct {
	IterationConcurrency int64 `json:"iteration_concurrency"`
	MaxPendingAttempts   int64 `json:"max_pending_attempts"`
}

type Identity struct {
	Version    int64  `json:"version"`
	Repository string `json:"repository"`
}

func LoadIdentity(path string) (Identity, error) {
	file, err := os.Open(path)
	if err != nil {
		return Identity{}, fmt.Errorf("open instance configuration: %w", err)
	}
	defer file.Close()
	section := ""
	var versionText, repositoryText string
	scanner := bufio.NewScanner(file)
	for lineNumber := 1; scanner.Scan(); lineNumber++ {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(line, "["), "]"))
			continue
		}
		key, encoded, found := strings.Cut(line, "=")
		if !found {
			if section == "" || section == "optimization" {
				return Identity{}, fmt.Errorf("invalid %s entry on line %d", identitySectionName(section), lineNumber)
			}
			continue
		}
		key, encoded = strings.TrimSpace(key), strings.TrimSpace(strings.SplitN(encoded, "#", 2)[0])
		switch section {
		case "":
			if key != "version" {
				return Identity{}, fmt.Errorf("unknown root configuration field %q", key)
			}
			if versionText != "" {
				return Identity{}, errors.New("duplicate root configuration field \"version\"")
			}
			versionText = encoded
		case "optimization":
			if key != "repository" {
				return Identity{}, fmt.Errorf("unknown optimization field %q", key)
			}
			if repositoryText != "" {
				return Identity{}, errors.New("duplicate optimization field \"repository\"")
			}
			repositoryText = encoded
		}
	}
	if err := scanner.Err(); err != nil {
		return Identity{}, fmt.Errorf("read instance configuration: %w", err)
	}
	version, err := strconv.ParseInt(versionText, 10, 64)
	if err != nil || version != 1 {
		return Identity{}, fmt.Errorf("unsupported configuration version %q", versionText)
	}
	repository, err := strconv.Unquote(repositoryText)
	if err != nil || repository == "" || !filepath.IsAbs(repository) {
		return Identity{}, errors.New("optimization.repository must be an absolute quoted path")
	}
	return Identity{Version: version, Repository: filepath.Clean(repository)}, nil
}

func identitySectionName(section string) string {
	if section == "" {
		return "root configuration"
	}
	return section
}

func LoadScheduler(path string) (Scheduler, error) {
	file, err := os.Open(path)
	if err != nil {
		return Scheduler{}, fmt.Errorf("open instance configuration: %w", err)
	}
	defer file.Close()
	section := ""
	values := map[string]string{}
	scanner := bufio.NewScanner(file)
	for lineNumber := 1; scanner.Scan(); lineNumber++ {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(line, "["), "]"))
			continue
		}
		if section != "scheduler" {
			continue
		}
		key, encoded, found := strings.Cut(line, "=")
		if !found {
			return Scheduler{}, fmt.Errorf("invalid scheduler entry on line %d", lineNumber)
		}
		key, encoded = strings.TrimSpace(key), strings.TrimSpace(strings.SplitN(encoded, "#", 2)[0])
		if key != "iteration_concurrency" && key != "max_pending_attempts" {
			return Scheduler{}, fmt.Errorf("unknown scheduler field %q", key)
		}
		if _, duplicate := values[key]; duplicate {
			return Scheduler{}, fmt.Errorf("duplicate scheduler field %q", key)
		}
		values[key] = encoded
	}
	if err := scanner.Err(); err != nil {
		return Scheduler{}, fmt.Errorf("read instance configuration: %w", err)
	}
	if len(values) != 2 {
		return Scheduler{}, errors.New("scheduler requires iteration_concurrency and max_pending_attempts")
	}
	concurrency, err := positiveInt(values["iteration_concurrency"], "scheduler.iteration_concurrency")
	if err != nil {
		return Scheduler{}, err
	}
	maxPending, err := positiveInt(values["max_pending_attempts"], "scheduler.max_pending_attempts")
	if err != nil {
		return Scheduler{}, err
	}
	return Scheduler{IterationConcurrency: concurrency, MaxPendingAttempts: maxPending}, nil
}

type FollowUpPolicy struct {
	MaxMessages          int64 `json:"max_messages"`
	GeneratorMaxAttempts int64 `json:"generator_max_attempts"`
}

type FollowUp struct {
	PaneIdleTimeout      time.Duration  `json:"pane_idle_timeout"`
	GeneratorConcurrency int64          `json:"generator_concurrency"`
	BaselineVerify       FollowUpPolicy `json:"baseline_verify"`
	Iteration            FollowUpPolicy `json:"iteration"`
	Integration          FollowUpPolicy `json:"integration"`
}

func LoadFollowUp(path string) (FollowUp, error) {
	file, err := os.Open(path)
	if err != nil {
		return FollowUp{}, fmt.Errorf("open instance configuration: %w", err)
	}
	defer file.Close()
	sections := map[string]map[string]string{}
	section := ""
	scanner := bufio.NewScanner(file)
	for lineNumber := 1; scanner.Scan(); lineNumber++ {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(line, "["), "]"))
			continue
		}
		if section != "follow_up" && !strings.HasPrefix(section, "follow_up.") {
			continue
		}
		key, encoded, found := strings.Cut(line, "=")
		if !found {
			return FollowUp{}, fmt.Errorf("invalid %s entry on line %d", section, lineNumber)
		}
		key, encoded = strings.TrimSpace(key), strings.TrimSpace(strings.SplitN(encoded, "#", 2)[0])
		if sections[section] == nil {
			sections[section] = map[string]string{}
		}
		if _, duplicate := sections[section][key]; duplicate {
			return FollowUp{}, fmt.Errorf("duplicate %s field %q", section, key)
		}
		sections[section][key] = encoded
	}
	if err := scanner.Err(); err != nil {
		return FollowUp{}, fmt.Errorf("read instance configuration: %w", err)
	}
	global := sections["follow_up"]
	if len(global) != 2 {
		return FollowUp{}, errors.New("follow_up requires pane_idle_timeout and generator_concurrency")
	}
	for key := range global {
		if key != "pane_idle_timeout" && key != "generator_concurrency" {
			return FollowUp{}, fmt.Errorf("unknown follow_up field %q", key)
		}
	}
	timeoutText, err := strconv.Unquote(global["pane_idle_timeout"])
	if err != nil {
		return FollowUp{}, errors.New("follow_up.pane_idle_timeout must be a quoted duration")
	}
	timeout, err := time.ParseDuration(timeoutText)
	if err != nil || timeout <= 0 {
		return FollowUp{}, fmt.Errorf("invalid follow_up.pane_idle_timeout %q", timeoutText)
	}
	concurrency, err := positiveInt(global["generator_concurrency"], "follow_up.generator_concurrency")
	if err != nil {
		return FollowUp{}, err
	}
	if concurrency != 1 {
		return FollowUp{}, errors.New("follow_up.generator_concurrency must be 1 in the first release")
	}
	baseline, err := parseFollowUpPolicy(sections["follow_up.baseline_verify"], "follow_up.baseline_verify")
	if err != nil {
		return FollowUp{}, err
	}
	iteration, err := parseFollowUpPolicy(sections["follow_up.iteration"], "follow_up.iteration")
	if err != nil {
		return FollowUp{}, err
	}
	integration, err := parseFollowUpPolicy(sections["follow_up.integration"], "follow_up.integration")
	if err != nil {
		return FollowUp{}, err
	}
	return FollowUp{PaneIdleTimeout: timeout, GeneratorConcurrency: concurrency, BaselineVerify: baseline, Iteration: iteration, Integration: integration}, nil
}

func parseFollowUpPolicy(values map[string]string, section string) (FollowUpPolicy, error) {
	if len(values) != 2 {
		return FollowUpPolicy{}, fmt.Errorf("%s requires max_messages and generator_max_attempts", section)
	}
	for key := range values {
		if key != "max_messages" && key != "generator_max_attempts" {
			return FollowUpPolicy{}, fmt.Errorf("unknown %s field %q", section, key)
		}
	}
	messages, err := positiveInt(values["max_messages"], section+".max_messages")
	if err != nil {
		return FollowUpPolicy{}, err
	}
	attempts, err := positiveInt(values["generator_max_attempts"], section+".generator_max_attempts")
	if err != nil {
		return FollowUpPolicy{}, err
	}
	return FollowUpPolicy{MaxMessages: messages, GeneratorMaxAttempts: attempts}, nil
}

func positiveInt(encoded, name string) (int64, error) {
	value, err := strconv.ParseInt(encoded, 10, 64)
	if err != nil || value <= 0 {
		return 0, fmt.Errorf("%s must be a positive integer", name)
	}
	return value, nil
}

func LoadPaneIdleTimeout(path string) (time.Duration, error) {
	file, err := os.Open(path)
	if err != nil {
		return 0, fmt.Errorf("open instance configuration: %w", err)
	}
	defer file.Close()
	section := ""
	scanner := bufio.NewScanner(file)
	for lineNumber := 1; scanner.Scan(); lineNumber++ {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(line, "["), "]"))
			continue
		}
		if section != "follow_up" {
			continue
		}
		key, encoded, found := strings.Cut(line, "=")
		if !found {
			return 0, fmt.Errorf("invalid follow_up entry on line %d", lineNumber)
		}
		key = strings.TrimSpace(key)
		if key != "pane_idle_timeout" {
			continue
		}
		value, err := strconv.Unquote(strings.TrimSpace(strings.SplitN(encoded, "#", 2)[0]))
		if err != nil {
			return 0, errors.New("follow_up.pane_idle_timeout must be a quoted duration")
		}
		duration, err := time.ParseDuration(value)
		if err != nil || duration <= 0 {
			return 0, fmt.Errorf("invalid follow_up.pane_idle_timeout %q", value)
		}
		return duration, nil
	}
	if err := scanner.Err(); err != nil {
		return 0, fmt.Errorf("read instance configuration: %w", err)
	}
	return 0, errors.New("missing follow_up.pane_idle_timeout")
}

var agentValuePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]*$`)

func LoadAgent(path, role string) (Agent, error) {
	if path == "" || role == "" {
		return Agent{}, errors.New("configuration path and role are required")
	}
	file, err := os.Open(path)
	if err != nil {
		return Agent{}, fmt.Errorf("open instance configuration: %w", err)
	}
	defer file.Close()
	targetSection := "agents." + role
	section := ""
	values := map[string]string{}
	scanner := bufio.NewScanner(file)
	for lineNumber := 1; scanner.Scan(); lineNumber++ {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(line, "["), "]"))
			continue
		}
		if section != targetSection {
			continue
		}
		key, encoded, found := strings.Cut(line, "=")
		if !found {
			return Agent{}, fmt.Errorf("invalid %s entry on line %d", targetSection, lineNumber)
		}
		key = strings.TrimSpace(key)
		if key != "kind" && key != "model" && key != "reasoning_effort" {
			return Agent{}, fmt.Errorf("unknown %s field %q", targetSection, key)
		}
		if _, duplicate := values[key]; duplicate {
			return Agent{}, fmt.Errorf("duplicate %s field %q", targetSection, key)
		}
		value, err := strconv.Unquote(strings.TrimSpace(strings.SplitN(encoded, "#", 2)[0]))
		if err != nil {
			return Agent{}, fmt.Errorf("%s.%s must be a quoted string", targetSection, key)
		}
		values[key] = value
	}
	if err := scanner.Err(); err != nil {
		return Agent{}, fmt.Errorf("read instance configuration: %w", err)
	}
	config := Agent{Kind: values["kind"], Model: values["model"], ReasoningEffort: values["reasoning_effort"]}
	if config.Kind == "" && config.Model == "" && config.ReasoningEffort == "" {
		return Agent{}, fmt.Errorf("missing [%s] configuration", targetSection)
	}
	if config.Kind != "codex" {
		return Agent{}, fmt.Errorf("unsupported Agent kind %q for role %s", config.Kind, role)
	}
	if !agentValuePattern.MatchString(config.Model) {
		return Agent{}, fmt.Errorf("invalid model for role %s", role)
	}
	switch config.ReasoningEffort {
	case "low", "medium", "high", "xhigh", "max", "ultra":
	default:
		return Agent{}, fmt.Errorf("invalid reasoning_effort %q for role %s", config.ReasoningEffort, role)
	}
	return config, nil
}
