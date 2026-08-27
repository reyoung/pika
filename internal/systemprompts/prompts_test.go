package systemprompts_test

import (
	"strings"
	"testing"

	"github.com/reyoung/pika-go/internal/symphony"
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
			if (test.logicalName == "baseline" || test.logicalName == "iteration") && !strings.Contains(prompt, "commit_changes") {
				t.Fatalf("write-capable Role prompt omits scoped commit MCP: %q", prompt)
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

func TestRenderIncludesFrozenBaselineRepositorySnapshot(t *testing.T) {
	t.Parallel()
	work := symphony.RuntimeWork{
		Work: symphony.WorkView{
			ID: "verification-work", BaselineRevisionID: "baseline-1", Role: symphony.RoleBaselineVerification, Generation: 1,
		},
		Repository: "/repo", OptimizationRepository: "/repo", BaselineNumber: 1,
		BaselineStatus: symphony.BaselineVerifying, BaselineDefinitionSHA256: "definition-digest",
		BaselineRepositorySHA: "0123456789012345678901234567890123456789",
	}
	prompt, err := systemprompts.Render("baseline-verify", work, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prompt, "冻结 Baseline Repository Snapshot SHA：`0123456789012345678901234567890123456789`") {
		t.Fatalf("rendered prompt omits frozen repository snapshot:\n%s", prompt)
	}
}

func TestRenderCombinesImmutablePromptDynamicContextAndOptionalUserInstructions(t *testing.T) {
	t.Parallel()
	work := symphony.RuntimeWork{
		Work: symphony.WorkView{
			ID: "work-7", BaselineRevisionID: "baseline-2", Role: symphony.RoleIteration,
			Generation: 3, AttemptID: "attempt-4", IterationRound: 2,
		},
		Repository: "/worktrees/attempt-4", OptimizationRepository: "/repo", OptimizationID: "optimization-1", OptimizationStatus: symphony.OptimizationOptimizing, OptimizationRevision: 17,
		BaselineNumber: 2, BaselineStatus: symphony.BaselineAccepted, BaselineDefinitionSHA256: "definition-digest",
		BaseSHA: "base-sha", BestSHA: "best-sha", BestSequence: 8, IterationKind: "back_off",
		BackOffMessage: "also measure case 9",
	}
	prompt, err := systemprompts.Render("iteration", work, []byte("优先检查 shared memory。\n"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"你是 Pika 的 Iteration Agent", "## Pika 动态运行上下文", "`optimization-1`", "revision `17`", "`definition-digest`", "`attempt-4`", "`base-sha`",
		"`best-sha`（revision `8`）", "also measure case 9", "## 用户追加 Instructions", "优先检查 shared memory。",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("rendered System Prompt does not contain %q:\n%s", want, prompt)
		}
	}

	withoutOverlay, err := systemprompts.Render("iteration", work, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(withoutOverlay, "用户追加 Instructions") || strings.Contains(withoutOverlay, "优先检查 shared memory") {
		t.Fatalf("empty default instructions added an overlay: %s", withoutOverlay)
	}
}

func TestRenderForCursorFreezesDynamicMCPBootstrapOutsideUserInstructions(t *testing.T) {
	t.Parallel()
	work := symphony.RuntimeWork{
		Work:       symphony.WorkView{ID: "work-1", BaselineRevisionID: "baseline-1", Role: symphony.RoleBaselineDraft, Generation: 1},
		Repository: "/repo", OptimizationRepository: "/repo",
	}
	cursorPrompt, err := systemprompts.RenderForProvider("baseline", work, []byte("user overlay"), "cursor")
	if err != nil {
		t.Fatal(err)
	}
	bootstrap := "`GetDynamicTools`，参数 `namespace` 设为 `pika_go`"
	if !strings.Contains(cursorPrompt, bootstrap) || !strings.Contains(cursorPrompt, "不要通过 Shell 手工运行 `pika-go mcp-proxy`") {
		t.Fatalf("Cursor System Prompt omits dynamic MCP bootstrap:\n%s", cursorPrompt)
	}
	if strings.Index(cursorPrompt, "## Cursor MCP bootstrap") > strings.Index(cursorPrompt, "## 用户追加 Instructions") {
		t.Fatalf("Cursor bootstrap was rendered as editable user Instructions:\n%s", cursorPrompt)
	}
	codexPrompt, err := systemprompts.RenderForProvider("baseline", work, nil, "codex")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(codexPrompt, "Cursor MCP bootstrap") || strings.Contains(codexPrompt, "GetDynamicTools") {
		t.Fatalf("Codex System Prompt contains Cursor-only bootstrap:\n%s", codexPrompt)
	}
}

func TestRenderIncludesRoleSpecificDynamicSystemContext(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		logicalName string
		work        symphony.RuntimeWork
		want        []string
	}{
		{
			name: "successor baseline draft", logicalName: "baseline",
			work: symphony.RuntimeWork{
				Work:       symphony.WorkView{ID: "draft-work-2", BaselineRevisionID: "baseline-2", Role: symphony.RoleBaselineDraft, Generation: 1},
				Repository: "/repo", OptimizationRepository: "/repo", BaselineNumber: 2,
				PredecessorBaselineID: "baseline-1", PredecessorFailureKind: "protocol_failure",
				PredecessorFailureReason:        "Definition used an execution Work ID as durable identity",
				PredecessorRequestedChanges:     "bind the Definition to its Baseline Revision instead",
				PredecessorVerificationEvidence: []byte(`{"invalid_field":"optimization.work_id"}`),
			},
			want: []string{"`baseline-1`", "`protocol_failure`", "Definition used an execution Work ID", "bind the Definition to its Baseline Revision", "上一版完整验证证据可通过 `get_context` 读取"},
		},
		{
			name: "integration", logicalName: "integration",
			work: symphony.RuntimeWork{
				Work:       symphony.WorkView{ID: "integration-work", BaselineRevisionID: "baseline-1", Role: symphony.RoleIntegration, Generation: 1, IntegrationID: "integration-9", AttemptID: "attempt-2"},
				Repository: "/candidate", OptimizationRepository: "/repo", BaselineNumber: 1,
				CandidateSHA: "candidate-sha", BaseSHA: "base-sha", ExpectedBestSHA: "expected-best-sha", BestSHA: "best-sha", BestSequence: 4,
				IntegrationFIFOPosition: 7, IntegrationStatus: "running", GitIntentID: "intent-3", GitIntentState: "pending",
			},
			want: []string{"`integration-9`", "FIFO position：`7`", "`running`", "`attempt-2`", "`candidate-sha`", "`base-sha`", "`expected-best-sha`", "`best-sha`（revision `4`）", "`intent-3`", "`pending`"},
		},
		{
			name: "follow-up", logicalName: "follow-up/integration",
			work: symphony.RuntimeWork{
				Work:       symphony.WorkView{ID: "generator-work", BaselineRevisionID: "baseline-1", Role: symphony.RoleFollowUp, Generation: 2, FollowUpRequestID: "request-3"},
				Repository: "/candidate", OptimizationRepository: "/repo", BaselineNumber: 1,
				FollowUpTargetWorkID: "target-work", FollowUpTargetRole: symphony.RoleIntegration, FollowUpSequence: 3, FollowUpDueAt: "2026-08-27T12:00:00Z",
				FollowUpMaxMessages: 8, FollowUpGeneratorMax: 3, FollowUpGeneratorTry: 2,
			},
			want: []string{"`request-3`", "`target-work`", "`integration`", "`3`", "`2026-08-27T12:00:00Z`", "最多 `8` 条", "`2/3`", "bounded conversation/tool journal"},
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			prompt, err := systemprompts.Render(test.logicalName, test.work, nil)
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range test.want {
				if !strings.Contains(prompt, want) {
					t.Fatalf("dynamic System Context does not contain %q:\n%s", want, prompt)
				}
			}
		})
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
