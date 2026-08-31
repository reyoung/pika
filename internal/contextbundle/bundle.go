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

const SchemaVersion int64 = 3

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
	HistoryLimit           int64                         `json:"history_limit"`
	RequiredCaseSet        symphony.IterationCaseSetView `json:"required_case_set"`
	RecentTerminalAttempts []AttemptHistory              `json:"recent_terminal_attempts"`
	PreviousRound          *RoundHistory                 `json:"previous_round,omitempty"`
}

type RoundHistory struct {
	Work     symphony.WorkView           `json:"work"`
	Round    symphony.IterationRoundView `json:"round"`
	Messages FileReference               `json:"messages"`
}

type Document struct {
	SchemaVersion     int64                          `json:"schema_version"`
	Session           symphony.AgentSession          `json:"session"`
	Optimization      symphony.OptimizationView      `json:"optimization"`
	Baseline          *symphony.BaselineView         `json:"baseline,omitempty"`
	Best              *symphony.BestView             `json:"best,omitempty"`
	IterationCaseSet  *symphony.IterationCaseSetView `json:"iteration_case_set,omitempty"`
	Attempts          []symphony.AttemptView         `json:"attempts"`
	IterationRounds   []symphony.IterationRoundView  `json:"iteration_rounds"`
	Integrations      []symphony.IntegrationView     `json:"integrations"`
	BackOffs          []symphony.BackOffView         `json:"back_offs"`
	Work              symphony.RuntimeWork           `json:"work"`
	GeneratorWork     symphony.WorkView              `json:"generator_work"`
	TerminalOperation string                         `json:"terminal_operation"`
	Messages          FileReference                  `json:"messages"`
	Iteration         *IterationContext              `json:"iteration_context,omitempty"`
}

type SummaryRecord struct {
	SchemaVersion int64                `json:"schema_version"`
	Attempt       symphony.AttemptView `json:"attempt"`
}

type MessageRecord struct {
	SchemaVersion   int64                         `json:"schema_version"`
	Sequence        int64                         `json:"sequence"`
	Turn            symphony.ConversationTurnView `json:"turn"`
	Tools           []symphony.ToolEventView      `json:"tools"`
	ToolSupplements []symphony.ToolSupplementView `json:"tool_supplements"`
}

func Schemas() (contextSchema, messageSchema []byte, err error) {
	contextSchema, err = schemaFiles.ReadFile("schemas/context.schema.json")
	if err != nil {
		return nil, nil, fmt.Errorf("read embedded context schema: %w", err)
	}
	messageSchema, err = schemaFiles.ReadFile("schemas/message.schema.json")
	if err != nil {
		return nil, nil, fmt.Errorf("read embedded message schema: %w", err)
	}
	return contextSchema, messageSchema, nil
}

func SummarySchema() ([]byte, error) {
	contents, err := schemaFiles.ReadFile("schemas/summary.schema.json")
	if err != nil {
		return nil, fmt.Errorf("read embedded summary schema: %w", err)
	}
	return contents, nil
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
	records := recordsFor(projection.Journal)
	messages, err := encodeJSONL(records)
	if err != nil {
		return Bundle{}, err
	}
	contextRelative := filepath.Join(session.ID, "context.json")
	messagesRelative := filepath.Join(session.ID, "messages.jsonl")
	messagesPath := filepath.Join(m.Root, messagesRelative)
	messagesDigest := digest(messages)
	document := Document{
		SchemaVersion: SchemaVersion, Session: projection.Session, Optimization: projection.View.Optimization,
		Baseline: projection.View.Baseline, Best: projection.View.Best, IterationCaseSet: projection.View.IterationCaseSet,
		Attempts: nonNil(projection.View.Attempts), IterationRounds: nonNil(projection.View.IterationRounds),
		Integrations: nonNil(projection.View.Integrations), BackOffs: nonNil(projection.View.BackOffs),
		Work: projection.TargetWork, GeneratorWork: projection.GeneratorWork,
		TerminalOperation: terminalOperation(projection.Session.Role),
		Messages:          FileReference{Path: messagesPath, SHA256: messagesDigest, Bytes: int64(len(messages)), Records: int64(len(records))},
	}
	files := map[string][]byte{messagesRelative: messages}
	if projection.TargetWork.Work.Role == symphony.RoleIteration {
		if projection.TargetWork.IterationCaseSet == nil {
			return Bundle{}, errors.New("Iteration Work has no frozen Iteration Case Snapshot")
		}
		document.Iteration = &IterationContext{HistoryLimit: projection.TargetWork.IterationHistoryLimit,
			RequiredCaseSet: *projection.TargetWork.IterationCaseSet, RecentTerminalAttempts: []AttemptHistory{}}
		if projection.PreviousRound != nil {
			previousRecords := recordsFor(projection.PreviousRound.Journal)
			previousMessages, err := encodeJSONL(previousRecords)
			if err != nil {
				return Bundle{}, err
			}
			previousRelative := filepath.Join(session.ID, "previous-round", fmt.Sprintf("round-%d", projection.PreviousRound.Round.Round), "messages.jsonl")
			files[previousRelative] = previousMessages
			document.Iteration.PreviousRound = &RoundHistory{Work: projection.PreviousRound.Work, Round: projection.PreviousRound.Round,
				Messages: FileReference{Path: filepath.Join(m.Root, previousRelative), SHA256: digest(previousMessages), Bytes: int64(len(previousMessages)), Records: int64(len(previousRecords))}}
		}
		for _, history := range projection.AttemptHistories {
			historyRecords := recordsFor(history.Journal)
			historyMessages, err := encodeJSONL(historyRecords)
			if err != nil {
				return Bundle{}, err
			}
			summary, err := encodeSummaryJSONL(SummaryRecord{SchemaVersion: SchemaVersion, Attempt: history.Attempt})
			if err != nil {
				return Bundle{}, err
			}
			root := filepath.Join(session.ID, "attempt-history", history.Attempt.ID)
			historyMessagesRelative := filepath.Join(root, "messages.jsonl")
			summaryRelative := filepath.Join(root, "summary.jsonl")
			files[historyMessagesRelative], files[summaryRelative] = historyMessages, summary
			document.Iteration.RecentTerminalAttempts = append(document.Iteration.RecentTerminalAttempts, AttemptHistory{
				Attempt:  history.Attempt,
				Messages: FileReference{Path: filepath.Join(m.Root, historyMessagesRelative), SHA256: digest(historyMessages), Bytes: int64(len(historyMessages)), Records: int64(len(historyRecords))},
				Summary:  FileReference{Path: filepath.Join(m.Root, summaryRelative), SHA256: digest(summary), Bytes: int64(len(summary)), Records: 1},
			})
		}
	}
	contextBytes, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return Bundle{}, fmt.Errorf("encode context.json: %w", err)
	}
	contextBytes = append(contextBytes, '\n')
	contextDigest := digest(contextBytes)
	files[contextRelative] = contextBytes

	if err := m.writeAtomically(session.ID, files); err != nil {
		return Bundle{}, err
	}
	candidate := symphony.ContextSnapshot{
		AgentSessionID: session.ID, SchemaVersion: SchemaVersion,
		ContextRelativePath: contextRelative, ContextSHA256: contextDigest, ContextBytes: int64(len(contextBytes)),
		MessagesRelativePath: messagesRelative, MessagesSHA256: messagesDigest, MessagesBytes: int64(len(messages)),
		MessageRecords: int64(len(records)),
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

func encodeJSONL(records []MessageRecord) ([]byte, error) {
	var output bytes.Buffer
	encoder := json.NewEncoder(&output)
	encoder.SetEscapeHTML(false)
	for _, record := range records {
		if err := encoder.Encode(record); err != nil {
			return nil, fmt.Errorf("encode messages.jsonl: %w", err)
		}
	}
	return output.Bytes(), nil
}

func encodeSummaryJSONL(record SummaryRecord) ([]byte, error) {
	contents, err := json.Marshal(record)
	if err != nil {
		return nil, fmt.Errorf("encode summary.jsonl: %w", err)
	}
	return append(contents, '\n'), nil
}

func (m Materializer) writeAtomically(sessionID string, files map[string][]byte) error {
	if err := os.MkdirAll(m.Root, 0o700); err != nil {
		return fmt.Errorf("create Context Bundle root: %w", err)
	}
	if err := os.Chmod(m.Root, 0o700); err != nil {
		return fmt.Errorf("protect Context Bundle root: %w", err)
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
	if err := os.Chmod(temporary, 0o700); err != nil {
		return fmt.Errorf("protect Context Bundle directory: %w", err)
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
	if err := file.Close(); err != nil {
		return fmt.Errorf("close Context Bundle file %s: %w", filepath.Base(path), err)
	}
	return nil
}

func (m Materializer) verifyStored(snapshot symphony.ContextSnapshot) (Bundle, error) {
	if (snapshot.SchemaVersion < 2 || snapshot.SchemaVersion > SchemaVersion) || filepath.Clean(snapshot.ContextRelativePath) != filepath.Join(snapshot.AgentSessionID, "context.json") ||
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
	contents, err := os.ReadFile(contextPath)
	if err != nil {
		return Bundle{}, err
	}
	var document Document
	if err := json.Unmarshal(contents, &document); err != nil {
		return Bundle{}, errors.New("frozen context.json is invalid")
	}
	if document.Iteration != nil {
		if document.Iteration.PreviousRound != nil {
			reference := document.Iteration.PreviousRound.Messages
			actualDigest, actualBytes, err := digestPath(reference.Path)
			if err != nil || actualDigest != reference.SHA256 || actualBytes != reference.Bytes {
				return Bundle{}, errors.New("frozen previous Round history digest does not match its file")
			}
		}
		for _, history := range document.Iteration.RecentTerminalAttempts {
			for _, reference := range []FileReference{history.Messages, history.Summary} {
				actualDigest, actualBytes, err := digestPath(reference.Path)
				if err != nil || actualDigest != reference.SHA256 || actualBytes != reference.Bytes {
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
