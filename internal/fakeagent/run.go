package fakeagent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/reyoung/pika-go/internal/benchmarkintegrity/testcontract"
	"github.com/reyoung/pika-go/internal/kdacontract"
	"github.com/reyoung/pika-go/internal/mcp"
)

func Run(ctx context.Context, input io.Reader, output io.Writer) int {
	return RunWithInterrupts(ctx, input, output, nil)
}

func RunWithInterrupts(ctx context.Context, input io.Reader, output io.Writer, interrupts <-chan os.Signal) int {
	if delayValue := os.Getenv("PIKA_GO_FAKE_AGENT_START_DELAY"); delayValue != "" {
		delay, err := time.ParseDuration(delayValue)
		if err != nil || delay < 0 {
			_, _ = fmt.Fprintf(output, "FAKE_AGENT_ERROR invalid startup delay %q\n", delayValue)
			return 1
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return 0
		case <-timer.C:
		}
	}
	setTitle(output, "FAKE_AGENT_READY")
	_, _ = fmt.Fprintln(output, "FAKE_AGENT_READY")
	var autorun func(context.Context, io.Writer)
	scenario := os.Getenv("PIKA_GO_FAKE_AGENT_AUTORUN")
	if strings.HasPrefix(scenario, "baseline-") {
		autorun = runBaselineAcceptedScenario
	} else if scenario == "optimization" {
		autorun = runOptimizationScenario
	}
	var autorunOnce sync.Once
	if autorun != nil && os.Getenv("PIKA_GO_FAKE_AGENT_AUTORUN_GATE") != "" {
		go autorunOnce.Do(func() { autorun(ctx, output) })
	}
	if initialPrompt := os.Getenv("PIKA_CODEX_INITIAL_PROMPT"); initialPrompt != "" {
		setTitle(output, "⠋ FAKE_AGENT_WORKING")
		_, _ = fmt.Fprintf(output, "FAKE_AGENT_PROMPT %s\n", strconv.Quote(initialPrompt))
		if os.Getenv("PIKA_GO_FAKE_AGENT_HANG_ON_PROMPT") != "1" {
			setTitle(output, "FAKE_AGENT_READY")
		}
		if autorun != nil {
			autorunOnce.Do(func() { go autorun(ctx, output) })
		}
	}
	lines := make(chan string)
	scanErrors := make(chan error, 1)
	go func() {
		scanner := bufio.NewScanner(input)
		for scanner.Scan() {
			select {
			case lines <- scanner.Text():
			case <-ctx.Done():
				return
			}
		}
		scanErrors <- scanner.Err()
		close(lines)
	}()
	var sleepTimer *time.Timer
	var sleepDone <-chan time.Time
	for {
		select {
		case <-ctx.Done():
			if sleepTimer != nil {
				sleepTimer.Stop()
			}
			return 0
		case <-interrupts:
			if sleepTimer != nil {
				sleepTimer.Stop()
				sleepTimer, sleepDone = nil, nil
			}
			setTitle(output, "FAKE_AGENT_READY")
			_, _ = fmt.Fprintln(output, "FAKE_AGENT_INTERRUPTED")
		case <-sleepDone:
			sleepTimer, sleepDone = nil, nil
			setTitle(output, "FAKE_AGENT_READY")
			_, _ = fmt.Fprintln(output, "FAKE_AGENT_AWAKE")
		case line, ok := <-lines:
			if !ok {
				if err := <-scanErrors; err != nil {
					_, _ = fmt.Fprintf(output, "FAKE_AGENT_ERROR %s\n", strconv.Quote(err.Error()))
					return 1
				}
				return 0
			}
			switch {
			case line == "/exit":
				return 0
			case strings.Contains(line, "/hang"):
				setTitle(output, "⠋ FAKE_AGENT_WORKING")
			case line == "/finish":
				setTitle(output, "⠋ FAKE_AGENT_WORKING")
				runBaselineAcceptedScenario(ctx, output)
				setTitle(output, "FAKE_AGENT_READY")
			case strings.HasPrefix(line, "/sleep "):
				setTitle(output, "⠋ FAKE_AGENT_WORKING")
				duration, err := time.ParseDuration(strings.TrimSpace(strings.TrimPrefix(line, "/sleep ")))
				if err != nil {
					_, _ = fmt.Fprintf(output, "FAKE_AGENT_ERROR %s\n", strconv.Quote(err.Error()))
					continue
				}
				if sleepTimer != nil {
					sleepTimer.Stop()
				}
				sleepTimer = time.NewTimer(duration)
				sleepDone = sleepTimer.C
			default:
				setTitle(output, "⠋ FAKE_AGENT_WORKING")
				_, _ = fmt.Fprintf(output, "FAKE_AGENT_PROMPT %s\n", strconv.Quote(line))
				if os.Getenv("PIKA_GO_FAKE_AGENT_HANG_ON_PROMPT") != "1" || line == schedulerResumeMessage {
					setTitle(output, "FAKE_AGENT_READY")
				}
				if autorun != nil {
					autorunOnce.Do(func() { go autorun(ctx, output) })
				}
			}
		}
	}
}

const schedulerResumeMessage = "继续"

func runOptimizationScenario(ctx context.Context, output io.Writer) {
	timer := time.NewTimer(300 * time.Millisecond)
	select {
	case <-ctx.Done():
		timer.Stop()
		return
	case <-timer.C:
	}
	role := os.Getenv("PIKA_ROLE")
	if role == "baseline_draft" || role == "baseline_verification" {
		runBaselineAcceptedScenario(ctx, output)
		return
	}
	switch role {
	case "diagnosis":
		runOptimizationDiagnosis(ctx, output)
	case "iteration":
		runOptimizationIteration(ctx, output)
	case "integration":
		runOptimizationIntegration(ctx, output)
	case "follow_up":
		runFollowUp(ctx, output)
	}
}

func runOptimizationDiagnosis(ctx context.Context, output io.Writer) {
	contents, err := os.ReadFile(os.Getenv("PIKA_CONTEXT_PATH"))
	if err != nil {
		_, _ = fmt.Fprintf(output, "FAKE_AGENT_AUTORUN_ERROR %q\n", err.Error())
		return
	}
	var contextValue struct {
		Baseline struct {
			ID string `json:"id"`
		} `json:"baseline"`
		Best struct {
			CommitSHA string `json:"commit_sha"`
		} `json:"best"`
		IterationCaseSet struct {
			CaseIDs []string `json:"case_ids"`
		} `json:"iteration_case_set"`
		Diagnosis struct {
			EvidenceRoot string `json:"evidence_root"`
		} `json:"diagnosis"`
	}
	if err := json.Unmarshal(contents, &contextValue); err != nil {
		_, _ = fmt.Fprintf(output, "FAKE_AGENT_AUTORUN_ERROR decode Diagnosis Context: %q\n", err)
		return
	}
	if contextValue.Baseline.ID == "" || contextValue.Best.CommitSHA == "" || len(contextValue.IterationCaseSet.CaseIDs) == 0 || contextValue.Diagnosis.EvidenceRoot == "" {
		_, _ = fmt.Fprintf(output, "FAKE_AGENT_AUTORUN_ERROR incomplete Diagnosis Context: baseline_id=%t best_sha=%t case_ids=%d evidence_root=%t\n", contextValue.Baseline.ID != "", contextValue.Best.CommitSHA != "", len(contextValue.IterationCaseSet.CaseIDs), contextValue.Diagnosis.EvidenceRoot != "")
		return
	}
	artifactPath := "profile.json"
	artifactContents := []byte("{\"fake_profile\":true}\n")
	if err := os.WriteFile(filepath.Join(contextValue.Diagnosis.EvidenceRoot, artifactPath), artifactContents, 0o600); err != nil {
		_, _ = fmt.Fprintf(output, "FAKE_AGENT_AUTORUN_ERROR %q\n", err.Error())
		return
	}
	report := kdacontract.DiagnosisReport{SchemaVersion: 1}
	report.Subject.BaselineRevisionID = contextValue.Baseline.ID
	report.Subject.BestSHA = contextValue.Best.CommitSHA
	report.Subject.Hardware = map[string]any{"fixture": "fake"}
	report.Subject.Software = map[string]any{"fixture": "fake"}
	report.Coverage.CaseIDs = contextValue.IterationCaseSet.CaseIDs
	report.Coverage.DispatchPaths = []string{"fake-kernel"}
	report.Artifacts = []kdacontract.ArtifactRef{{Path: artifactPath, Kind: "profile"}}
	report.Observations = []kdacontract.DiagnosisObservation{{ID: "observation-1", CaseIDs: contextValue.IterationCaseSet.CaseIDs, Metric: "latency", Value: 1.0, Unit: "ms", SourceArtifacts: []string{artifactPath}, Summary: "fake profile measured launch overhead"}}
	report.Bottlenecks = []kdacontract.DiagnosisBottleneck{{ID: "bottleneck-1", Class: "launch-overhead", Confidence: "high", ObservationIDs: []string{"observation-1"}, Summary: "fake launch overhead"}}
	report.Hypotheses = []kdacontract.DiagnosisHypothesis{{ID: "hypothesis-1", Rank: 1, Summary: "fuse fake launches", Mechanism: "remove one launch", TargetCaseIDs: contextValue.IterationCaseSet.CaseIDs, ExpectedEffect: "lower latency", Risk: "fixture correctness", BottleneckIDs: []string{"bottleneck-1"}, KnowledgeRefs: []string{}}}
	report.Limitations = []kdacontract.DiagnosisLimitation{}
	encoded, err := json.Marshal(report)
	if err != nil {
		_, _ = fmt.Fprintf(output, "FAKE_AGENT_AUTORUN_ERROR %q\n", err.Error())
		return
	}
	call := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"finish_diagnosis","arguments":{"idempotency_key":"fake-diagnosis-%s","outcome":"ready","report":%s}}}`, os.Getenv("PIKA_SESSION_ID"), encoded)
	response, err := callMCPUntilActive(ctx, call)
	if err != nil {
		_, _ = fmt.Fprintf(output, "FAKE_AGENT_AUTORUN_ERROR %q response=%q\n", err.Error(), response)
		return
	}
	_, _ = fmt.Fprintf(output, "FAKE_AGENT_AUTORUN_RESPONSE %s\n", strings.TrimSpace(response))
}

func runFollowUp(ctx context.Context, output io.Writer) {
	call := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"submit_followup_message","arguments":{"idempotency_key":"fake-follow-up-%s","message":"请继续当前工作并在证据完整后调用 required terminal MCP。"}}}`, os.Getenv("PIKA_SESSION_ID"))
	if response, err := callMCPUntilActive(ctx, call); err != nil {
		_, _ = fmt.Fprintf(output, "FAKE_AGENT_AUTORUN_ERROR %q response=%q\n", err.Error(), response)
	}
}

func runOptimizationIteration(ctx context.Context, output io.Writer) {
	attemptID := os.Getenv("PIKA_ATTEMPT_ID")
	round := os.Getenv("PIKA_ITERATION_ROUND")
	if os.Getenv("PIKA_ITERATION_KIND") == "initial" {
		position, ok := acquireFakePosition()
		if !ok || position > 3 {
			return
		}
		timer := time.NewTimer(time.Duration(3-position) * 8 * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
	name := "candidate-" + attemptID + "-round-" + round + ".txt"
	if err := os.WriteFile(name, []byte("optimized "+attemptID+" round "+round+"\n"), 0o600); err != nil {
		_, _ = fmt.Fprintf(output, "FAKE_AGENT_AUTORUN_ERROR %q\n", err.Error())
		return
	}
	commitCall := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"commit_changes","arguments":{"idempotency_key":"fake-commit-%s","message":"fake candidate %s round %s","paths":["%s"]}}}`, os.Getenv("PIKA_SESSION_ID"), attemptID, round, name)
	commitResponse, err := callMCPUntilActive(ctx, commitCall)
	if err != nil {
		_, _ = fmt.Fprintf(output, "FAKE_AGENT_AUTORUN_ERROR %q response=%q\n", err.Error(), commitResponse)
		return
	}
	commit, err := decodeScopedCommit(commitResponse)
	if err != nil {
		_, _ = fmt.Fprintf(output, "FAKE_AGENT_AUTORUN_ERROR %q response=%q\n", err.Error(), commitResponse)
		return
	}
	sha := commit.CommitSHA
	contextContents, err := os.ReadFile(os.Getenv("PIKA_CONTEXT_PATH"))
	if err != nil {
		_, _ = fmt.Fprintf(output, "FAKE_AGENT_AUTORUN_ERROR %q\n", err.Error())
		return
	}
	var contextValue struct {
		Work struct {
			CurrentCheckpointSHA string `json:"current_checkpoint_sha"`
		} `json:"work"`
		Iteration struct {
			EvidenceRoot    string `json:"evidence_root"`
			RequiredCaseSet struct {
				CaseIDs []string `json:"case_ids"`
			} `json:"required_case_set"`
		} `json:"iteration_context"`
	}
	if err := json.Unmarshal(contextContents, &contextValue); err != nil {
		_, _ = fmt.Fprintf(output, "FAKE_AGENT_AUTORUN_ERROR decode Iteration Context: %q\n", err)
		return
	}
	if commit.CurrentCheckpointSHA != "" {
		contextValue.Work.CurrentCheckpointSHA = commit.CurrentCheckpointSHA
	}
	if contextValue.Work.CurrentCheckpointSHA == "" || contextValue.Iteration.EvidenceRoot == "" || len(contextValue.Iteration.RequiredCaseSet.CaseIDs) == 0 {
		_, _ = fmt.Fprintf(output, "FAKE_AGENT_AUTORUN_ERROR incomplete Iteration Context: current_checkpoint_sha=%t evidence_root=%t case_ids=%d\n", contextValue.Work.CurrentCheckpointSHA != "", contextValue.Iteration.EvidenceRoot != "", len(contextValue.Iteration.RequiredCaseSet.CaseIDs))
		return
	}
	artifactPath := filepath.ToSlash(filepath.Join("experiments", os.Getenv("PIKA_SESSION_ID")+".json"))
	if err := os.MkdirAll(filepath.Dir(filepath.Join(contextValue.Iteration.EvidenceRoot, artifactPath)), 0o700); err != nil {
		_, _ = fmt.Fprintf(output, "FAKE_AGENT_AUTORUN_ERROR %q\n", err.Error())
		return
	}
	if err := os.WriteFile(filepath.Join(contextValue.Iteration.EvidenceRoot, artifactPath), []byte("{\"fake_benchmark\":true}\n"), 0o600); err != nil {
		_, _ = fmt.Fprintf(output, "FAKE_AGENT_AUTORUN_ERROR %q\n", err.Error())
		return
	}
	comparisons := make([]any, 0, len(contextValue.Iteration.RequiredCaseSet.CaseIDs))
	for _, caseID := range contextValue.Iteration.RequiredCaseSet.CaseIDs {
		comparisons = append(comparisons, map[string]any{"case_id": caseID, "reference": map[string]any{"latency": 10.0}, "candidate": map[string]any{"latency": 9.0}})
	}
	var correctness struct {
		BenchmarkIntegrity json.RawMessage `json:"benchmark_integrity"`
	}
	if err := json.Unmarshal(testcontract.Evidence(), &correctness); err != nil {
		_, _ = fmt.Fprintf(output, "FAKE_AGENT_AUTORUN_ERROR %q\n", err.Error())
		return
	}
	measurements, err := json.Marshal(map[string]any{"schema_version": 1, "comparisons": comparisons})
	if err != nil {
		_, _ = fmt.Fprintf(output, "FAKE_AGENT_AUTORUN_ERROR %q\n", err.Error())
		return
	}
	experiment := kdacontract.Experiment{
		SchemaVersion: 1, ParentCheckpointSHA: contextValue.Work.CurrentCheckpointSHA,
		Hypothesis: kdacontract.ExperimentHypothesis{DiagnosisHypothesisID: "hypothesis-1", Summary: "fake fusion"},
		Change:     kdacontract.ExperimentChange{Summary: "fake candidate", Paths: []string{name}, Mechanism: "reduce fake latency"},
		Outcome:    "kept", CheckpointSHA: sha,
		Correctness:           &kdacontract.ExperimentCorrectness{BenchmarkIntegrity: correctness.BenchmarkIntegrity},
		BenchmarkMeasurements: measurements,
		Artifacts:             []kdacontract.ArtifactRef{{Path: artifactPath, Kind: "benchmark"}}, Summary: "fake kept experiment",
	}
	experimentJSON, err := json.Marshal(experiment)
	if err != nil {
		_, _ = fmt.Fprintf(output, "FAKE_AGENT_AUTORUN_ERROR %q\n", err.Error())
		return
	}
	recordCall := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"record_iteration_experiment","arguments":{"idempotency_key":"fake-experiment-%s","experiment":%s}}}`, os.Getenv("PIKA_SESSION_ID"), experimentJSON)
	recordResponse, err := callMCPUntilActive(ctx, recordCall)
	if err != nil {
		_, _ = fmt.Fprintf(output, "FAKE_AGENT_AUTORUN_ERROR %q response=%q\n", err.Error(), recordResponse)
		return
	}
	experimentID, err := recordedExperimentID(recordResponse)
	if err != nil {
		_, _ = fmt.Fprintf(output, "FAKE_AGENT_AUTORUN_ERROR %q response=%q\n", err.Error(), recordResponse)
		return
	}
	call := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"finish_iteration","arguments":{"idempotency_key":"fake-iteration-%s","outcome":"candidate","experiment_id":"%s","candidate_sha":"%s","summary":"fake improvement round %s","evidence":%s}}}`, os.Getenv("PIKA_SESSION_ID"), experimentID, sha, round, testcontract.Evidence())
	if response, err := callMCPUntilActive(ctx, call); err != nil {
		_, _ = fmt.Fprintf(output, "FAKE_AGENT_AUTORUN_ERROR %q response=%q\n", err.Error(), response)
	}
}

func recordedExperimentID(response string) (string, error) {
	text, err := decodeMCPText(response)
	if err != nil {
		return "", err
	}
	var receipt struct {
		Result struct {
			ExperimentID string `json:"experiment_id"`
		} `json:"result"`
	}
	if err := json.Unmarshal(text, &receipt); err != nil {
		return "", fmt.Errorf("decode Experiment receipt: %w", err)
	}
	if receipt.Result.ExperimentID == "" {
		return "", errors.New("decode Experiment receipt: experiment_id is missing")
	}
	return receipt.Result.ExperimentID, nil
}

func runOptimizationIntegration(ctx context.Context, output io.Writer) {
	intentID, err := pendingIntentFromContext(os.Getenv("PIKA_CONTEXT_PATH"))
	if err != nil {
		_, _ = fmt.Fprintf(output, "FAKE_AGENT_AUTORUN_ERROR %q\n", err.Error())
		return
	}
	if intentID == "" {
		prepare := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"prepare_best_update","arguments":{"idempotency_key":"fake-prepare-%s","validation":%s}}}`, os.Getenv("PIKA_SESSION_ID"), testcontract.Validation())
		response, callErr := callMCPUntilActive(ctx, prepare)
		err = callErr
		if err != nil {
			_, _ = fmt.Fprintf(output, "FAKE_AGENT_AUTORUN_ERROR %q response=%q\n", err.Error(), response)
			return
		}
		intentID, err = preparedIntentID(response)
		if err != nil {
			_, _ = fmt.Fprintf(output, "FAKE_AGENT_AUTORUN_ERROR %q response=%q\n", err.Error(), response)
			return
		}
	}
	applyCall := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"apply_best_update","arguments":{"intent_id":"%s","message":"fake accepted candidate"}}}`, intentID)
	applyOutput, err := callMCPUntilActive(ctx, applyCall)
	if err != nil {
		_, _ = fmt.Fprintf(output, "FAKE_AGENT_AUTORUN_ERROR %q output=%q\n", err.Error(), applyOutput)
		return
	}
	appliedSHA, err := appliedBestSHA(applyOutput)
	if err != nil {
		_, _ = fmt.Fprintf(output, "FAKE_AGENT_AUTORUN_ERROR decode-apply=%q output=%q\n", err, applyOutput)
		return
	}
	if delayValue := os.Getenv("PIKA_GO_FAKE_AFTER_BEST_UPDATE_DELAY"); delayValue != "" {
		delay, delayErr := time.ParseDuration(delayValue)
		if delayErr != nil || delay < 0 {
			_, _ = fmt.Fprintf(output, "FAKE_AGENT_AUTORUN_ERROR invalid Best update delay %q\n", delayValue)
			return
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return
		case <-timer.C:
		}
	}
	finish := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"finish_integration","arguments":{"idempotency_key":"fake-integration-%s","outcome":"accepted","intent_id":"%s","applied_sha":"%s","result":{"verified":true}}}}`, os.Getenv("PIKA_SESSION_ID"), intentID, appliedSHA)
	if response, err := callMCPUntilActive(ctx, finish); err != nil {
		_, _ = fmt.Fprintf(output, "FAKE_AGENT_AUTORUN_ERROR %q response=%q\n", err.Error(), response)
	}
}

func pendingIntentFromContext(path string) (string, error) {
	if path == "" {
		return "", errors.New("PIKA_CONTEXT_PATH is required")
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read Pika Context Bundle: %w", err)
	}
	var value struct {
		Work struct {
			GitIntentID    string `json:"git_intent_id"`
			GitIntentState string `json:"git_intent_state"`
		} `json:"work"`
	}
	if err := json.Unmarshal(contents, &value); err != nil {
		return "", fmt.Errorf("decode Pika Integration context: %w", err)
	}
	if value.Work.GitIntentID == "" && value.Work.GitIntentState == "" {
		return "", nil
	}
	if value.Work.GitIntentID == "" || value.Work.GitIntentState != "pending" {
		return "", fmt.Errorf("inconsistent Git intent context: id=%q state=%q", value.Work.GitIntentID, value.Work.GitIntentState)
	}
	return value.Work.GitIntentID, nil
}

func acquireFakePosition() (int, bool) {
	directory := os.Getenv("PIKA_GO_FAKE_COUNTER_DIR")
	if directory == "" {
		return 0, false
	}
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return 0, false
	}
	for position := 1; position <= 64; position++ {
		path := filepath.Join(directory, fmt.Sprintf("initial-%02d", position))
		file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			_ = file.Close()
			return position, true
		}
		if !os.IsExist(err) {
			return 0, false
		}
	}
	return 0, false
}

func preparedIntentID(response string) (string, error) {
	text, err := decodeMCPText(response)
	if err != nil {
		return "", err
	}
	var value struct {
		GitIntent struct {
			ID string `json:"id"`
		} `json:"git_intent"`
	}
	if err := json.Unmarshal(text, &value); err != nil {
		return "", fmt.Errorf("decode Git intent: %w", err)
	}
	if value.GitIntent.ID == "" {
		return "", errors.New("decode Git intent: id is missing")
	}
	return value.GitIntent.ID, nil
}

func appliedBestSHA(response string) (string, error) {
	text, err := decodeMCPText(response)
	if err != nil {
		return "", err
	}
	var value struct {
		AppliedSHA string `json:"applied_sha"`
	}
	if err := json.Unmarshal(text, &value); err != nil {
		return "", fmt.Errorf("decode applied Best SHA: %w", err)
	}
	if value.AppliedSHA == "" {
		return "", errors.New("decode applied Best SHA: applied_sha is missing")
	}
	return value.AppliedSHA, nil
}

type scopedCommitResult struct {
	CommitSHA            string `json:"commit_sha"`
	Clean                bool   `json:"clean"`
	CurrentCheckpointSHA string `json:"current_checkpoint_sha"`
}

func decodeScopedCommit(response string) (scopedCommitResult, error) {
	text, err := decodeMCPText(response)
	if err != nil {
		return scopedCommitResult{}, err
	}
	var value scopedCommitResult
	if err := json.Unmarshal(text, &value); err != nil {
		return scopedCommitResult{}, fmt.Errorf("decode clean scoped commit: %w", err)
	}
	if value.CommitSHA == "" || !value.Clean || value.CurrentCheckpointSHA == "" {
		return scopedCommitResult{}, errors.New("decode clean scoped commit: commit_sha/current_checkpoint_sha is missing or worktree is dirty")
	}
	return value, nil
}

func decodeMCPText(response string) ([]byte, error) {
	var envelope struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(response), &envelope); err != nil {
		return nil, fmt.Errorf("decode MCP envelope: %w", err)
	}
	if len(envelope.Result.Content) == 0 || strings.TrimSpace(envelope.Result.Content[0].Text) == "" {
		return nil, errors.New("decode MCP envelope: text content is missing")
	}
	return []byte(envelope.Result.Content[0].Text), nil
}

func callMCPUntilActive(ctx context.Context, call string) (string, error) {
	last := ""
	for attempt := 0; attempt < 100; attempt++ {
		var response strings.Builder
		if err := mcp.RunProxy(ctx, os.Getenv("PIKA_GO_SOCKET"), os.Getenv("PIKA_MCP_GRANT"), strings.NewReader(call+"\n"), &response); err != nil {
			return response.String(), err
		}
		last = strings.TrimSpace(response.String())
		if !strings.Contains(last, `"error"`) {
			return last, nil
		}
		select {
		case <-ctx.Done():
			return last, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	return last, fmt.Errorf("session never became active")
}

func runBaselineAcceptedScenario(ctx context.Context, output io.Writer) {
	if gate := os.Getenv("PIKA_GO_FAKE_AGENT_AUTORUN_GATE"); gate != "" {
		ticker := time.NewTicker(25 * time.Millisecond)
		defer ticker.Stop()
		for {
			if _, err := os.Stat(gate); err == nil {
				break
			} else if !errors.Is(err, os.ErrNotExist) {
				_, _ = fmt.Fprintf(output, "FAKE_AGENT_AUTORUN_ERROR %q\n", err.Error())
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}
	timer := time.NewTimer(300 * time.Millisecond)
	select {
	case <-ctx.Done():
		timer.Stop()
		return
	case <-timer.C:
	}
	var call string
	scenario := os.Getenv("PIKA_GO_FAKE_AGENT_AUTORUN")
	sessionID := os.Getenv("PIKA_SESSION_ID")
	switch os.Getenv("PIKA_ROLE") {
	case "baseline_draft":
		if scenario == "baseline-rejected" && os.Getenv("PIKA_BASELINE_NUMBER") != "1" {
			return
		}
		definition := testcontract.Definition()
		if os.Getenv("PIKA_GO_FAKE_AGENT_SCHEMA20_BASELINE") == "1" {
			var value map[string]any
			if err := json.Unmarshal(definition, &value); err != nil {
				_, _ = fmt.Fprintf(output, "FAKE_AGENT_AUTORUN_ERROR %q\n", err.Error())
				return
			}
			if measurements, ok := value["benchmark_measurements"].(map[string]any); ok {
				delete(measurements, "iteration_performance_gate")
			}
			var err error
			definition, err = json.Marshal(value)
			if err != nil {
				_, _ = fmt.Fprintf(output, "FAKE_AGENT_AUTORUN_ERROR %q\n", err.Error())
				return
			}
		}
		call = fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"submit_baseline_definition","arguments":{"idempotency_key":"fake-baseline-submit-%s","definition":%s}}}`, sessionID, definition)
	case "baseline_verification":
		if scenario == "baseline-follow-up" {
			return
		}
		if scenario == "baseline-rejected" {
			call = fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"finish_baseline_verification","arguments":{"idempotency_key":"fake-baseline-verify-%s","decision":"rejected","failure_kind":"missing_case","reason":"coverage is incomplete","requested_changes":"add the missing case","evidence":{"scenario":"fake"}}}}`, sessionID)
		} else {
			call = fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"finish_baseline_verification","arguments":{"idempotency_key":"fake-baseline-verify-%s","decision":"accepted","evidence":%s}}}`, sessionID, testcontract.Evidence())
		}
	case "follow_up":
		runFollowUp(ctx, output)
		return
	case "diagnosis":
		runOptimizationDiagnosis(ctx, output)
		return
	default:
		return
	}
	lastResponse := ""
	_, _ = fmt.Fprintf(output, "FAKE_AGENT_MCP_START role=%q socket=%q grant_present=%t session=%q\n",
		os.Getenv("PIKA_ROLE"), os.Getenv("PIKA_GO_SOCKET"), os.Getenv("PIKA_MCP_GRANT") != "", sessionID)
	for attempt := 0; attempt < 100; attempt++ {
		var response strings.Builder
		if err := mcp.RunProxy(ctx, os.Getenv("PIKA_GO_SOCKET"), os.Getenv("PIKA_MCP_GRANT"), strings.NewReader(call+"\n"), &response); err != nil {
			_, _ = fmt.Fprintf(output, "FAKE_AGENT_MCP_ERROR %q socket=%q role=%q grant_present=%t\n", err.Error(), os.Getenv("PIKA_GO_SOCKET"), os.Getenv("PIKA_ROLE"), os.Getenv("PIKA_MCP_GRANT") != "")
			return
		}
		_, _ = fmt.Fprintf(output, "FAKE_AGENT_MCP_RESPONSE %s\n", strings.TrimSpace(response.String()))
		if !strings.Contains(response.String(), `"error"`) {
			_, _ = fmt.Fprintf(output, "FAKE_AGENT_AUTORUN_RESPONSE %s\n", strings.TrimSpace(response.String()))
			return
		}
		lastResponse = strings.TrimSpace(response.String())
		select {
		case <-ctx.Done():
			return
		case <-time.After(100 * time.Millisecond):
		}
	}
	_, _ = fmt.Fprintf(output, "FAKE_AGENT_AUTORUN_ERROR session never became active: %s\n", lastResponse)
}

func setTitle(output io.Writer, title string) {
	if os.Getenv("PIKA_GO_FAKE_AGENT_OSC") == "1" {
		_, _ = fmt.Fprintf(output, "\x1b]0;%s\x07", title)
	}
}
