package testdriver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/reyoung/pika-go/internal/control"
	"github.com/reyoung/pika-go/internal/instance"
	_ "modernc.org/sqlite"
)

func Run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printUsage(stderr)
		return 2
	}
	switch args[0] {
	case "wait-health":
		return runWaitHealth(ctx, args[1:], stdout, stderr)
	case "kill-at":
		return runKillAt(ctx, args[1:], stdout, stderr)
	default:
		_, _ = fmt.Fprintf(stderr, "unknown test-driver command %q\n", args[0])
		printUsage(stderr)
		return 2
	}
}

func runWaitHealth(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("wait-health", flag.ContinueOnError)
	flags.SetOutput(stderr)
	socketFlag := flags.String("socket", "", "Unix socket path")
	timeout := flags.Duration("timeout", 3*time.Second, "Maximum wait")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if *timeout <= 0 {
		_, _ = fmt.Fprintln(stderr, "wait-health: timeout must be positive")
		return 2
	}
	socketPath, err := instance.ResolveSocketContext(ctx, *socketFlag)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "wait-health: resolve socket: %v\n", err)
		return 2
	}
	waitCtx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	for {
		health, healthErr := control.Health(waitCtx, socketPath)
		if healthErr == nil {
			if err := json.NewEncoder(stdout).Encode(health); err != nil {
				_, _ = fmt.Fprintf(stderr, "wait-health: encode health: %v\n", err)
				return 1
			}
			return 0
		}
		select {
		case <-waitCtx.Done():
			_, _ = fmt.Fprintf(stderr, "wait-health: %v (last error: %v)\n", waitCtx.Err(), healthErr)
			return 1
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func runKillAt(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("kill-at", flag.ContinueOnError)
	flags.SetOutput(stderr)
	pid := flags.Int("pid", 0, "Process ID to terminate")
	databasePath := flags.String("database", "", "Absolute Pika SQLite path")
	point := flags.String("point", "", "Crash point")
	effectType := flags.String("effect-type", "", "Runtime effect type")
	requestID := flags.String("request-id", "", "Terminal operation request ID")
	workID := flags.String("work-id", "", "Work ID")
	timeout := flags.Duration("timeout", 10*time.Second, "Maximum wait")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if *pid <= 1 || *pid == os.Getpid() {
		_, _ = fmt.Fprintln(stderr, "kill-at: pid must identify another non-system process")
		return 2
	}
	if *databasePath == "" || !filepath.IsAbs(*databasePath) {
		_, _ = fmt.Fprintln(stderr, "kill-at: database must be an absolute path")
		return 2
	}
	validPoint := (*point == "outbox-dispatching" && *effectType != "") ||
		(*point == "terminal-committed" && *requestID != "") ||
		(*point == "agent-session-running" && *workID != "") || *point == "git-intent-applied"
	if !validPoint {
		_, _ = fmt.Fprintln(stderr, "kill-at: outbox-dispatching requires --effect-type; terminal-committed requires --request-id; agent-session-running requires --work-id; git-intent-applied takes no selector")
		return 2
	}
	if *timeout <= 0 {
		_, _ = fmt.Fprintln(stderr, "kill-at: timeout must be positive")
		return 2
	}
	database, err := sql.Open("sqlite", *databasePath)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "kill-at: open database: %v\n", err)
		return 1
	}
	defer database.Close()
	database.SetMaxOpenConns(1)
	waitCtx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	for {
		var reached bool
		var reachedIntentID string
		var inspectErr error
		if *point == "outbox-dispatching" {
			inspectErr = database.QueryRowContext(waitCtx, `SELECT EXISTS(
				SELECT 1 FROM runtime_outbox WHERE status = 'dispatching' AND effect_type = ?
			)`, *effectType).Scan(&reached)
		} else if *point == "terminal-committed" {
			inspectErr = database.QueryRowContext(waitCtx, `SELECT EXISTS(
				SELECT 1 FROM operation_receipts WHERE request_id = ? AND command_type IN (
					'submit_baseline_definition', 'finish_baseline_verification', 'finish_iteration',
					'finish_integration', 'submit_followup_message'
				)
			)`, *requestID).Scan(&reached)
		} else if *point == "agent-session-running" {
			inspectErr = database.QueryRowContext(waitCtx, `SELECT EXISTS(
				SELECT 1 FROM agent_sessions WHERE work_id = ? AND status = 'running'
			)`, *workID).Scan(&reached)
		} else {
			reached, reachedIntentID, inspectErr = gitIntentApplied(waitCtx, database)
		}
		if inspectErr == nil && reached {
			process, findErr := os.FindProcess(*pid)
			if findErr != nil {
				_, _ = fmt.Fprintf(stderr, "kill-at: find process: %v\n", findErr)
				return 1
			}
			if killErr := process.Kill(); killErr != nil {
				_, _ = fmt.Fprintf(stderr, "kill-at: terminate process: %v\n", killErr)
				return 1
			}
			_ = json.NewEncoder(stdout).Encode(map[string]any{
				"pid": *pid, "point": *point, "effect_type": *effectType, "request_id": *requestID, "work_id": *workID,
				"intent_id": reachedIntentID,
			})
			return 0
		}
		if inspectErr != nil && !isDatabaseNotReady(inspectErr) {
			_, _ = fmt.Fprintf(stderr, "kill-at: inspect crash point: %v\n", inspectErr)
			return 1
		}
		select {
		case <-waitCtx.Done():
			_, _ = fmt.Fprintf(stderr, "kill-at: %v waiting for %s\n", waitCtx.Err(), *point)
			return 1
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func gitIntentApplied(ctx context.Context, database *sql.DB) (bool, string, error) {
	var repository, intentID, expectedBestSHA string
	err := database.QueryRowContext(ctx, `SELECT o.repository, g.id, g.expected_best_sha
		FROM git_intents g CROSS JOIN optimizations o
		WHERE g.state = 'pending' ORDER BY g.created_at LIMIT 1`).Scan(&repository, &intentID, &expectedBestSHA)
	if errors.Is(err, sql.ErrNoRows) {
		return false, "", nil
	}
	if err != nil {
		return false, "", err
	}
	if !filepath.IsAbs(repository) {
		return false, "", errors.New("Optimization repository is not absolute")
	}
	currentOutput, err := exec.CommandContext(ctx, "git", "-C", repository, "rev-parse", "refs/heads/pika/best").Output()
	if err != nil {
		return false, "", fmt.Errorf("read pika/best for Git intent %s: %w", intentID, err)
	}
	current := strings.TrimSpace(string(currentOutput))
	if current == "" || current == expectedBestSHA {
		return false, "", nil
	}
	trailerOutput, err := exec.CommandContext(ctx, "git", "-C", repository, "show", "-s", "--format=%(trailers:key=Pika-Intent,valueonly)", current).Output()
	if err != nil {
		return false, "", fmt.Errorf("read Best Git intent trailer: %w", err)
	}
	if strings.TrimSpace(string(trailerOutput)) != intentID {
		return false, "", nil
	}
	return true, intentID, nil
}

func isDatabaseNotReady(err error) bool {
	return strings.Contains(err.Error(), "no such table:")
}

func printUsage(writer io.Writer) {
	_, _ = fmt.Fprintln(writer, "usage: pika-go-test-driver <wait-health|kill-at> [options]")
}
