package activation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/reyoung/pika-go/internal/configuration"
	"github.com/reyoung/pika-go/internal/contextbundle"
	"github.com/reyoung/pika-go/internal/instructions"
	"github.com/reyoung/pika-go/internal/provider"
	"github.com/reyoung/pika-go/internal/symphony"
	"github.com/reyoung/pika-go/internal/systemprompts"
	"github.com/reyoung/pika-go/internal/toolapp"
	"github.com/reyoung/pika-go/internal/workruntime"
)

type Store interface {
	contextbundle.Store
	FreezeInstruction(context.Context, symphony.InstructionSnapshot) (symphony.InstructionSnapshot, error)
	MintAgentGrant(context.Context, string, []string, time.Duration) (symphony.AgentGrant, error)
}

type Preparer struct {
	Store           Store
	InstructionRoot string
	ContextsRoot    string
	GrantTTL        time.Duration
	SocketPath      string
	Environment     map[string]string
	AgentConfigPath string
	Providers       *provider.Registry
}

func (p Preparer) Prepare(ctx context.Context, session symphony.AgentSession, work symphony.RuntimeWork) (workruntime.Preparation, error) {
	if p.Store == nil || p.InstructionRoot == "" || p.ContextsRoot == "" {
		return workruntime.Preparation{}, errors.New("activation store, instruction root, and contexts root are required")
	}
	logicalName, err := logicalNameForRole(session.Role)
	if session.Role == symphony.RoleFollowUp {
		logicalName, err = logicalNameForFollowUp(work.FollowUpTargetRole)
	}
	if err != nil {
		return workruntime.Preparation{}, err
	}
	path, err := instructions.Path(p.InstructionRoot, logicalName)
	if err != nil {
		return workruntime.Preparation{}, err
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return workruntime.Preparation{}, fmt.Errorf("resolve instruction %s: %w", logicalName, err)
	}
	contents, err := os.ReadFile(resolved)
	if err != nil {
		return workruntime.Preparation{}, fmt.Errorf("read instruction %s: %w", logicalName, err)
	}
	agentConfiguration := provider.AgentConfiguration{Kind: session.AgentKind}
	registry := p.Providers
	if registry == nil {
		registry = provider.DefaultRegistry()
	}
	if p.AgentConfigPath != "" {
		configured, err := configuration.LoadAgentWithRegistry(p.AgentConfigPath, agentConfigRole(session.Role), registry)
		if err != nil {
			return workruntime.Preparation{}, fmt.Errorf("load Agent configuration for %s: %w", session.Role, err)
		}
		if configured.Kind != session.AgentKind {
			return workruntime.Preparation{}, fmt.Errorf("configured Agent kind changed during activation: session=%s configured=%s", session.AgentKind, configured.Kind)
		}
		agentConfiguration = configured
	}
	contentHash := sha256.Sum256(contents)
	bundle, err := (contextbundle.Materializer{Store: p.Store, Root: p.ContextsRoot}).Materialize(ctx, session)
	if err != nil {
		return workruntime.Preparation{}, fmt.Errorf("materialize Agent Session Context Bundle: %w", err)
	}
	contextSchema, messageSchema, err := contextbundle.Schemas()
	if err != nil {
		return workruntime.Preparation{}, err
	}
	summarySchema, err := contextbundle.SummarySchema()
	if err != nil {
		return workruntime.Preparation{}, err
	}
	systemPrompt, err := systemprompts.RenderForProvider(logicalName, contents, agentConfiguration.Kind, systemprompts.ContextFiles{
		ContextPath: bundle.ContextPath, ContextSHA256: bundle.ContextSHA256,
		MessagesPath: bundle.MessagesPath, MessagesSHA256: bundle.MessagesSHA256,
		ContextSchema: contextSchema, MessageSchema: messageSchema, SummarySchema: summarySchema,
	})
	if err != nil {
		return workruntime.Preparation{}, fmt.Errorf("render System Prompt %s: %w", logicalName, err)
	}
	promptHash := sha256.Sum256([]byte(systemPrompt))
	stored, err := p.Store.FreezeInstruction(ctx, symphony.InstructionSnapshot{
		AgentSessionID:   session.ID,
		LogicalName:      logicalName,
		SourcePath:       resolved,
		ContentSHA256:    hex.EncodeToString(contentHash[:]),
		Content:          contents,
		SystemPrompt:     []byte(systemPrompt),
		ActivationSHA256: hex.EncodeToString(promptHash[:]),
	})
	if err != nil {
		return workruntime.Preparation{}, err
	}
	if len(stored.SystemPrompt) == 0 {
		return workruntime.Preparation{}, errors.New("frozen System Prompt is empty")
	}
	storedPromptHash := sha256.Sum256(stored.SystemPrompt)
	if stored.ActivationSHA256 != hex.EncodeToString(storedPromptHash[:]) {
		return workruntime.Preparation{}, errors.New("frozen System Prompt digest does not match its content")
	}
	ttl := p.GrantTTL
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	grant, err := p.Store.MintAgentGrant(ctx, session.ID, toolapp.CatalogForRole(session.Role), ttl)
	if err != nil {
		return workruntime.Preparation{}, err
	}
	environment := map[string]string{
		"PIKA_SESSION_ID":           session.ID,
		"PIKA_MCP_GRANT":            grant.Token,
		"PIKA_ROLE":                 string(session.Role),
		"PIKA_GO_SOCKET":            p.SocketPath,
		"PIKA_BASELINE_NUMBER":      fmt.Sprintf("%d", work.BaselineNumber),
		"PIKA_ATTEMPT_ID":           work.Work.AttemptID,
		"PIKA_ITERATION_ROUND":      fmt.Sprintf("%d", work.Work.IterationRound),
		"PIKA_ITERATION_KIND":       work.IterationKind,
		"PIKA_BASE_SHA":             work.BaseSHA,
		"PIKA_BEST_SHA":             work.BestSHA,
		"PIKA_SYSTEM_PROMPT_SHA256": stored.ActivationSHA256,
		"PIKA_CONTEXT_PATH":         bundle.ContextPath,
		"PIKA_CONTEXT_SHA256":       bundle.ContextSHA256,
		"PIKA_MESSAGES_PATH":        bundle.MessagesPath,
		"PIKA_MESSAGES_SHA256":      bundle.MessagesSHA256,
	}
	adapter, err := registry.Resolve(session.AgentKind)
	if err != nil {
		return workruntime.Preparation{}, err
	}
	kickoff := kickoffPrompt(work)
	launch, err := adapter.PrepareSession(ctx, provider.SessionActivation{
		AgentSessionID: session.ID,
		Repository:     work.Repository,
		Configuration:  agentConfiguration,
		SystemPrompt:   stored.SystemPrompt,
		InitialPrompt:  kickoff,
		Environment:    environment,
	})
	if err != nil {
		return workruntime.Preparation{}, fmt.Errorf("prepare %s provider Session: %w", session.AgentKind, err)
	}
	if launch.AgentKind != session.AgentKind {
		return workruntime.Preparation{}, fmt.Errorf("provider launch kind changed during activation: session=%s launch=%s", session.AgentKind, launch.AgentKind)
	}
	for key, value := range p.Environment {
		launch.Environment[key] = value
	}
	if launch.HandlesInitialPrompt {
		kickoff = ""
	}
	return workruntime.Preparation{Prompt: kickoff, Environment: launch.Environment, EphemeralPath: launch.EphemeralPath, StartupTimeout: launch.StartupTimeout, ReturnOnLaunch: launch.ReturnOnLaunch, Cleanup: launch.Cleanup}, nil
}

func agentConfigRole(role symphony.WorkRole) string {
	switch role {
	case symphony.RoleBaselineDraft:
		return "baseline"
	case symphony.RoleBaselineVerification:
		return "baseline_verify"
	case symphony.RoleIteration:
		return "iteration"
	case symphony.RoleIntegration:
		return "integration"
	default:
		return string(role)
	}
}

func kickoffPrompt(work symphony.RuntimeWork) string {
	return fmt.Sprintf("开始 Pika Work `%s`。先按 System Prompt 完整读取并核对只读 Context Bundle，再开始工作；完成时必须调用 `%s`。",
		work.Work.ID, terminalOperation(work.Work.Role))
}

func logicalNameForRole(role symphony.WorkRole) (string, error) {
	switch role {
	case symphony.RoleBaselineDraft:
		return "baseline", nil
	case symphony.RoleBaselineVerification:
		return "baseline-verify", nil
	case symphony.RoleIteration:
		return "iteration", nil
	case symphony.RoleIntegration:
		return "integration", nil
	case symphony.RoleFollowUp:
		return "", nil
	default:
		return "", fmt.Errorf("unsupported activation role %q", role)
	}
}

func logicalNameForFollowUp(role symphony.WorkRole) (string, error) {
	switch role {
	case symphony.RoleBaselineVerification:
		return "follow-up/baseline-verify", nil
	case symphony.RoleIteration:
		return "follow-up/iteration", nil
	case symphony.RoleIntegration:
		return "follow-up/integration", nil
	default:
		return "", fmt.Errorf("unsupported Follow-up target role %q", role)
	}
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
