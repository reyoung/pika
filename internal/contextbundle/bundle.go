package contextbundle

import (
	"bytes"
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/reyoung/pika-go/internal/symphony"
)

const SchemaVersion int64 = 2

//go:embed schemas/*.json
var schemaFiles embed.FS

type Store interface {
	ContextProjection(context.Context, string) (symphony.ContextProjection, error)
	ReadContextSnapshot(context.Context, string) (symphony.ContextSnapshot, bool, error)
	FreezeContextSnapshot(context.Context, symphony.ContextSnapshot) (symphony.ContextSnapshot, error)
}

type Materializer struct {
	Store Store
	Root  string
}

type Bundle struct {
	ContextPath    string
	ContextSHA256  string
	MessagesPath   string
	MessagesSHA256 string
	MessageRecords int64
}

type FileReference struct {
	Path    string `json:"path"`
	SHA256  string `json:"sha256"`
	Bytes   int64  `json:"bytes"`
	Records int64  `json:"records"`
}

type AttemptHistory struct {
	Attempt  symphony.AttemptView `json:"attempt"`
	Messages FileReference        `json:"messages"`
	Summary  FileReference        `json:"summary"`
}

type IterationContext struct {
	HistoryLimit           int64            `json:"history_limit"`
	RecentTerminalAttempts []AttemptHistory `json:"recent_terminal_attempts"`
}

type Document struct {
	SchemaVersion     int64                     `json:"schema_version"`
	Session           symphony.AgentSession     `json:"session"`
	Optimization      symphony.OptimizationView `json:"optimization"`
	Baseline          *symphony.BaselineView    `json:"baseline,omitempty"`
	Best              *symphony.BestView        `json:"best,omitempty"`
	Work              symphony.RuntimeWork      `json:"work"`
	GeneratorWork     symphony.WorkView         `json:"generator_work"`
	TerminalOperation string                    `json:"terminal_operation"`
	Messages          FileReference             `json:"messages"`
	Iteration         *IterationContext         `json:"iteration_context,omitempty"`
}

type MessageRecord struct {
	SchemaVersion   int64                         `json:"schema_version"`
	Sequence        int64                         `json:"sequence"`
	Turn            symphony.ConversationTurnView `json:"turn"`
	Tools           []symphony.ToolEventView      `json:"tools"`
	ToolSupplements []symphony.ToolSupplementView `json:"tool_supplements"`
}

type SummaryRecord struct {
	SchemaVersion int64                 `json:"schema_version"`
	Attempt       symphony.AttemptView `json:"attempt"`
}

func Schemas() (contextSchema, messageSchema, summarySchema []byte, err error) {
	contextSchema, err = schemaFiles.ReadFile("schemas/context.schema.json")
	if err != nil {
		return nil, nil, nil, fmt.Errorf("read embedded context schema: %w", err)
	}
	messageSchema, err = schemaFiles.ReadFile("schemas/message.schema.json")
	if err != nil {
		return nil, nil, nil, fmt.Errorf("read embedded message schema: %w", err)
	}
	summarySchema, err = schemaFiles.ReadFile("schemas/summary.schema.json")
	if err != nil {
		return nil, nil, nil, fmt.Errorf("read embedded summary schema: %w", err)
	}
	return contextSchema, messageSchema, summarySchema, nil
}

func (m Materializer) Materialize(ctx context.Context, session symphony.AgentSession) (Bundle, error) {
	if m.Store == nil || m.Root == "" {
		return Bundle{}, errors.New("Context Bundle store and root are required")
	}
	if session.ID == "" || filepath.Base(session.ID) != session.ID || strings.ContainsAny(session.ID, `/\\`) {
		return Bundle{}, errors.New("safe Agent Session ID is required")
	}
	if stored, found, err := m.Store.ReadContextSnapshot(ctx, session.ID); err != nil {
		return Bundle{}, err
	} else if found {
		return m.verifyStored(stored)
	}

	projection, err := m.Store.ContextProjection(ctx, session.ID)
	if err != nil {
		return Bundle{}, err
	}
	if projection.Session.ID != session.ID || projection.Session.WorkID != session.WorkID || projection.Session.Generation != session.Generation {
		return Bundle{}, errors.New("Context Projection does not match the requested Agent Session")
	}
	files := map[string][]byte{}
	messagesRelative := filepath.Join(session.ID, "messages.jsonl")
	messages := mustEncodeJSONL(recordsFor(projection.Journal))
	files[messagesRelative] = messages
	document := Document{
		SchemaVersion: SchemaVersion, Session: projection.Session, Optimization: projection.View.Optimization,
		Baseline: projection.View.Baseline, Best: projection.View.Best, Work: projection.TargetWork,
		GeneratorWork: projection.GeneratorWork, TerminalOperation: terminalOperation(projection.Session.Role),
		Messages: reference(filepath.Join(m.Root, messagesRelative), messages, int64(len(recordsFor(projection.Journal)))),
	}
	if projection.TargetWork.Work.Role == symphony.RoleIteration {
		document.Iteration = &IterationContext{HistoryLimit: projection.TargetWork.IterationHistoryLimit, RecentTerminalAttempts: []AttemptHistory{}}
		for _, history := range projection.AttemptHistories {
			root := filepath.Join(session.ID, "attempt-history", history.Attempt.ID)
			historyRecords := recordsFor(history.Journal)
			historyMessages := mustEncodeJSONL(historyRecords)
			summary := mustEncodeJSONL([]SummaryRecord{{SchemaVersion: SchemaVersion, Attempt: history.Attempt}})
			messagesPath, summaryPath := filepath.Join(root, "messages.jsonl"), filepath.Join(root, "summary.jsonl")
			files[messagesPath], files[summaryPath] = historyMessages, summary
			document.Iteration.RecentTerminalAttempts = append(document.Iteration.RecentTerminalAttempts, AttemptHistory{
				Attempt: history.Attempt,
				Messages: reference(filepath.Join(m.Root, messagesPath), historyMessages, int64(len(historyRecords))),
				Summary: reference(filepath.Join(m.Root, summaryPath), summary, 1),
			})
		}
	}
	contextRelative := filepath.Join(session.ID, "context.json")
	contextBytes, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return Bundle{}, fmt.Errorf("encode context.json: %w", err)
	}
	contextBytes = append(contextBytes, '\n')
	files[contextRelative] = contextBytes
	if err := m.writeAtomically(session.ID, files); err != nil {
		return Bundle{}, err
	}
	candidate := symphony.ContextSnapshot{
		AgentSessionID: session.ID, SchemaVersion: SchemaVersion,
		ContextRelativePath: contextRelative, ContextSHA256: digest(contextBytes), ContextBytes: int64(len(contextBytes)),
		MessagesRelativePath: messagesRelative, MessagesSHA256: digest(messages), MessagesBytes: int64(len(messages)),
		MessageRecords: document.Messages.Records,
	}
	stored, err := m.Store.FreezeContextSnapshot(ctx, candidate)
	if err != nil {
		_ = os.RemoveAll(filepath.Join(m.Root, session.ID))
		return Bundle{}, err
	}
	return m.verifyStored(stored)
}

func recordsFor(journal symphony.ConversationJournalView) []MessageRecord {
	tools := make(map[string][]symphony.ToolEventView)
	for _, tool := range journal.Tools {
		tools[tool.ConversationTurnID] = append(tools[tool.ConversationTurnID], tool)
	}
	supplements := make(map[string][]symphony.ToolSupplementView)
	for _, supplement := range journal.ToolSupplements {
		supplements[supplement.ConversationTurnID] = append(supplements[supplement.ConversationTurnID], supplement)
	}
	records := make([]MessageRecord, 0, len(journal.Turns))
	for index, turn := range journal.Turns {
		records = append(records, MessageRecord{SchemaVersion: SchemaVersion, Sequence: int64(index + 1), Turn: turn,
			Tools: nonNil(tools[turn.ID]), ToolSupplements: nonNil(supplements[turn.ID])})
	}
	return records
}

func mustEncodeJSONL[T any](records []T) []byte {
	var output bytes.Buffer
	encoder := json.NewEncoder(&output)
	encoder.SetEscapeHTML(false)
	for _, record := range records {
		if err := encoder.Encode(record); err != nil {
			panic(err)
		}
	}
	return output.Bytes()
}

func reference(path string, contents []byte, records int64) FileReference {
	return FileReference{Path: path, SHA256: digest(contents), Bytes: int64(len(contents)), Records: records}
}

func (m Materializer) writeAtomically(sessionID string, files map[string][]byte) error {
	if err := os.MkdirAll(m.Root, 0o700); err != nil {
		return fmt.Errorf("create Context Bundle root: %w", err)
	}
	finalRoot := filepath.Join(m.Root, sessionID)
	if _, err := os.Stat(finalRoot); err == nil {
		if err := os.RemoveAll(finalRoot); err != nil {
			return fmt.Errorf("remove incomplete Context Bundle: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect Context Bundle destination: %w", err)
	}
	temporary, err := os.MkdirTemp(m.Root, ".context-"+sessionID+"-")
	if err != nil {
		return fmt.Errorf("create temporary Context Bundle: %w", err)
	}
	defer os.RemoveAll(temporary)
	for relative, contents := range files {
		trimmed := strings.TrimPrefix(relative, sessionID+string(filepath.Separator))
		if trimmed == relative || trimmed == "" || strings.HasPrefix(trimmed, "..") {
			return errors.New("Context Bundle contains an unsafe relative path")
		}
		path := filepath.Join(temporary, trimmed)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return fmt.Errorf("create Context Bundle directory: %w", err)
		}
		if err := writeFile(path, contents); err != nil {
			return err
		}
	}
	if err := os.Rename(temporary, finalRoot); err != nil {
		return fmt.Errorf("publish Context Bundle: %w", err)
	}
	return nil
}

func writeFile(path string, contents []byte) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o400)
	if err != nil {
		return fmt.Errorf("create Context Bundle file %s: %w", filepath.Base(path), err)
	}
	if _, err := file.Write(contents); err != nil {
		_ = file.Close()
		return fmt.Errorf("write Context Bundle file %s: %w", filepath.Base(path), err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return fmt.Errorf("sync Context Bundle file %s: %w", filepath.Base(path), err)
	}
	return file.Close()
}

func (m Materializer) verifyStored(snapshot symphony.ContextSnapshot) (Bundle, error) {
	if snapshot.SchemaVersion != SchemaVersion || filepath.Clean(snapshot.ContextRelativePath) != filepath.Join(snapshot.AgentSessionID, "context.json") ||
		filepath.Clean(snapshot.MessagesRelativePath) != filepath.Join(snapshot.AgentSessionID, "messages.jsonl") {
		return Bundle{}, errors.New("stored Context Snapshot has an unsupported contract")
	}
	contextPath := filepath.Join(m.Root, snapshot.ContextRelativePath)
	messagesPath := filepath.Join(m.Root, snapshot.MessagesRelativePath)
	contextDigest, contextBytes, err := digestPath(contextPath)
	if err != nil {
		return Bundle{}, err
	}
	messagesDigest, messagesBytes, err := digestPath(messagesPath)
	if err != nil {
		return Bundle{}, err
	}
	if contextDigest != snapshot.ContextSHA256 || contextBytes != snapshot.ContextBytes ||
		messagesDigest != snapshot.MessagesSHA256 || messagesBytes != snapshot.MessagesBytes {
		return Bundle{}, errors.New("frozen Context Bundle digest does not match its files")
	}
	var document Document
	contents, err := os.ReadFile(contextPath)
	if err != nil || json.Unmarshal(contents, &document) != nil {
		return Bundle{}, errors.New("frozen context.json is invalid")
	}
	if document.Iteration != nil {
		for _, history := range document.Iteration.RecentTerminalAttempts {
			for _, ref := range []FileReference{history.Messages, history.Summary} {
				actual, size, err := digestPath(ref.Path)
				if err != nil || actual != ref.SHA256 || size != ref.Bytes {
					return Bundle{}, errors.New("frozen Attempt history digest does not match its file")
				}
			}
		}
	}
	return Bundle{ContextPath: contextPath, ContextSHA256: contextDigest, MessagesPath: messagesPath,
		MessagesSHA256: messagesDigest, MessageRecords: snapshot.MessageRecords}, nil
}

func digest(contents []byte) string {
	value := sha256.Sum256(contents)
	return hex.EncodeToString(value[:])
}

func digestPath(path string) (string, int64, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", 0, fmt.Errorf("open frozen Context Bundle file %s: %w", filepath.Base(path), err)
	}
	defer file.Close()
	hash := sha256.New()
	size, err := io.Copy(hash, file)
	if err != nil {
		return "", 0, fmt.Errorf("hash frozen Context Bundle file %s: %w", filepath.Base(path), err)
	}
	return hex.EncodeToString(hash.Sum(nil)), size, nil
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

func nonNil[T any](values []T) []T {
	if values == nil {
		return []T{}
	}
	return values
}
