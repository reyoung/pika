package systemprompts_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/reyoung/pika-go/internal/symphony"
	"github.com/reyoung/pika-go/internal/systemprompts"
	"github.com/reyoung/pika-go/internal/toolapp"
)

func TestRoleSystemPromptsAreEmbeddedAndMatchCurrentContracts(t *testing.T) {
	t.Parallel()
	tests := []struct {
		logicalName string
		terminal    string
	}{
		{"baseline", "submit_baseline_definition"},
		{"baseline-verify", "finish_baseline_verification"},
		{"benchmark", "finish_iteration_benchmark"},
		{"iteration", "finish_iteration"},
		{"integration", "finish_integration"},
		{"follow-up/baseline-verify", "submit_followup_message"},
		{"follow-up/iteration", "submit_followup_message"},
		{"follow-up/integration", "submit_followup_message"},
	}
	for _, test := range tests {
		test := test
		t.Run(test.logicalName, func(t *testing.T) {
			t.Parallel()
			contents, err := systemprompts.Content(test.logicalName)
			if err != nil {
				t.Fatal(err)
			}
			prompt := string(contents)
			if strings.TrimSpace(prompt) == "" || !strings.Contains(prompt, test.terminal) {
				t.Fatalf("prompt does not contain current terminal contract %q: %q", test.terminal, prompt)
			}
			if strings.HasPrefix(test.logicalName, "follow-up/") {
				if !strings.Contains(prompt, "supersede") {
					t.Fatalf("Follow-up prompt omits user-activity supersession: %q", prompt)
				}
			} else if !strings.Contains(prompt, "用户可以直接") || !strings.Contains(prompt, "新建 Session") {
				t.Fatalf("core Role prompt omits direct steering or fresh-session recovery: %q", prompt)
			}
			if test.logicalName == "integration" && !strings.Contains(prompt, "apply_best_update") {
				t.Fatalf("Integration prompt omits scoped Best application MCP: %q", prompt)
			}
			if test.logicalName == "benchmark" {
				for _, requirement := range []string{"reference-only", "detached", "benchmark_context.evidence_root", "outcome=\"unavailable\"", "pika-go resume", "外部网络", "远程计算资源", "当前环境能力"} {
					if !strings.Contains(prompt, requirement) {
						t.Fatalf("Benchmark prompt omits requirement %q: %q", requirement, prompt)
					}
				}
			}
			if test.logicalName == "baseline" || test.logicalName == "baseline-verify" || test.logicalName == "iteration" || test.logicalName == "integration" {
				for _, requirement := range []string{"candidate_change_policy", "allowed_candidate_surface", "implementation allowlist", "冻结验证资产"} {
					if !strings.Contains(prompt, requirement) {
						t.Fatalf("prompt omits candidate change policy requirement %q: %q", requirement, prompt)
					}
				}
			}
			if test.logicalName == "integration" {
				for _, requirement := range []string{"每次 warm-up 和 measured invocation", "checked invocation 数", "NaN 或其他 sentinel", "接近或超过一个数量级", "首次调用、capture/compile/setup", `"benchmark_measurements"`, `"comparisons"`, `"reference"`, `"candidate"`, "全部冻结 Case", "全部冻结 Metric", `"performance_claim"`, `"independent_retest"`, `"max_case_speedup"`} {
					if !strings.Contains(prompt, requirement) {
						t.Fatalf("Integration prompt omits suspicious-speedup validation requirement %q: %q", requirement, prompt)
					}
				}
			}
			if test.logicalName == "baseline" {
				for _, requirement := range []string{"per-run integrity", "canonical input tensors", "每次 warm-up 和 measured invocation", "设备端累计", "端到端口径", `"schema_version": 1`, `"restore_inputs_before_every_invocation"`, `"canonical_inputs_candidate_visible": false`, `"benchmark_measurements"`, `"weight"`, `"role"`, `"direction"`, `"sample_statistic"`, `"aggregation"`} {
					if !strings.Contains(prompt, requirement) {
						t.Fatalf("Baseline prompt omits per-run benchmark integrity requirement %q: %q", requirement, prompt)
					}
				}
			}
			if test.logicalName == "baseline-verify" {
				for _, requirement := range []string{"canonical inputs", "checked invocation 数", "设备端累计紧凑统计", "端到端 metric", `"checked_invocations"`, `"canonical_input_mutations"`, `"tolerance_passed"`, `"benchmark_measurements"`, `"baseline"`, `"values"`, "全部冻结 Metric"} {
					if !strings.Contains(prompt, requirement) {
						t.Fatalf("Baseline Verification prompt omits per-run benchmark integrity requirement %q: %q", requirement, prompt)
					}
				}
			}
			if (test.logicalName == "baseline" || test.logicalName == "iteration") && !strings.Contains(prompt, "commit_changes") {
				t.Fatalf("write-capable Role prompt omits scoped commit MCP: %q", prompt)
			}
			if test.logicalName == "iteration" && (!strings.Contains(prompt, "MERGE_HEAD") || !strings.Contains(prompt, "staged/unmerged")) {
				t.Fatalf("Iteration prompt omits stale merge conflict recovery: %q", prompt)
			}
			if test.logicalName == "baseline" && (!strings.Contains(prompt, "Repository Snapshot SHA") || !strings.Contains(prompt, "自引用")) {
				t.Fatalf("Baseline Draft prompt does not separate the frozen repository snapshot from Definition identity: %q", prompt)
			}
			if test.logicalName == "baseline-verify" && (!strings.Contains(prompt, "Repository Snapshot SHA") || !strings.Contains(prompt, "Development Baseline")) {
				t.Fatalf("Baseline Verification prompt does not distinguish repository snapshot and Development Baseline: %q", prompt)
			}
			if test.logicalName == "baseline-verify" && (!strings.Contains(prompt, "后续 Candidate") || !strings.Contains(prompt, "建立基准测量")) {
				t.Fatalf("Baseline Verification prompt applies Candidate improvement gates to the Development Baseline: %q", prompt)
			}
			if test.logicalName == "baseline" || test.logicalName == "baseline-verify" || test.logicalName == "iteration" || test.logicalName == "integration" {
				for _, capability := range []string{"外部网络", "远程计算资源", "当前环境能力"} {
					if !strings.Contains(prompt, capability) {
						t.Fatalf("execution Role prompt omits external resource capability %q: %q", capability, prompt)
					}
				}
				for _, leakedEnvironmentDetail := range []string{"run-afs-process", "afs_cli", "H20", "nvidia-smi"} {
					if strings.Contains(prompt, leakedEnvironmentDetail) {
						t.Fatalf("execution Role prompt leaks environment-specific execution detail %q: %q", leakedEnvironmentDetail, prompt)
					}
				}
			}
			if test.logicalName == "follow-up/baseline-verify" && !strings.Contains(prompt, "不能要求 Development Baseline 达到后续 Candidate") {
				t.Fatalf("Baseline Verification Follow-up prompt can suggest an invalid Candidate gate: %q", prompt)
			}
			for _, obsolete := range []string{"ask_questions", "result_path", "PIKA_CANDIDATE_MANIFEST", "/schemas/"} {
				if strings.Contains(prompt, obsolete) {
					t.Fatalf("prompt contains obsolete contract %q", obsolete)
				}
			}
		})
	}
}

func TestRenderInjectsContextPathsDigestsSchemasAndOptionalInstructions(t *testing.T) {
	t.Parallel()
	files := systemprompts.ContextFiles{
		ContextPath: "/workspace/contexts/session/context.json", ContextSHA256: strings.Repeat("a", 64),
		MessagesPath: "/workspace/contexts/session/messages.jsonl", MessagesSHA256: strings.Repeat("b", 64),
		ContextSchema: []byte(`{"title":"context schema"}`), MessageSchema: []byte(`{"title":"message schema"}`), SummarySchema: []byte(`{"title":"summary schema"}`),
	}
	cursorPrompt, err := systemprompts.RenderForProvider("baseline", []byte("user overlay"), "cursor", files)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"context.json", files.ContextPath, files.ContextSHA256, files.MessagesPath, files.MessagesSHA256,
		"context schema", "message schema", "summary schema", "完整读取", "## 用户追加 Instructions", "user overlay",
		"`GetDynamicTools`，参数 `namespace` 设为 `pika_go`"} {
		if !strings.Contains(cursorPrompt, want) {
			t.Fatalf("rendered System Prompt omits %q:\n%s", want, cursorPrompt)
		}
	}
	if strings.Index(cursorPrompt, "## Cursor MCP bootstrap") > strings.Index(cursorPrompt, "## 用户追加 Instructions") {
		t.Fatalf("Cursor bootstrap was rendered as editable user Instructions:\n%s", cursorPrompt)
	}
	codexPrompt, err := systemprompts.RenderForProvider("baseline", nil, "codex", files)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(codexPrompt, "Cursor MCP bootstrap") || strings.Contains(codexPrompt, "GetDynamicTools") || strings.Contains(codexPrompt, "用户追加 Instructions") {
		t.Fatalf("Codex System Prompt contains Cursor-only bootstrap:\n%s", codexPrompt)
	}
}

func TestRenderRejectsIncompleteContextByFreezingFailureInstruction(t *testing.T) {
	t.Parallel()
	prompt, err := systemprompts.Render("iteration", nil, systemprompts.ContextFiles{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prompt, "Context Bundle 未完整物化；不得开始执行") {
		t.Fatalf("incomplete Context Bundle was not rejected in the prompt: %s", prompt)
	}
}

func TestBaselinePromptsDistinguishDurableRevisionFromExecutionWork(t *testing.T) {
	t.Parallel()
	draft, err := systemprompts.Content("baseline")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Baseline Revision ID", "不得把 Session ID 或 Work ID 写成 Definition 的持久身份", "上一版验证失败"} {
		if !strings.Contains(string(draft), want) {
			t.Fatalf("Baseline Draft prompt omits %q:\n%s", want, draft)
		}
	}
	verification, err := systemprompts.Content("baseline-verify")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Verification Work ID", "必然不同于", "不能作为 Definition 有效性判断", "不得在仓库中物化内联 Definition", "daemon 存储的 JSON 字节"} {
		if !strings.Contains(string(verification), want) {
			t.Fatalf("Baseline Verification prompt omits %q:\n%s", want, verification)
		}
	}
}

func TestPerformancePromptsSeparateIncrementalGateFromOverallGoal(t *testing.T) {
	t.Parallel()
	tests := []struct {
		role      string
		required  []string
		forbidden []string
	}{
		{role: "baseline", required: []string{"iteration_performance_gate", "单轮增量准入门槛", "测量噪声", "不得直接复制", "实际存在的已跟踪冻结验证资产"}},
		{role: "baseline-verify", required: []string{"前者比较 Candidate 和本轮 reference checkpoint", "后者比较当前 Best 和 Development Baseline", "不约束任一单个 Candidate", "应拒绝并要求修订"}, forbidden: []string{"门禁只约束后续 Candidate"}},
		{role: "iteration", required: []string{"当前 checkpoint", "单轮增量准入门槛", "整体性能目标只用于评估 Best 的累计进展"}},
		{role: "integration", required: []string{"Candidate 相对当前 Best", "超过噪声的渐进改善可以 Accept", "整体性能目标只用于累计进展和停止条件"}},
	}
	for _, test := range tests {
		prompt, err := systemprompts.Content(test.role)
		if err != nil {
			t.Fatal(err)
		}
		for _, phrase := range test.required {
			if !strings.Contains(string(prompt), phrase) {
				t.Errorf("%s prompt omits performance contract %q", test.role, phrase)
			}
		}
		for _, phrase := range test.forbidden {
			if strings.Contains(string(prompt), phrase) {
				t.Errorf("%s prompt retains conflated performance contract %q", test.role, phrase)
			}
		}
	}
}

func TestBaselinePromptExamplePassesDaemonContract(t *testing.T) {
	t.Parallel()
	prompt, err := systemprompts.Content("baseline")
	if err != nil {
		t.Fatal(err)
	}
	example := fencedJSONExample(t, string(prompt))
	if err := submitBaselineThroughDaemon(t, example); err != nil {
		t.Fatalf("Baseline prompt example was rejected by submit_baseline_definition: %v", err)
	}

	invalid := json.RawMessage(strings.Replace(string(example), `"minimum_aggregate_speedup": 1.01`, `"minimum_aggregate_speedup": 1`, 1))
	if string(invalid) == string(example) {
		t.Fatal("Baseline prompt example no longer contains the gate exercised by this test")
	}
	if err := submitBaselineThroughDaemon(t, invalid); err == nil {
		t.Fatal("submit_baseline_definition accepted a non-improving iteration gate")
	}
}

func fencedJSONExample(t *testing.T, prompt string) json.RawMessage {
	t.Helper()
	_, remainder, found := strings.Cut(prompt, "```json\n")
	if !found {
		t.Fatal("prompt has no JSON contract example")
	}
	example, _, found := strings.Cut(remainder, "\n```")
	if !found {
		t.Fatal("prompt has an unterminated JSON contract example")
	}
	return json.RawMessage(example)
}

func submitBaselineThroughDaemon(t *testing.T, definition json.RawMessage) error {
	t.Helper()
	ctx := context.Background()
	repository := filepath.Join(t.TempDir(), "repository")
	if err := os.MkdirAll(filepath.Join(repository, "tests"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repository, "tests", "correctness_test.py"), []byte("# frozen correctness asset\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runPromptTestGit(t, repository, "init", "--quiet", "--initial-branch=main")
	runPromptTestGit(t, repository, "add", "tests/correctness_test.py")
	runPromptTestGit(t, repository, "-c", "user.name=Pika Test", "-c", "user.email=pika@example.invalid", "commit", "--quiet", "-m", "baseline")

	engine, err := symphony.Open(ctx, filepath.Join(t.TempDir(), "pika.db"), symphony.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine.Close() })
	if _, err := engine.Apply(ctx, symphony.Init{Meta: symphony.CommandMeta{RequestID: "init"}, OptimizationID: "optimization", Repository: repository}); err != nil {
		t.Fatal(err)
	}
	view, err := engine.Inspect(ctx, symphony.Status{})
	if err != nil {
		t.Fatal(err)
	}
	work := view.Works[0]
	session := symphony.AgentSession{ID: "session", WorkID: work.ID, Generation: work.Generation, Role: work.Role, AgentKind: "codex", AgentName: "prompt-test", Status: symphony.AgentSessionStarting}
	if err := engine.EnsureAgentSession(ctx, session); err != nil {
		t.Fatal(err)
	}
	if err := engine.BindPane(ctx, session.ID, symphony.PaneBinding{WorkspaceID: "workspace", TabID: "tab", PaneID: "pane", TerminalID: "terminal"}); err != nil {
		t.Fatal(err)
	}
	grant, err := engine.MintAgentGrant(ctx, session.ID, toolapp.CatalogForRole(work.Role), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	arguments, err := json.Marshal(struct {
		IdempotencyKey string          `json:"idempotency_key"`
		Definition     json.RawMessage `json:"definition"`
	}{IdempotencyKey: "submit", Definition: definition})
	if err != nil {
		t.Fatal(err)
	}
	invocation, err := (toolapp.Application{Store: engine, WorktreeRoot: filepath.Join(t.TempDir(), "worktrees")}).Invoke(
		ctx, grant.Token, toolapp.Call{Name: "submit_baseline_definition", Arguments: arguments},
	)
	if err == nil && !invocation.Terminal {
		return fmt.Errorf("submit_baseline_definition was not terminal")
	}
	return err
}

func runPromptTestGit(t *testing.T, repository string, arguments ...string) {
	t.Helper()
	output, err := exec.Command("git", append([]string{"-C", repository}, arguments...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", arguments, err, output)
	}
}
