package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

type CodexAdapter struct{}

func NewCodexAdapter() CodexAdapter { return CodexAdapter{} }

func (CodexAdapter) Kind() string { return "codex" }

func (CodexAdapter) InterruptKeys() []string { return []string{"ctrl+c"} }

var agentValuePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]*$`)

func (CodexAdapter) Validate(configuration AgentConfiguration) error {
	if configuration.Kind != "codex" {
		return fmt.Errorf("Codex Adapter cannot validate Agent kind %q", configuration.Kind)
	}
	if configuration.Model == "" && configuration.ReasoningEffort == "" {
		if len(configuration.Args) != 0 {
			return errors.New("Codex launch args are not supported")
		}
		return nil
	}
	if !agentValuePattern.MatchString(configuration.Model) {
		return errors.New("invalid Codex model")
	}
	switch configuration.ReasoningEffort {
	case "low", "medium", "high", "xhigh", "max", "ultra":
	default:
		return fmt.Errorf("invalid Codex reasoning_effort %q", configuration.ReasoningEffort)
	}
	if len(configuration.Args) != 0 {
		return errors.New("Codex launch args are not supported")
	}
	return nil
}

func (CodexAdapter) Probe(ctx context.Context, request ProbeRequest) (Capabilities, error) {
	executable := request.Executable
	if executable == "" {
		var err error
		executable, err = exec.LookPath("codex")
		if err != nil {
			return Capabilities{}, fmt.Errorf("resolve Codex executable: %w", err)
		}
	}
	output, err := providerCombinedOutput(ctx, executable, "--version")
	if err != nil {
		return Capabilities{}, fmt.Errorf("probe Codex version: %w", err)
	}
	version := strings.TrimSpace(string(output))
	if version == "" {
		return Capabilities{}, errors.New("probe Codex version: empty output")
	}
	if statusOutput, statusErr := providerCombinedOutput(ctx, executable, "login", "status"); statusErr != nil {
		return Capabilities{}, fmt.Errorf("probe Codex authentication: %s", strings.TrimSpace(string(statusOutput)))
	}
	skillInjection := false
	if request.RequireSkillInjection {
		if err := probeCodexSkillDiscovery(ctx, executable); err != nil {
			return Capabilities{}, err
		}
		skillInjection = true
	}
	return Capabilities{
		Kind: "codex", Executable: executable, Version: version, Authenticated: true,
		Journal: true, TurnStop: true, FollowUp: true, FullOutput: true, FreshSession: true, Interrupt: true, SkillInjection: skillInjection, Compatible: true,
	}, nil
}

func probeCodexSkillDiscovery(ctx context.Context, executable string) error {
	root, err := os.MkdirTemp("", "pika-codex-discovery-")
	if err != nil {
		return fmt.Errorf("probe Codex skill discovery: %w", err)
	}
	defer os.RemoveAll(root)
	repository := filepath.Join(root, "repository")
	if err := os.Mkdir(repository, 0o700); err != nil {
		return fmt.Errorf("probe Codex skill discovery: %w", err)
	}
	if output, err := exec.CommandContext(ctx, "git", "-C", repository, "init", "--quiet").CombinedOutput(); err != nil {
		return fmt.Errorf("probe Codex skill discovery repository: %w: %s", err, strings.TrimSpace(string(output)))
	}
	snapshot := FrozenSkillSnapshot{SnapshotID: "probe", RootPath: filepath.Join(root, "snapshot")}
	for _, item := range codexSkillMounts {
		path := filepath.Join(snapshot.RootPath, item.name)
		if err := os.MkdirAll(path, 0o700); err != nil {
			return err
		}
		contents := []byte("---\nname: " + item.name + "\ndescription: Pika discovery probe\n---\n")
		if err := os.WriteFile(filepath.Join(path, "SKILL.md"), contents, 0o600); err != nil {
			return err
		}
		snapshot.Skills = append(snapshot.Skills, FrozenSkillReference{Name: item.name, Path: path, CommitSHA: "probe", ContentSHA: "probe"})
	}
	cleanup, err := mountCodexSkills(repository, "probe", &snapshot)
	if err != nil {
		return fmt.Errorf("probe Codex skill discovery mount: %w", err)
	}
	defer cleanup()
	output, diagnostics, err := providerOutputInDir(ctx, repository, executable, "debug", "prompt-input")
	if err != nil {
		return fmt.Errorf("probe Codex skill discovery: %w: %s", err, strings.TrimSpace(strings.Join([]string{string(diagnostics), string(output)}, "\n")))
	}
	discovered, err := parseCodexSkillInstructions(output)
	if err != nil {
		if detail := strings.TrimSpace(string(diagnostics)); detail != "" {
			return fmt.Errorf("probe Codex skill discovery: %w (stderr: %s)", err, detail)
		}
		return fmt.Errorf("probe Codex skill discovery: %w", err)
	}
	for _, item := range codexSkillMounts {
		want := filepath.Join(snapshot.RootPath, item.name, "SKILL.md")
		resolvedWant, err := filepath.EvalSymlinks(want)
		if err != nil || discovered[item.name] != filepath.Clean(resolvedWant) {
			return fmt.Errorf("probe Codex skill discovery: reserved skill %s did not resolve to %s", item.name, filepath.Clean(resolvedWant))
		}
	}
	return nil
}

func parseCodexSkillInstructions(output []byte) (map[string]string, error) {
	var messages []struct {
		Type    string `json:"type"`
		Role    string `json:"role"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(output, &messages); err != nil {
		return nil, fmt.Errorf("decode debug prompt-input JSON messages: %w", err)
	}
	var instructions string
	for _, message := range messages {
		if message.Type != "message" || message.Role != "developer" {
			continue
		}
		for _, content := range message.Content {
			if content.Type != "input_text" {
				continue
			}
			if strings.Count(content.Text, "<skills_instructions>") > 1 || strings.Count(content.Text, "</skills_instructions>") > 1 {
				return nil, errors.New("skills_instructions block is missing, malformed, or ambiguous")
			}
			start := strings.Index(content.Text, "<skills_instructions>")
			end := strings.Index(content.Text, "</skills_instructions>")
			if start < 0 && end < 0 {
				continue
			}
			if start < 0 || end < start || instructions != "" {
				return nil, errors.New("skills_instructions block is missing, malformed, or ambiguous")
			}
			instructions = content.Text[start+len("<skills_instructions>") : end]
		}
	}
	if instructions == "" {
		return nil, errors.New("skills_instructions block is missing")
	}
	roots := map[string]string{}
	discovered := map[string]string{}
	section := ""
	for _, rawLine := range strings.Split(instructions, "\n") {
		line := strings.TrimSpace(rawLine)
		switch line {
		case "### Skill roots":
			section = "roots"
			continue
		case "### Available skills":
			section = "skills"
			continue
		}
		if !strings.HasPrefix(line, "- ") {
			continue
		}
		switch section {
		case "roots":
			parts := strings.Split(strings.TrimPrefix(line, "- "), "`")
			if len(parts) != 5 || strings.TrimSpace(parts[2]) != "=" {
				return nil, fmt.Errorf("malformed skill root line %q", line)
			}
			alias, root := parts[1], parts[3]
			if alias == "" || !filepath.IsAbs(root) {
				return nil, fmt.Errorf("invalid skill root %q", line)
			}
			if _, exists := roots[alias]; exists {
				return nil, fmt.Errorf("duplicate skill root alias %s", alias)
			}
			roots[alias] = filepath.Clean(root)
		case "skills":
			nameEnd := strings.Index(line, ":")
			fileStart := strings.LastIndex(line, "(file: ")
			if nameEnd < 2 || fileStart < 0 || !strings.HasSuffix(line, ")") {
				continue
			}
			name := strings.TrimSpace(line[2:nameEnd])
			if name != "KernelWiki" && name != "ncu-report-skill" {
				continue
			}
			if _, exists := discovered[name]; exists {
				return nil, fmt.Errorf("duplicate reserved skill %s", name)
			}
			reference := strings.TrimSpace(line[fileStart+len("(file: ") : len(line)-1])
			alias, relative, found := strings.Cut(reference, "/")
			root, rootFound := roots[alias]
			if !found || !rootFound || relative == "" || filepath.IsAbs(relative) {
				return nil, fmt.Errorf("invalid file reference for reserved skill %s", name)
			}
			candidate := filepath.Join(root, filepath.FromSlash(relative))
			within, err := filepath.Rel(root, candidate)
			if err != nil || within == ".." || strings.HasPrefix(within, ".."+string(filepath.Separator)) {
				return nil, fmt.Errorf("escaping file reference for reserved skill %s", name)
			}
			resolved, err := filepath.EvalSymlinks(candidate)
			if err != nil || !filepath.IsAbs(resolved) || filepath.Base(resolved) != "SKILL.md" {
				return nil, fmt.Errorf("invalid absolute SKILL.md path for reserved skill %s", name)
			}
			discovered[name] = filepath.Clean(resolved)
		}
	}
	for _, item := range codexSkillMounts {
		if discovered[item.name] == "" {
			return nil, fmt.Errorf("reserved skill %s is missing", item.name)
		}
	}
	return discovered, nil
}

func (adapter CodexAdapter) PrepareSession(_ context.Context, activation SessionActivation) (Launch, error) {
	if activation.Configuration.Kind != "codex" {
		return Launch{}, fmt.Errorf("Codex Adapter cannot prepare Agent kind %q", activation.Configuration.Kind)
	}
	if activation.Configuration.Model != "" || activation.Configuration.ReasoningEffort != "" || len(activation.Configuration.Args) != 0 {
		if err := adapter.Validate(activation.Configuration); err != nil {
			return Launch{}, err
		}
	}
	if len(activation.SystemPrompt) == 0 {
		return Launch{}, errors.New("frozen System Prompt is required")
	}
	cleanupSkills, err := mountCodexSkills(activation.Repository, activation.AgentSessionID, activation.FrozenSkills)
	if err != nil {
		return Launch{}, err
	}
	environment := make(map[string]string, len(activation.Environment)+4)
	for key, value := range activation.Environment {
		environment[key] = value
	}
	environment["PIKA_CODEX_DEVELOPER_INSTRUCTIONS_TOML"] = strconv.Quote(string(activation.SystemPrompt))
	if activation.Configuration.Model != "" {
		environment["PIKA_AGENT_MODEL"] = activation.Configuration.Model
	}
	if activation.Configuration.ReasoningEffort != "" {
		environment["PIKA_AGENT_REASONING_EFFORT"] = activation.Configuration.ReasoningEffort
	}
	handlesInitialPrompt := activation.InitialPrompt != ""
	if handlesInitialPrompt {
		environment["PIKA_CODEX_INITIAL_PROMPT"] = activation.InitialPrompt
	}
	return Launch{
		AgentKind:            "codex",
		Environment:          environment,
		HandlesInitialPrompt: handlesInitialPrompt,
		ReturnOnLaunch:       handlesInitialPrompt,
		Cleanup:              cleanupSkills,
	}, nil
}

type codexHookEnvelope struct {
	SessionID            string          `json:"session_id"`
	TurnID               string          `json:"turn_id"`
	HookEventName        string          `json:"hook_event_name"`
	Prompt               string          `json:"prompt"`
	LastAssistantMessage string          `json:"last_assistant_message"`
	ToolName             string          `json:"tool_name"`
	ToolUseID            string          `json:"tool_use_id"`
	ToolInput            json.RawMessage `json:"tool_input"`
	ToolResponse         json.RawMessage `json:"tool_response"`
}

func (CodexAdapter) Normalize(binding SessionBinding, raw json.RawMessage) ([]JournalEvent, error) {
	if binding.AgentSessionID == "" || len(raw) == 0 {
		return nil, errors.New("Pika Agent Session and provider event are required")
	}
	var envelope codexHookEnvelope
	if err := json.Unmarshal(raw, &envelope); err != nil || envelope.SessionID == "" || envelope.HookEventName == "" {
		return nil, errors.New("provider hook envelope is invalid")
	}
	canonical, err := canonicalJSON(raw)
	if err != nil {
		return nil, errors.New("provider hook envelope must be JSON")
	}
	event := JournalEvent{
		Provider:          "codex",
		ProviderSessionID: envelope.SessionID,
		ProviderTurnID:    envelope.TurnID,
		HookEventName:     envelope.HookEventName,
		Kind:              JournalObserved,
		Raw:               canonical,
	}
	switch envelope.HookEventName {
	case "SessionStart":
		event.Kind = JournalSessionStarted
	case "SessionEnd":
		event.Kind = JournalSessionEnded
		event.ProviderSessionEnded = true
	case "UserPromptSubmit":
		event.Kind = JournalUserMessage
		event.UserMessage = envelope.Prompt
	case "PostToolUse":
		if envelope.TurnID == "" || envelope.ToolUseID == "" || envelope.ToolName == "" || len(envelope.ToolInput) == 0 {
			return nil, errors.New("PostToolUse identity and input are required")
		}
		input, err := canonicalJSON(envelope.ToolInput)
		if err != nil {
			return nil, errors.New("tool input must be valid JSON")
		}
		var output json.RawMessage
		if len(envelope.ToolResponse) != 0 && string(envelope.ToolResponse) != "null" {
			output, err = canonicalJSON(envelope.ToolResponse)
			if err != nil {
				return nil, errors.New("tool response must be valid JSON")
			}
		}
		event.Kind = JournalToolCompleted
		event.Tool = &ToolEvent{ID: envelope.ToolUseID, Name: envelope.ToolName, Input: input, Output: output}
	case "Stop":
		event.Kind = JournalTurnStopped
		event.AssistantMessage = envelope.LastAssistantMessage
	}
	return []JournalEvent{event}, nil
}

func canonicalJSON(raw json.RawMessage) (json.RawMessage, error) {
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, err
	}
	return json.Marshal(value)
}
