package systemprompts_test

import (
	"strings"
	"testing"

	"github.com/reyoung/pika-go/internal/systemprompts"
)

func TestRoleSystemPromptsAreEmbeddedAndMatchCurrentContracts(t *testing.T) {
	t.Parallel()
	tests := []struct {
		logicalName string
		terminal    string
	}{
		{"baseline", "submit_baseline_definition"},
		{"baseline-verify", "finish_baseline_verification"},
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
			if test.logicalName == "baseline" || test.logicalName == "baseline-verify" || test.logicalName == "iteration" || test.logicalName == "integration" {
				for _, requirement := range []string{"candidate_change_policy", "allowed_candidate_surface", "implementation allowlist", "冻结验证资产"} {
					if !strings.Contains(prompt, requirement) {
						t.Fatalf("prompt omits candidate change policy requirement %q: %q", requirement, prompt)
					}
				}
			}
			if test.logicalName == "integration" {
				for _, requirement := range []string{"每次 warm-up 和 measured invocation", "checked invocation 数", "NaN 或其他 sentinel", "接近或超过一个数量级", "首次调用、capture/compile/setup", `"performance_claim"`, `"independent_retest"`, `"max_case_speedup"`} {
					if !strings.Contains(prompt, requirement) {
						t.Fatalf("Integration prompt omits suspicious-speedup validation requirement %q: %q", requirement, prompt)
					}
				}
			}
			if test.logicalName == "baseline" {
				for _, requirement := range []string{"per-run integrity", "canonical input tensors", "每次 warm-up 和 measured invocation", "设备端累计", "端到端口径", `"schema_version": 1`, `"restore_inputs_before_every_invocation"`, `"canonical_inputs_candidate_visible": false`} {
					if !strings.Contains(prompt, requirement) {
						t.Fatalf("Baseline prompt omits per-run benchmark integrity requirement %q: %q", requirement, prompt)
					}
				}
			}
			if test.logicalName == "baseline-verify" {
				for _, requirement := range []string{"canonical inputs", "checked invocation 数", "设备端累计紧凑统计", "端到端 metric", `"checked_invocations"`, `"canonical_input_mutations"`, `"tolerance_passed"`} {
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
