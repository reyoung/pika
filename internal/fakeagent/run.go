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
	if strings.HasPrefix(os.Getenv("PIKA_GO_FAKE_AGENT_AUTORUN"), "baseline-") {
		autorun = runBaselineAcceptedScenario
	} else if os.Getenv("PIKA_GO_FAKE_AGENT_AUTORUN") == "optimization" {
		autorun = runOptimizationScenario
	}
	var autorunOnce sync.Once
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
	case "iteration":
		runOptimizationIteration(ctx, output)
	case "integration":
		runOptimizationIntegration(ctx, output)
	case "follow_up":
		runFollowUp(ctx, output)
	}
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
	sha, err := committedSHA(commitResponse)
	if err != nil {
		_, _ = fmt.Fprintf(output, "FAKE_AGENT_AUTORUN_ERROR %q response=%q\n", err.Error(), commitResponse)
		return
	}
	call := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"finish_iteration","arguments":{"idempotency_key":"fake-iteration-%s","outcome":"candidate","candidate_sha":"%s","summary":"fake improvement round %s","evidence":{"scenario":"optimization"}}}}`, os.Getenv("PIKA_SESSION_ID"), sha, round)
	if response, err := callMCPUntilActive(ctx, call); err != nil {
		_, _ = fmt.Fprintf(output, "FAKE_AGENT_AUTORUN_ERROR %q response=%q\n", err.Error(), response)
	}
}

func runOptimizationIntegration(ctx context.Context, output io.Writer) {
	intentID, err := pendingIntentFromContext(os.Getenv("PIKA_CONTEXT_PATH"))
	if err != nil {
		_, _ = fmt.Fprintf(output, "FAKE_AGENT_AUTORUN_ERROR %q\n", err.Error())
		return
	}
	if intentID == "" {
		prepare := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"prepare_best_update","arguments":{"idempotency_key":"fake-prepare-%s","validation":{"guard":"passed"}}}}`, os.Getenv("PIKA_SESSION_ID"))
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
	var envelope struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(response), &envelope); err != nil || len(envelope.Result.Content) == 0 {
		return "", fmt.Errorf("decode MCP envelope: %w", err)
	}
	var value struct {
		GitIntent struct {
			ID string `json:"id"`
		} `json:"git_intent"`
	}
	if err := json.Unmarshal([]byte(envelope.Result.Content[0].Text), &value); err != nil || value.GitIntent.ID == "" {
		return "", fmt.Errorf("decode Git intent: %w", err)
	}
	return value.GitIntent.ID, nil
}

func appliedBestSHA(response string) (string, error) {
	var envelope struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(response), &envelope); err != nil || len(envelope.Result.Content) == 0 {
		return "", fmt.Errorf("decode MCP envelope: %w", err)
	}
	var value struct {
		AppliedSHA string `json:"applied_sha"`
	}
	if err := json.Unmarshal([]byte(envelope.Result.Content[0].Text), &value); err != nil || value.AppliedSHA == "" {
		return "", fmt.Errorf("decode applied Best SHA: %w", err)
	}
	return value.AppliedSHA, nil
}

func committedSHA(response string) (string, error) {
	var envelope struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(response), &envelope); err != nil || len(envelope.Result.Content) == 0 {
		return "", fmt.Errorf("decode MCP envelope: %w", err)
	}
	var value struct {
		CommitSHA string `json:"commit_sha"`
		Clean     bool   `json:"clean"`
	}
	if err := json.Unmarshal([]byte(envelope.Result.Content[0].Text), &value); err != nil || value.CommitSHA == "" || !value.Clean {
		return "", fmt.Errorf("decode clean scoped commit: %w", err)
	}
	return value.CommitSHA, nil
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
		call = fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"submit_baseline_definition","arguments":{"idempotency_key":"fake-baseline-submit-%s","definition":{"target":"fake-kernel","development_baseline":"main","cases":[{"name":"smoke","critical":true}],"oracle":{"kind":"exact"},"metric":"latency_ms","aggregation":"median","regression_guard":0.05,"harness":"fake","repeat":3,"stop_condition":"verified"}}}}`, sessionID)
	case "baseline_verification":
		if scenario == "baseline-follow-up" {
			return
		}
		if scenario == "baseline-rejected" {
			call = fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"finish_baseline_verification","arguments":{"idempotency_key":"fake-baseline-verify-%s","decision":"rejected","failure_kind":"missing_case","reason":"coverage is incomplete","requested_changes":"add the missing case","evidence":{"scenario":"fake"}}}}`, sessionID)
		} else {
			call = fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"finish_baseline_verification","arguments":{"idempotency_key":"fake-baseline-verify-%s","decision":"accepted","evidence":{"scenario":"fake"}}}}`, sessionID)
		}
	case "follow_up":
		runFollowUp(ctx, output)
		return
	default:
		return
	}
	lastResponse := ""
	for attempt := 0; attempt < 100; attempt++ {
		var response strings.Builder
		if err := mcp.RunProxy(ctx, os.Getenv("PIKA_GO_SOCKET"), os.Getenv("PIKA_MCP_GRANT"), strings.NewReader(call+"\n"), &response); err != nil {
			_, _ = fmt.Fprintf(output, "FAKE_AGENT_AUTORUN_ERROR %q socket=%q role=%q grant=%t\n", err.Error(), os.Getenv("PIKA_GO_SOCKET"), os.Getenv("PIKA_ROLE"), os.Getenv("PIKA_MCP_GRANT") != "")
			return
		}
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
