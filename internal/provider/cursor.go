package provider

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const CursorCandidateVersion = "2026.08.31-4057e58"

const (
	cursorVersionProbeTimeout        = 15 * time.Second
	cursorAuthenticationProbeTimeout = 45 * time.Second
)

type CursorOptions struct {
	Executable     string
	RuntimeRoot    string
	InstanceBin    string
	PikaExecutable string
}

type CursorAdapter struct {
	options CursorOptions
}

func NewCursorAdapter(options ...CursorOptions) CursorAdapter {
	var selected CursorOptions
	if len(options) != 0 {
		selected = options[0]
	}
	return CursorAdapter{options: selected}
}

func (CursorAdapter) Kind() string { return "cursor" }

func (CursorAdapter) InterruptKeys() []string { return []string{"ctrl+c"} }

func (CursorAdapter) Validate(configuration AgentConfiguration) error {
	if configuration.Kind != "cursor" {
		return fmt.Errorf("Cursor Adapter cannot validate Agent kind %q", configuration.Kind)
	}
	if configuration.Model == "auto" {
		if configuration.ReasoningEffort != "" {
			return errors.New("Cursor auto-routing does not accept reasoning_effort")
		}
		return validateCursorArgs(configuration.Args)
	}
	if !agentValuePattern.MatchString(configuration.Model) {
		return errors.New("invalid Cursor model")
	}
	switch configuration.ReasoningEffort {
	case "low", "medium", "high", "xhigh", "max":
	default:
		return fmt.Errorf("invalid Cursor reasoning_effort %q", configuration.ReasoningEffort)
	}
	return validateCursorArgs(configuration.Args)
}

func (adapter CursorAdapter) Probe(ctx context.Context, request ProbeRequest) (Capabilities, error) {
	executable := request.Executable
	if executable == "" {
		executable = adapter.options.Executable
	}
	if executable == "" {
		var err error
		executable, err = exec.LookPath("cursor-agent")
		if err != nil {
			return Capabilities{}, fmt.Errorf("resolve Cursor executable: %w", err)
		}
	}
	versionCtx, cancelVersion := context.WithTimeout(ctx, cursorVersionProbeTimeout)
	output, err := providerCombinedOutput(versionCtx, executable, "--version")
	versionCtxErr := versionCtx.Err()
	cancelVersion()
	if err != nil {
		if versionCtxErr != nil {
			err = versionCtxErr
		}
		return Capabilities{}, fmt.Errorf("probe Cursor version: %w", err)
	}
	version := strings.TrimSpace(string(output))
	if version != CursorCandidateVersion {
		return Capabilities{}, fmt.Errorf("unsupported Cursor version %q; require %s", version, CursorCandidateVersion)
	}
	authenticationCtx, cancelAuthentication := context.WithTimeout(ctx, cursorAuthenticationProbeTimeout)
	output, err = providerCombinedOutput(authenticationCtx, executable, "status")
	authenticationCtxErr := authenticationCtx.Err()
	cancelAuthentication()
	if err != nil {
		status := strings.TrimSpace(string(output))
		cause := err
		if authenticationCtxErr != nil {
			cause = authenticationCtxErr
		}
		if status == "" {
			return Capabilities{}, fmt.Errorf("probe Cursor authentication: %w", cause)
		}
		return Capabilities{}, fmt.Errorf("probe Cursor authentication: %s: %w", status, cause)
	}
	status := strings.TrimSpace(string(output))
	if !strings.Contains(status, "Logged in") || strings.Contains(status, "Not logged in") {
		return Capabilities{}, fmt.Errorf("probe Cursor authentication: %s", status)
	}
	skillInjection := false
	if request.RequireSkillInjection {
		if err := probeCursorPluginDirectory(ctx, executable); err != nil {
			return Capabilities{}, err
		}
		skillInjection = true
	}
	return Capabilities{
		Kind: "cursor", Executable: executable, Version: version, Authenticated: true,
		Journal: true, TurnStop: true, FollowUp: true, FullOutput: true, FreshSession: true, Interrupt: true, SkillInjection: skillInjection, Compatible: true,
	}, nil
}

func probeCursorPluginDirectory(ctx context.Context, executable string) error {
	root, err := os.MkdirTemp("", "pika-cursor-plugin-")
	if err != nil {
		return fmt.Errorf("probe Cursor --plugin-dir: %w", err)
	}
	defer os.RemoveAll(root)
	manifest := []byte(`{"$schema":"https://agent-plugins.org/schemas/1.0.0/plugin.schema.json","name":"pika-kda-probe","description":"Pika non-model plugin discovery probe","version":"1.0.0","author":{"name":"pika-go"}}`)
	if err := os.WriteFile(filepath.Join(root, "plugin.json"), manifest, 0o600); err != nil {
		return fmt.Errorf("probe Cursor plugin manifest: %w", err)
	}
	for _, name := range []string{"KernelWiki", "ncu-report-skill"} {
		path := filepath.Join(root, "skills", name)
		if err := os.MkdirAll(path, 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(path, "SKILL.md"), []byte("---\nname: "+name+"\ndescription: Pika discovery probe\n---\n"), 0o600); err != nil {
			return err
		}
	}
	if err := validateCursorPluginDiscovery(root); err != nil {
		return fmt.Errorf("probe Cursor generated plugin: %w", err)
	}
	output, err := providerCombinedOutput(ctx, executable, "--plugin-dir", root, "--help")
	if err != nil {
		return fmt.Errorf("probe Cursor --plugin-dir manifest acceptance: %w: %s", err, strings.TrimSpace(string(output)))
	}
	if !strings.Contains(string(output), "--plugin-dir") {
		return errors.New("probe Cursor --plugin-dir: help output does not advertise local plugin support")
	}
	return nil
}

func validateCursorPluginDiscovery(root string) error {
	contents, err := os.ReadFile(filepath.Join(root, "plugin.json"))
	if err != nil {
		return err
	}
	var manifest struct {
		Schema      string `json:"$schema"`
		Name        string `json:"name"`
		Description string `json:"description"`
		Version     string `json:"version"`
		Author      struct {
			Name string `json:"name"`
		} `json:"author"`
	}
	decoder := json.NewDecoder(strings.NewReader(string(contents)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return fmt.Errorf("invalid plugin.json: %w", err)
	}
	if manifest.Schema != "https://agent-plugins.org/schemas/1.0.0/plugin.schema.json" ||
		manifest.Name == "" || manifest.Description == "" || manifest.Version == "" || manifest.Author.Name == "" {
		return errors.New("plugin.json identity is incomplete")
	}
	entries, err := os.ReadDir(filepath.Join(root, "skills"))
	if err != nil {
		return err
	}
	want := map[string]bool{"KernelWiki": true, "ncu-report-skill": true}
	if len(entries) != len(want) {
		return errors.New("plugin must expose exactly two skill directories")
	}
	for _, entry := range entries {
		if !entry.IsDir() || !want[entry.Name()] {
			return fmt.Errorf("unexpected plugin skill %s", entry.Name())
		}
		info, err := os.Stat(filepath.Join(root, "skills", entry.Name(), "SKILL.md"))
		if err != nil || info.IsDir() {
			return fmt.Errorf("plugin skill %s has no SKILL.md", entry.Name())
		}
	}
	return nil
}

func (adapter CursorAdapter) PrepareSession(_ context.Context, activation SessionActivation) (Launch, error) {
	if err := adapter.Validate(activation.Configuration); err != nil {
		return Launch{}, err
	}
	if len(activation.SystemPrompt) == 0 {
		return Launch{}, errors.New("frozen System Prompt is required")
	}
	if err := validateFrozenSkills(activation.FrozenSkills); err != nil {
		return Launch{}, err
	}
	if activation.AgentSessionID == "" || filepath.Base(activation.AgentSessionID) != activation.AgentSessionID || activation.AgentSessionID == "." {
		return Launch{}, errors.New("safe Pika Agent Session identity is required")
	}
	if activation.Repository == "" || !filepath.IsAbs(activation.Repository) {
		return Launch{}, errors.New("Cursor Session repository must be absolute")
	}
	if err := adapter.validateSessionOptions(); err != nil {
		return Launch{}, err
	}
	sessionDir := filepath.Join(adapter.options.RuntimeRoot, "cursor-sessions", activation.AgentSessionID)
	for _, directory := range []string{
		sessionDir,
		adapter.options.InstanceBin,
	} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			return Launch{}, fmt.Errorf("create Cursor Session directory: %w", err)
		}
		if err := os.Chmod(directory, 0o700); err != nil {
			return Launch{}, fmt.Errorf("protect Cursor Session directory: %w", err)
		}
	}
	arguments := append([]string{}, activation.Configuration.Args...)
	if activation.FrozenSkills != nil {
		arguments = append(arguments, "--plugin-dir", activation.FrozenSkills.RootPath)
	}
	files := map[string][]byte{
		filepath.Join(sessionDir, "system-prompt.md"): activation.SystemPrompt,
		filepath.Join(sessionDir, "cursor.args"):      []byte(strings.Join(arguments, "\n") + cursorArgsTerminator(arguments)),
	}
	for path, contents := range files {
		if err := writeProviderFile(path, contents, 0o600); err != nil {
			return Launch{}, err
		}
	}
	err := ensureProviderSymlink(
		filepath.Join(adapter.options.InstanceBin, "cursor-agent"),
		adapter.options.PikaExecutable,
	)
	if err != nil {
		return Launch{}, err
	}
	environment := make(map[string]string, len(activation.Environment)+6)
	for key, value := range activation.Environment {
		environment[key] = value
	}
	parameterizedModel := activation.Configuration.Model
	if activation.Configuration.ReasoningEffort != "" {
		parameterizedModel += "-" + activation.Configuration.ReasoningEffort
	}
	environment["PIKA_AGENT_MODEL"] = activation.Configuration.Model
	if activation.Configuration.ReasoningEffort != "" {
		environment["PIKA_AGENT_REASONING_EFFORT"] = activation.Configuration.ReasoningEffort
	}
	environment["PIKA_CURSOR_MODEL"] = parameterizedModel
	environment["PIKA_CURSOR_WORKSPACE"] = activation.Repository
	environment["PIKA_CURSOR_ARGS_FILE"] = filepath.Join(sessionDir, "cursor.args")
	handlesInitialPrompt := activation.InitialPrompt != ""
	if handlesInitialPrompt {
		environment["PIKA_CURSOR_INITIAL_PROMPT"] = activation.InitialPrompt
	}
	environment["PIKA_CURSOR_SYSTEM_PROMPT_PATH"] = filepath.Join(sessionDir, "system-prompt.md")
	environment["PIKA_GO_EXECUTABLE"] = adapter.options.PikaExecutable
	environment["PIKA_CURSOR_EXECUTABLE"] = adapter.options.Executable
	return Launch{AgentKind: "cursor", Environment: environment, EphemeralPath: sessionDir, StartupTimeout: 2 * time.Minute, HandlesInitialPrompt: handlesInitialPrompt, ReturnOnLaunch: handlesInitialPrompt, Cleanup: func() error {
		return CleanupCursorSession(adapter.options.RuntimeRoot, activation.AgentSessionID)
	}}, nil
}

// CleanupCursorSession removes only the private Cursor state owned by one safe
// Pika Agent Session. It is intentionally deterministic so cleanup can be repeated
// after a daemon restart, when the original Launch cleanup closure is gone.
func CleanupCursorSession(runtimeRoot, agentSessionID string) error {
	if runtimeRoot == "" || !filepath.IsAbs(runtimeRoot) {
		return errors.New("Cursor runtime root must be absolute")
	}
	if agentSessionID == "" || filepath.Base(agentSessionID) != agentSessionID || agentSessionID == "." {
		return errors.New("safe Pika Agent Session identity is required")
	}
	path := filepath.Join(runtimeRoot, "cursor-sessions", agentSessionID)
	if err := os.RemoveAll(path); err != nil {
		return fmt.Errorf("remove Cursor Session state: %w", err)
	}
	return nil
}

// ReconcileCursorSessions removes orphaned provider state while preserving the
// exact set of Agent Sessions that the durable store still considers active.
func ReconcileCursorSessions(runtimeRoot string, activeSessionIDs map[string]bool) error {
	if runtimeRoot == "" || !filepath.IsAbs(runtimeRoot) {
		return errors.New("Cursor runtime root must be absolute")
	}
	root := filepath.Join(runtimeRoot, "cursor-sessions")
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read Cursor Session state root: %w", err)
	}
	for _, entry := range entries {
		if activeSessionIDs[entry.Name()] {
			continue
		}
		if err := CleanupCursorSession(runtimeRoot, entry.Name()); err != nil {
			return err
		}
	}
	return nil
}

func (adapter CursorAdapter) validateSessionOptions() error {
	for label, path := range map[string]string{
		"Cursor executable": adapter.options.Executable, "Cursor runtime root": adapter.options.RuntimeRoot,
		"Cursor instance bin": adapter.options.InstanceBin, "pika-go executable": adapter.options.PikaExecutable,
	} {
		if path == "" || !filepath.IsAbs(path) {
			return fmt.Errorf("%s must be an absolute path", label)
		}
	}
	for label, path := range map[string]string{"Cursor executable": adapter.options.Executable, "pika-go executable": adapter.options.PikaExecutable} {
		info, err := os.Stat(path)
		if err != nil {
			return fmt.Errorf("inspect %s: %w", label, err)
		}
		if info.IsDir() || info.Mode().Perm()&0o111 == 0 {
			return fmt.Errorf("%s is not executable: %s", label, path)
		}
	}
	return nil
}

func cursorArgsTerminator(args []string) string {
	if len(args) == 0 {
		return ""
	}
	return "\n"
}

func writeProviderFile(path string, contents []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create provider file directory: %w", err)
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".pika-provider-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary provider file: %w", err)
	}
	temporaryPath := temporary.Name()
	defer func() {
		_ = temporary.Close()
		_ = os.Remove(temporaryPath)
	}()
	if _, err := temporary.Write(contents); err != nil {
		return err
	}
	if err := temporary.Sync(); err != nil {
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	// Never mark an inode executable while it still has a writable descriptor.
	// Linux rejects exec with ETXTBSY if publication races a lingering writer.
	if err := os.Chmod(temporaryPath, mode); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("commit provider file %s: %w", path, err)
	}
	return nil
}

func ensureProviderSymlink(path, target string) error {
	if existing, err := os.Readlink(path); err == nil && existing == target {
		return nil
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".pika-provider-link-*")
	if err != nil {
		return fmt.Errorf("reserve provider symlink: %w", err)
	}
	temporaryPath := temporary.Name()
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Remove(temporaryPath); err != nil {
		return err
	}
	defer os.Remove(temporaryPath)
	if err := os.Symlink(target, temporaryPath); err != nil {
		return fmt.Errorf("create provider symlink: %w", err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("commit provider symlink %s: %w", path, err)
	}
	return nil
}

func RunCursorWrapper(arguments []string, environment []string, stdin io.Reader, stdout, stderr io.Writer) int {
	values := make(map[string]string, len(environment))
	for _, entry := range environment {
		if separator := strings.IndexByte(entry, '='); separator >= 0 {
			values[entry[:separator]] = entry[separator+1:]
		}
	}
	for _, argument := range arguments {
		if argument == "--resume" || strings.HasPrefix(argument, "--resume=") ||
			argument == "--continue" || argument == "resume" || argument == "ls" {
			fmt.Fprintln(stderr, "pika-go: native Cursor resume is disabled")
			return 64
		}
	}
	argsPath, workspace, model, executable := values["PIKA_CURSOR_ARGS_FILE"], values["PIKA_CURSOR_WORKSPACE"], values["PIKA_CURSOR_MODEL"], values["PIKA_CURSOR_EXECUTABLE"]
	if argsPath == "" || workspace == "" || model == "" || executable == "" {
		fmt.Fprintln(stderr, "pika-go: incomplete Cursor launch environment")
		return 64
	}
	file, err := os.Open(argsPath)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	scanner := bufio.NewScanner(file)
	launchArguments := append([]string{}, arguments...)
	explicitSandbox := false
	for scanner.Scan() {
		argument := scanner.Text()
		if argument == "--yolo" || argument == "--sandbox" || strings.HasPrefix(argument, "--sandbox=") {
			explicitSandbox = true
		}
		launchArguments = append(launchArguments, argument)
	}
	closeErr := file.Close()
	if err := errors.Join(scanner.Err(), closeErr); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if !explicitSandbox {
		launchArguments = append(launchArguments, "--yolo")
	}
	launchArguments = append(launchArguments, "--workspace", workspace, "--model", model)
	if prompt := values["PIKA_CURSOR_INITIAL_PROMPT"]; prompt != "" {
		launchArguments = append(launchArguments, prompt)
	}
	command := exec.Command(executable, launchArguments...)
	command.Env = replaceEnvironmentValue(environment, "HERDR_AGENT", "cursor")
	command.Stdin, command.Stdout, command.Stderr = stdin, stdout, stderr
	if err := command.Run(); err != nil {
		if exit, ok := err.(*exec.ExitError); ok {
			return exit.ExitCode()
		}
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}

func replaceEnvironmentValue(environment []string, key, value string) []string {
	prefix := key + "="
	result := make([]string, 0, len(environment)+1)
	for _, entry := range environment {
		if !strings.HasPrefix(entry, prefix) {
			result = append(result, entry)
		}
	}
	return append(result, prefix+value)
}

type cursorHookEnvelope struct {
	ConversationID      string          `json:"conversation_id"`
	ConversationIDCamel string          `json:"conversationId"`
	SessionID           string          `json:"session_id"`
	SessionIDCamel      string          `json:"sessionId"`
	GenerationID        string          `json:"generation_id"`
	GenerationIDCamel   string          `json:"generationId"`
	HookEventName       string          `json:"hook_event_name"`
	HookEventNameCamel  string          `json:"hookEventName"`
	Prompt              string          `json:"prompt"`
	Text                string          `json:"text"`
	ToolName            string          `json:"tool_name"`
	ToolNameCamel       string          `json:"toolName"`
	ToolInput           json.RawMessage `json:"tool_input"`
	ToolInputCamel      json.RawMessage `json:"toolInput"`
	ToolOutput          json.RawMessage `json:"tool_output"`
	ToolOutputCamel     json.RawMessage `json:"toolOutput"`
	ToolUseID           string          `json:"tool_use_id"`
	ToolUseIDCamel      string          `json:"toolUseId"`
	ErrorMessage        string          `json:"error_message"`
	ErrorMessageCamel   string          `json:"errorMessage"`
	FailureType         string          `json:"failure_type"`
	FailureTypeCamel    string          `json:"failureType"`
	Duration            float64         `json:"duration"`
	IsInterrupt         bool            `json:"is_interrupt"`
	IsInterruptCamel    bool            `json:"isInterrupt"`
	Command             string          `json:"command"`
	Output              string          `json:"output"`
	Sandbox             bool            `json:"sandbox"`
	MCPServerName       string          `json:"mcp_server_name"`
	MCPServerNameCamel  string          `json:"mcpServerName"`
	ResultJSON          string          `json:"result_json"`
	ResultJSONCamel     string          `json:"resultJson"`
	Status              string          `json:"status"`
}

func (envelope *cursorHookEnvelope) canonicalize() {
	envelope.ConversationID = firstCursorValue(envelope.ConversationID, envelope.ConversationIDCamel)
	envelope.SessionID = firstCursorValue(envelope.SessionID, envelope.SessionIDCamel)
	envelope.GenerationID = firstCursorValue(envelope.GenerationID, envelope.GenerationIDCamel)
	envelope.HookEventName = firstCursorValue(envelope.HookEventName, envelope.HookEventNameCamel)
	envelope.ToolName = firstCursorValue(envelope.ToolName, envelope.ToolNameCamel)
	envelope.ToolUseID = firstCursorValue(envelope.ToolUseID, envelope.ToolUseIDCamel)
	envelope.ErrorMessage = firstCursorValue(envelope.ErrorMessage, envelope.ErrorMessageCamel)
	envelope.FailureType = firstCursorValue(envelope.FailureType, envelope.FailureTypeCamel)
	envelope.MCPServerName = firstCursorValue(envelope.MCPServerName, envelope.MCPServerNameCamel)
	envelope.ResultJSON = firstCursorValue(envelope.ResultJSON, envelope.ResultJSONCamel)
	if len(envelope.ToolInput) == 0 {
		envelope.ToolInput = envelope.ToolInputCamel
	}
	if len(envelope.ToolOutput) == 0 {
		envelope.ToolOutput = envelope.ToolOutputCamel
	}
	envelope.IsInterrupt = envelope.IsInterrupt || envelope.IsInterruptCamel
}

func firstCursorValue(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func (CursorAdapter) Normalize(binding SessionBinding, raw json.RawMessage) ([]JournalEvent, error) {
	if binding.AgentSessionID == "" || len(raw) == 0 {
		return nil, errors.New("Pika Agent Session and provider event are required")
	}
	var envelope cursorHookEnvelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, errors.New("provider hook envelope is invalid")
	}
	envelope.canonicalize()
	if envelope.HookEventName == "" {
		envelope.HookEventName = binding.HookEventName
	}
	if envelope.HookEventName == "" {
		return nil, errors.New("provider hook envelope is invalid")
	}
	providerSessionID := envelope.ConversationID
	if providerSessionID == "" {
		providerSessionID = envelope.SessionID
	}
	if providerSessionID == "" {
		return nil, errors.New("Cursor provider conversation identity is required")
	}
	event := JournalEvent{
		Provider:          "cursor",
		ProviderSessionID: providerSessionID,
		ProviderTurnID:    envelope.GenerationID,
		HookEventName:     envelope.HookEventName,
		Kind:              JournalObserved,
		Raw:               append(json.RawMessage(nil), raw...),
	}
	requireTurn := func() error {
		if envelope.GenerationID == "" {
			return fmt.Errorf("Cursor %s event requires generation_id", envelope.HookEventName)
		}
		return nil
	}
	switch envelope.HookEventName {
	case "sessionStart":
		event.Kind = JournalSessionStarted
	case "sessionEnd":
		event.Kind = JournalSessionEnded
		event.ProviderSessionEnded = true
	case "beforeSubmitPrompt":
		if err := requireTurn(); err != nil {
			return nil, err
		}
		event.Kind = JournalUserMessage
		event.UserMessage = envelope.Prompt
	case "afterAgentResponse":
		if err := requireTurn(); err != nil {
			return nil, err
		}
		event.Kind = JournalAssistantMessage
		event.AssistantMessage = envelope.Text
	case "postToolUse":
		if err := requireTurn(); err != nil {
			return nil, err
		}
		tool, err := cursorToolEvent(envelope, "completed")
		if err != nil {
			return nil, err
		}
		event.Kind, event.Tool = JournalToolCompleted, tool
	case "postToolUseFailure":
		if err := requireTurn(); err != nil {
			return nil, err
		}
		tool, err := cursorToolEvent(envelope, "failed")
		if err != nil {
			return nil, err
		}
		tool.ErrorMessage, tool.FailureType, tool.Interrupted = envelope.ErrorMessage, envelope.FailureType, envelope.IsInterrupt
		event.Kind, event.Tool = JournalToolFailed, tool
	case "afterShellExecution":
		if err := requireTurn(); err != nil {
			return nil, err
		}
		input, _ := json.Marshal(map[string]any{"command": envelope.Command, "sandbox": envelope.Sandbox})
		output, _ := json.Marshal(envelope.Output)
		event.Kind = JournalToolSupplement
		event.Supplement = &ToolSupplement{Kind: "shell", Name: "Shell", Input: input, Output: output, DurationMS: int64(envelope.Duration)}
	case "afterMCPExecution":
		if err := requireTurn(); err != nil {
			return nil, err
		}
		input, err := cursorJSONValue(envelope.ToolInput)
		if err != nil {
			return nil, errors.New("Cursor MCP tool input must be valid JSON")
		}
		output, err := cursorJSONStringValue(envelope.ResultJSON)
		if err != nil {
			return nil, errors.New("Cursor MCP result must be valid JSON")
		}
		event.Kind = JournalToolSupplement
		event.Supplement = &ToolSupplement{Kind: "mcp", Name: envelope.ToolName, ServerName: envelope.MCPServerName, Input: input, Output: output, DurationMS: int64(envelope.Duration)}
	case "stop":
		if err := requireTurn(); err != nil {
			return nil, err
		}
		switch envelope.Status {
		case "completed", "aborted", "error":
		default:
			return nil, fmt.Errorf("invalid Cursor stop status %q", envelope.Status)
		}
		event.Kind, event.TurnStatus = JournalTurnStopped, envelope.Status
	}
	return []JournalEvent{event}, nil
}

func cursorToolEvent(envelope cursorHookEnvelope, status string) (*ToolEvent, error) {
	if envelope.ToolUseID == "" || envelope.ToolName == "" || len(envelope.ToolInput) == 0 {
		return nil, fmt.Errorf("Cursor %s Tool identity and input are required", status)
	}
	input, err := cursorJSONValue(envelope.ToolInput)
	if err != nil {
		return nil, errors.New("Cursor tool input must be valid JSON")
	}
	var output json.RawMessage
	if len(envelope.ToolOutput) != 0 {
		output, err = cursorJSONValue(envelope.ToolOutput)
		if err != nil {
			return nil, errors.New("Cursor tool output must be valid JSON")
		}
	}
	return &ToolEvent{ID: envelope.ToolUseID, Name: envelope.ToolName, Input: input, Output: output, Status: status, DurationMS: int64(envelope.Duration)}, nil
}

func cursorJSONValue(raw json.RawMessage) (json.RawMessage, error) {
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, err
	}
	if encoded, ok := value.(string); ok {
		return cursorJSONStringValue(encoded)
	}
	return json.Marshal(value)
}

func cursorJSONStringValue(encoded string) (json.RawMessage, error) {
	if json.Valid([]byte(encoded)) {
		return canonicalJSON(json.RawMessage(encoded))
	}
	return json.Marshal(encoded)
}

func validateCursorArgs(args []string) error {
	for index := 0; index < len(args); index++ {
		argument := args[index]
		if argument == "" || strings.ContainsAny(argument, "\x00\r\n") {
			return fmt.Errorf("invalid Cursor argument %q", argument)
		}
		if argument == "--" || !strings.HasPrefix(argument, "-") {
			return fmt.Errorf("Cursor positional command or prompt %q is not allowed", argument)
		}
		name, inlineValue, hasInlineValue := strings.Cut(argument, "=")
		switch name {
		case "--force", "-f", "--yolo", "--auto-review", "--approve-mcps", "--trust", "--plan":
			if hasInlineValue {
				return fmt.Errorf("Cursor option %s does not take a value", name)
			}
		case "--api-key", "-H", "--header", "-e", "--endpoint", "--mode":
			value := inlineValue
			if !hasInlineValue {
				index++
				if index >= len(args) || args[index] == "" || strings.HasPrefix(args[index], "-") || strings.ContainsAny(args[index], "\x00\r\n") {
					return fmt.Errorf("Cursor option %s requires a value", name)
				}
				value = args[index]
			} else if inlineValue == "" {
				return fmt.Errorf("Cursor option %s requires a value", name)
			}
			if name == "--mode" && value != "plan" && value != "ask" {
				return fmt.Errorf("Cursor option --mode requires plan or ask, got %q", value)
			}
		case "--sandbox":
			value := inlineValue
			if !hasInlineValue {
				index++
				if index >= len(args) || strings.HasPrefix(args[index], "-") {
					return errors.New("Cursor option --sandbox requires a value")
				}
				value = args[index]
			}
			if value != "enabled" {
				return errors.New("Cursor sandbox may only be enabled")
			}
		case "--resume", "--continue", "--print", "-p", "--model", "--workspace", "--add-dir", "--plugin-dir",
			"--worktree", "-w", "--worktree-base", "--skip-worktree-setup", "--output-format", "--stream-partial-output",
			"--list-models", "--version", "-v", "--help", "-h":
			return fmt.Errorf("Cursor option %s is reserved by Pika", name)
		default:
			return fmt.Errorf("unsupported Cursor option %q for pinned version %s", name, CursorCandidateVersion)
		}
	}
	return nil
}
