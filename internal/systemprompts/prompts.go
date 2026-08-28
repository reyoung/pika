package systemprompts

import (
	"bytes"
	"embed"
	"fmt"
	"strings"
	"text/template"
)

//go:embed defaults/*.md templates/*.tmpl
var promptFiles embed.FS

var contextBundleTemplate = template.Must(template.New("context-bundle").Option("missingkey=error").ParseFS(promptFiles, "templates/context-bundle.md.tmpl"))

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
	SummarySchema  []byte
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

	contextSection, err := renderContextFiles(contextFiles)
	if err != nil {
		return "", err
	}
	sections := []string{strings.TrimSpace(string(rolePrompt)), contextSection}
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

func renderContextFiles(files ContextFiles) (string, error) {
	if files.ContextPath == "" || files.ContextSHA256 == "" || files.MessagesPath == "" || files.MessagesSHA256 == "" ||
		len(files.ContextSchema) == 0 || len(files.MessageSchema) == 0 || len(files.SummarySchema) == 0 {
		return "## Pika Session Context Bundle\n\nContext Bundle 未完整物化；不得开始执行。", nil
	}
	data := struct {
		ContextFiles
		ContextSchemaText string
		MessageSchemaText string
		SummarySchemaText string
	}{ContextFiles: files, ContextSchemaText: strings.TrimSpace(string(files.ContextSchema)),
		MessageSchemaText: strings.TrimSpace(string(files.MessageSchema)), SummarySchemaText: strings.TrimSpace(string(files.SummarySchema))}
	var output bytes.Buffer
	if err := contextBundleTemplate.ExecuteTemplate(&output, "context-bundle.md.tmpl", data); err != nil {
		return "", fmt.Errorf("render Context Bundle System Prompt template: %w", err)
	}
	return strings.TrimSpace(output.String()), nil
}
