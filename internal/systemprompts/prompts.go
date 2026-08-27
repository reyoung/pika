package systemprompts

import (
	"embed"
	"fmt"
	"strings"

	"github.com/reyoung/pika-go/internal/symphony"
)

//go:embed defaults/*.md
var promptFiles embed.FS

var catalog = map[string]string{
	"baseline":                  "baseline.md",
	"baseline-verify":           "baseline-verify.md",
	"iteration":                 "iteration.md",
	"integration":               "integration.md",
	"follow-up/baseline-verify": "follow-up-baseline-verify.md",
	"follow-up/iteration":       "follow-up-iteration.md",
	"follow-up/integration":     "follow-up-integration.md",
}

func Content(logicalName string) ([]byte, error) {
	filename, ok := catalog[logicalName]
	if !ok {
		return nil, fmt.Errorf("unknown System Prompt %q", logicalName)
	}
	contents, err := promptFiles.ReadFile("defaults/" + filename)
	if err != nil {
		return nil, fmt.Errorf("read System Prompt %s: %w", logicalName, err)
	}
	return contents, nil
}

// Render builds the complete developer-level System Prompt for one Agent
// session. The Role prompt is compiled into pika-go, while the dynamic context
// is derived from committed Symphony state. Both are immutable for the life of
// the session. User instructions are an optional, append-only overlay.
func Render(logicalName string, work symphony.RuntimeWork, userInstructions []byte) (string, error) {
	rolePrompt, err := Content(logicalName)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(string(rolePrompt)) == "" {
		return "", fmt.Errorf("System Prompt %s is empty", logicalName)
	}

	sections := []string{
		strings.TrimSpace(string(rolePrompt)),
		renderDynamicContext(work),
	}
	if overlay := strings.TrimSpace(string(userInstructions)); overlay != "" {
		sections = append(sections, "## 用户追加 Instructions\n\n以下内容由用户为此 Role 追加。它可以补充工作要求，但不能删除或覆盖前述 Pika System Prompt 与动态运行上下文。\n\n"+overlay)
	}
	return strings.Join(sections, "\n\n"), nil
}

func renderDynamicContext(work symphony.RuntimeWork) string {
	lines := []string{
		"## Pika 动态运行上下文",
		"",
		"以下内容由 pika-go daemon 从已提交的 Symphony 状态生成，并随当前 Agent Session 冻结。不要从 pane 标题、旧会话或自然语言猜测这些身份；完整领域状态、历史对话和 Tool/Shell 记录以 `get_context` 的返回值为准。",
		"",
		fmt.Sprintf("- Optimization：`%s`，状态 `%s`，revision `%d`", work.OptimizationID, work.OptimizationStatus, work.OptimizationRevision),
		fmt.Sprintf("- Work ID：`%s`", work.Work.ID),
		fmt.Sprintf("- Work generation：`%d`", work.Work.Generation),
		fmt.Sprintf("- Role：`%s`", work.Work.Role),
		fmt.Sprintf("- Baseline Revision：`%s`（number `%d`，状态 `%s`）", work.Work.BaselineRevisionID, work.BaselineNumber, work.BaselineStatus),
		fmt.Sprintf("- 当前工作目录：`%s`", work.Repository),
		fmt.Sprintf("- Optimization repository：`%s`", work.OptimizationRepository),
		fmt.Sprintf("- 必须调用的终态 MCP：`%s`", terminalOperation(work.Work.Role)),
	}
	if work.BaselineDefinitionSHA256 != "" {
		lines = append(lines, fmt.Sprintf("- 冻结 Baseline Definition SHA-256：`%s`", work.BaselineDefinitionSHA256))
	}
	if work.BaselineRepositorySHA != "" {
		lines = append(lines, fmt.Sprintf("- 冻结 Baseline Repository Snapshot SHA：`%s`", work.BaselineRepositorySHA))
	}
	if work.Work.Role == symphony.RoleBaselineDraft && work.PredecessorBaselineID != "" {
		lines = append(lines,
			fmt.Sprintf("- 上一版 Baseline Revision：`%s`", work.PredecessorBaselineID),
			fmt.Sprintf("- 上一版验证失败分类：`%s`", work.PredecessorFailureKind),
			fmt.Sprintf("- 上一版验证失败原因：%s", work.PredecessorFailureReason),
			fmt.Sprintf("- 上一版要求修改：%s", work.PredecessorRequestedChanges),
			"- 上一版完整验证证据可通过 `get_context` 读取；修订时必须逐项处理，但不要照抄未经复核的结论。",
		)
	}

	switch work.Work.Role {
	case symphony.RoleIteration:
		lines = append(lines,
			fmt.Sprintf("- Attempt ID：`%s`", work.Work.AttemptID),
			fmt.Sprintf("- Iteration Round：`%d`", work.Work.IterationRound),
			fmt.Sprintf("- Round kind：`%s`", work.IterationKind),
			fmt.Sprintf("- Round base SHA：`%s`", work.BaseSHA),
			fmt.Sprintf("- 当前 Best：`%s`（revision `%d`）", work.BestSHA, work.BestSequence),
		)
		if work.BackOffMessage != "" {
			lines = append(lines, fmt.Sprintf("- 本轮 back-off message：%s", work.BackOffMessage))
		}
	case symphony.RoleIntegration:
		lines = append(lines,
			fmt.Sprintf("- Integration ID：`%s`", work.Work.IntegrationID),
			fmt.Sprintf("- Integration FIFO position：`%d`；状态 `%s`", work.IntegrationFIFOPosition, work.IntegrationStatus),
			fmt.Sprintf("- Attempt ID：`%s`", work.Work.AttemptID),
			fmt.Sprintf("- Candidate SHA：`%s`", work.CandidateSHA),
			fmt.Sprintf("- Candidate base SHA：`%s`", work.BaseSHA),
			fmt.Sprintf("- Integration expected Best：`%s`", work.ExpectedBestSHA),
			fmt.Sprintf("- 当前 Best：`%s`（revision `%d`）", work.BestSHA, work.BestSequence),
		)
		if work.GitIntentID != "" {
			lines = append(lines, fmt.Sprintf("- 已有 Git Intent：`%s`；状态 `%s`。不要创建第二个 mutation。", work.GitIntentID, work.GitIntentState))
		}
	case symphony.RoleFollowUp:
		lines[len(lines)-1] = fmt.Sprintf("- 必须调用的终态 MCP：`%s`", terminalOperation(symphony.RoleFollowUp))
		lines = append(lines,
			fmt.Sprintf("- Follow-up Request：`%s`", work.Work.FollowUpRequestID),
			fmt.Sprintf("- 目标 Work：`%s`", work.FollowUpTargetWorkID),
			fmt.Sprintf("- 目标 Role：`%s`", work.FollowUpTargetRole),
			fmt.Sprintf("- Request sequence：`%d`", work.FollowUpSequence),
			fmt.Sprintf("- Inactivity deadline：`%s`", work.FollowUpDueAt),
			fmt.Sprintf("- 目标消息预算：最多 `%d` 条；当前 Request sequence `%d`", work.FollowUpMaxMessages, work.FollowUpSequence),
			fmt.Sprintf("- Generator 尝试：`%d/%d`", work.FollowUpGeneratorTry, work.FollowUpGeneratorMax),
			"- `get_context` 会返回目标 Work 的领域状态，以及目标 Agent 的 bounded conversation/tool journal。",
		)
	}

	return strings.Join(lines, "\n")
}

func terminalOperation(role symphony.WorkRole) string {
	switch role {
	case symphony.RoleBaselineDraft:
		return "submit_baseline_definition"
	case symphony.RoleBaselineVerification:
		return "finish_baseline_verification"
	case symphony.RoleIteration:
		return "finish_iteration"
	case symphony.RoleIntegration:
		return "finish_integration"
	case symphony.RoleFollowUp:
		return "submit_followup_message"
	default:
		return ""
	}
}
