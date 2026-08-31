package activation_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reyoung/pika-go/internal/activation"
	"github.com/reyoung/pika-go/internal/instructions"
	"github.com/reyoung/pika-go/internal/symphony"
)

func TestPreparationFreezesInstructionForOneSession(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	instructionRoot := filepath.Join(t.TempDir(), "instructions")
	if err := instructions.Install(instructionRoot); err != nil {
		t.Fatal(err)
	}
	engine, err := symphony.Open(ctx, filepath.Join(t.TempDir(), "pika.db"), symphony.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine.Close() })
	if _, err := engine.Apply(ctx, symphony.Init{Meta: symphony.CommandMeta{RequestID: "init"}, OptimizationID: "optimization", Repository: "/repo"}); err != nil {
		t.Fatal(err)
	}
	view, _ := engine.Inspect(ctx, symphony.Status{})
	effects, _ := engine.PendingEffects(ctx, 1)
	session := symphony.AgentSession{ID: effects[0].ID, WorkID: view.Works[0].ID, Generation: 1, Role: symphony.RoleBaselineDraft, AgentKind: "codex", AgentName: "pika-freeze", Status: symphony.AgentSessionStarting}
	if err := engine.EnsureAgentSession(ctx, session); err != nil {
		t.Fatal(err)
	}
	work, err := engine.RuntimeWork(ctx, session.WorkID)
	if err != nil {
		t.Fatal(err)
	}
	preparer := activation.Preparer{Store: engine, InstructionRoot: instructionRoot, ContextsRoot: filepath.Join(t.TempDir(), "contexts"), SocketPath: "/tmp/pika.sock"}
	first, err := preparer.Prepare(ctx, session, work)
	if err != nil {
		t.Fatalf("first preparation: %v", err)
	}
	if !strings.Contains(first.Environment["PIKA_CODEX_DEVELOPER_INSTRUCTIONS_TOML"], "你是 Pika 的 Baseline Draft Agent") {
		t.Fatalf("immutable Role System Prompt was not delivered through Codex developer instructions: %+v", first.Environment)
	}
	if first.Prompt != "" || first.Environment["PIKA_CODEX_INITIAL_PROMPT"] != "" || first.ReturnOnLaunch {
		t.Fatalf("first Baseline Draft did not wait for operator input: %+v", first)
	}
	if strings.Contains(first.Environment["PIKA_CODEX_DEVELOPER_INSTRUCTIONS_TOML"], "用户追加 Instructions") {
		t.Fatalf("empty default instruction created an overlay: %s", first.Environment["PIKA_CODEX_DEVELOPER_INSTRUCTIONS_TOML"])
	}
	contextPath, messagesPath := first.Environment["PIKA_CONTEXT_PATH"], first.Environment["PIKA_MESSAGES_PATH"]
	if contextPath == "" || messagesPath == "" || first.Environment["PIKA_CONTEXT_SHA256"] == "" || first.Environment["PIKA_MESSAGES_SHA256"] == "" {
		t.Fatalf("Context Bundle environment is incomplete: %+v", first.Environment)
	}
	if _, err := os.Stat(contextPath); err != nil {
		t.Fatalf("context.json was not materialized: %v", err)
	}
	if _, err := os.Stat(messagesPath); err != nil {
		t.Fatalf("messages.jsonl was not materialized: %v", err)
	}
	for _, want := range []string{contextPath, messagesPath, "json-schema.org/draft/2020-12/schema", "完整读取"} {
		if !strings.Contains(first.Environment["PIKA_CODEX_DEVELOPER_INSTRUCTIONS_TOML"], want) {
			t.Fatalf("frozen System Prompt omits Context Bundle contract %q", want)
		}
	}
	if strings.Contains(first.Environment["PIKA_CODEX_DEVELOPER_INSTRUCTIONS_TOML"], "get_context") {
		t.Fatal("frozen System Prompt still references removed get_context")
	}
	path, _ := instructions.Path(instructionRoot, "baseline")
	if err := os.WriteFile(path, []byte("changed after session creation\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	retried, err := preparer.Prepare(ctx, session, work)
	if err != nil {
		t.Fatalf("retry preparation: %v", err)
	}
	if retried.Prompt != first.Prompt || retried.Environment["PIKA_CODEX_DEVELOPER_INSTRUCTIONS_TOML"] != first.Environment["PIKA_CODEX_DEVELOPER_INSTRUCTIONS_TOML"] || strings.Contains(retried.Environment["PIKA_CODEX_DEVELOPER_INSTRUCTIONS_TOML"], "changed after session creation") {
		t.Fatalf("System Prompt was not frozen: first=%q retried=%q", first.Environment["PIKA_CODEX_DEVELOPER_INSTRUCTIONS_TOML"], retried.Environment["PIKA_CODEX_DEVELOPER_INSTRUCTIONS_TOML"])
	}
	if retried.Environment["PIKA_MCP_GRANT"] == first.Environment["PIKA_MCP_GRANT"] {
		t.Fatal("retry did not rotate the raw grant")
	}
	if retried.Environment["PIKA_CONTEXT_PATH"] != contextPath || retried.Environment["PIKA_CONTEXT_SHA256"] != first.Environment["PIKA_CONTEXT_SHA256"] {
		t.Fatal("same Agent Session did not reuse its frozen Context Bundle")
	}
	freshSession := session
	freshSession.ID = "fresh-session"
	freshSession.Generation = 2
	freshSession.AgentName = "pika-fresh"
	if err := engine.EnsureAgentSession(ctx, freshSession); err != nil {
		t.Fatal(err)
	}
	freshWork := work
	freshWork.Work.Generation = 2
	fresh, err := preparer.Prepare(ctx, freshSession, freshWork)
	if err != nil {
		t.Fatalf("fresh session preparation: %v", err)
	}
	if !strings.Contains(fresh.Environment["PIKA_CODEX_DEVELOPER_INSTRUCTIONS_TOML"], "你是 Pika 的 Baseline Draft Agent") ||
		!strings.Contains(fresh.Environment["PIKA_CODEX_DEVELOPER_INSTRUCTIONS_TOML"], "## 用户追加 Instructions") ||
		!strings.Contains(fresh.Environment["PIKA_CODEX_DEVELOPER_INSTRUCTIONS_TOML"], "changed after session creation") {
		t.Fatalf("fresh session did not combine immutable System Prompt and current user instructions: %s", fresh.Environment["PIKA_CODEX_DEVELOPER_INSTRUCTIONS_TOML"])
	}
	if fresh.Prompt != "" || !strings.Contains(fresh.Environment["PIKA_CODEX_INITIAL_PROMPT"], "开始 Pika Work") || !fresh.ReturnOnLaunch {
		t.Fatalf("recovery Baseline Draft did not retain autonomous kickoff: %+v", fresh)
	}
	if fresh.Environment["PIKA_CONTEXT_PATH"] == contextPath {
		t.Fatal("fresh Agent Session reused the previous Session Context Bundle path")
	}
}

func TestPreparationInjectsStaticRoleModelSelection(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	instructionRoot := filepath.Join(root, "instructions")
	if err := instructions.Install(instructionRoot); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(root, "config.toml")
	if err := os.WriteFile(configPath, []byte(`[agents.baseline]
kind = "codex"
model = "gpt-role"
reasoning_effort = "xhigh"
`), 0o600); err != nil {
		t.Fatal(err)
	}
	engine, err := symphony.Open(ctx, filepath.Join(root, "pika.db"), symphony.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine.Close() })
	if _, err := engine.Apply(ctx, symphony.Init{Meta: symphony.CommandMeta{RequestID: "init"}, OptimizationID: "optimization", Repository: "/repo"}); err != nil {
		t.Fatal(err)
	}
	view, _ := engine.Inspect(ctx, symphony.Status{})
	session := symphony.AgentSession{ID: "session", WorkID: view.Works[0].ID, Generation: 1, Role: symphony.RoleBaselineDraft, AgentKind: "codex", AgentName: "pika-model", Status: symphony.AgentSessionStarting}
	if err := engine.EnsureAgentSession(ctx, session); err != nil {
		t.Fatal(err)
	}
	work, _ := engine.RuntimeWork(ctx, session.WorkID)
	prepared, err := (activation.Preparer{Store: engine, InstructionRoot: instructionRoot, ContextsRoot: filepath.Join(root, "contexts"), AgentConfigPath: configPath}).Prepare(ctx, session, work)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.Environment["PIKA_AGENT_MODEL"] != "gpt-role" || prepared.Environment["PIKA_AGENT_REASONING_EFFORT"] != "xhigh" {
		t.Fatalf("environment = %+v", prepared.Environment)
	}
}
