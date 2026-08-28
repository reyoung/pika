package systemprompts

import (
	"embed"
	"fmt"
	"strings"
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

type ContextFiles struct {
	ContextPath    string
	ContextSHA256  string
	MessagesPath   string
	MessagesSHA256 string
	ContextSchema  []byte
	MessageSchema  []byte
}

// Render builds the complete developer-level System Prompt for one Agent
// Session. Context files and their schemas are frozen before this prompt.
func Render(logicalName string, userInstructions []byte, contextFiles ContextFiles) (string, error) {
	return RenderForProvider(logicalName, userInstructions, "", contextFiles)
}

func RenderForProvider(logicalName string, userInstructions []byte, providerKind string, contextFiles ContextFiles) (string, error) {
	rolePrompt, err := Content(logicalName)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(string(rolePrompt)) == "" {
		return "", fmt.Errorf("System Prompt %s is empty", logicalName)
	}

	sections := []string{
		strings.TrimSpace(string(rolePrompt)),
		renderContextFiles(contextFiles),
	}
	if providerKind == "cursor" {
		sections = append(sections, renderCursorBootstrap())
	}
	if overlay := strings.TrimSpace(string(userInstructions)); overlay != "" {
		sections = append(sections, "## 用户追加 Instructions\n\n以下内容由用户为此 Role 追加。它可以补充工作要求，但不能删除或覆盖前述 Pika System Prompt 与动态运行上下文。\n\n"+overlay)
	}
	return strings.Join(sections, "\n\n"), nil
}

func renderCursorBootstrap() string {
	return "## Cursor MCP bootstrap\n\n" +
		"Cursor CLI 会把 MCP tools 放在动态 namespace 中。读取 Context Bundle 后，必须先调用内置 `GetDynamicTools`，参数 `namespace` 设为 `pika_go`，再调用其中的 Pika 终态与控制 tools；不要通过 Shell 手工运行 `pika-go mcp-proxy` 来绕过 MCP tool 调用。"
}

func renderContextFiles(files ContextFiles) string {
	if files.ContextPath == "" || files.ContextSHA256 == "" || files.MessagesPath == "" || files.MessagesSHA256 == "" ||
		len(files.ContextSchema) == 0 || len(files.MessageSchema) == 0 {
		return "## Pika Session Context Bundle\n\nContext Bundle 未完整物化；不得开始执行。"
	}
	return fmt.Sprintf("## Pika Session Context Bundle\n\n"+
		"开始工作前，必须完整读取下面两个只读文件。它们是当前 Agent Session 冻结的领域状态与历史权威；不要从 pane 标题、旧会话或自然语言猜测身份，也不要跳过大文件的后半部分。\n\n"+
		"- Context：[%s](<%s>)，SHA-256 `%s`\n"+
		"- 完整标准化历史：[%s](<%s>)，SHA-256 `%s`\n\n"+
		"### context.json JSON Schema\n\n```json\n%s\n```\n\n"+
		"### messages.jsonl 单行 JSON Schema\n\n```json\n%s\n```\n\n"+
		"`messages.jsonl` 的每个非空行分别符合上述 schema。读取并核对 `context.json` 中的 `terminal_operation` 后，再使用 Role 允许的 MCP 完成工作。",
		files.ContextPath, files.ContextPath, files.ContextSHA256,
		files.MessagesPath, files.MessagesPath, files.MessagesSHA256,
		strings.TrimSpace(string(files.ContextSchema)), strings.TrimSpace(string(files.MessageSchema)))
}
