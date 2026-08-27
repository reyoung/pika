package configuration_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/reyoung/pika-go/internal/configuration"
)

func TestLoadAgentReadsStaticRoleConfiguration(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.toml")
	contents := `[optimization]
repository = "/repo"

[agents.iteration]
kind = "codex"
model = "gpt-test"
reasoning_effort = "xhigh"
`
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := configuration.LoadAgent(path, "iteration")
	if err != nil {
		t.Fatal(err)
	}
	if config.Kind != "codex" || config.Model != "gpt-test" || config.ReasoningEffort != "xhigh" {
		t.Fatalf("agent config = %+v", config)
	}
}

func TestLoadSchedulerReadsConcurrencyAndQueueLimit(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.toml")
	contents := `[scheduler]
iteration_concurrency = 2
max_pending_attempts = 6
`
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := configuration.LoadScheduler(path)
	if err != nil {
		t.Fatal(err)
	}
	if config.IterationConcurrency != 2 || config.MaxPendingAttempts != 6 {
		t.Fatalf("scheduler config = %+v", config)
	}
}

func TestLoadSchedulerRejectsMissingUnknownOrNonPositiveFields(t *testing.T) {
	t.Parallel()
	for name, contents := range map[string]string{
		"missing":       "[scheduler]\niteration_concurrency = 2\n",
		"unknown":       "[scheduler]\niteration_concurrency = 2\nmax_pending_attempts = 6\nunsafe = 1\n",
		"zero":          "[scheduler]\niteration_concurrency = 0\nmax_pending_attempts = 6\n",
		"not-an-int":    "[scheduler]\niteration_concurrency = two\nmax_pending_attempts = 6\n",
		"duplicate-key": "[scheduler]\niteration_concurrency = 2\niteration_concurrency = 3\nmax_pending_attempts = 6\n",
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := configuration.LoadScheduler(path); err == nil {
				t.Fatal("invalid scheduler configuration was accepted")
			}
		})
	}
}

func TestLoadPaneIdleTimeout(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("[follow_up]\npane_idle_timeout = \"7m30s\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	duration, err := configuration.LoadPaneIdleTimeout(path)
	if err != nil || duration != 7*time.Minute+30*time.Second {
		t.Fatalf("duration=%v err=%v", duration, err)
	}
}

func TestLoadFollowUpReadsSeparateTargetAndGeneratorBudgets(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.toml")
	contents := `[follow_up]
pane_idle_timeout = "7m30s"
generator_concurrency = 1

[follow_up.baseline_verify]
max_messages = 8
generator_max_attempts = 3

[follow_up.iteration]
max_messages = 5
generator_max_attempts = 2

[follow_up.integration]
max_messages = 6
generator_max_attempts = 4
`
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := configuration.LoadFollowUp(path)
	if err != nil {
		t.Fatal(err)
	}
	if config.PaneIdleTimeout != 7*time.Minute+30*time.Second || config.GeneratorConcurrency != 1 {
		t.Fatalf("global Follow-up config = %+v", config)
	}
	if config.BaselineVerify.MaxMessages != 8 || config.BaselineVerify.GeneratorMaxAttempts != 3 ||
		config.Iteration.MaxMessages != 5 || config.Iteration.GeneratorMaxAttempts != 2 ||
		config.Integration.MaxMessages != 6 || config.Integration.GeneratorMaxAttempts != 4 {
		t.Fatalf("Follow-up policies = %+v", config)
	}
}

func TestLoadFollowUpRejectsUnknownGlobalField(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.toml")
	contents := `[follow_up]
pane_idle_timeout = "5m"
generator_concurrency = 1
silently_unsafe = true

[follow_up.baseline_verify]
max_messages = 8
generator_max_attempts = 3

[follow_up.iteration]
max_messages = 5
generator_max_attempts = 3

[follow_up.integration]
max_messages = 8
generator_max_attempts = 3
`
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := configuration.LoadFollowUp(path); err == nil {
		t.Fatal("unknown global Follow-up field was accepted")
	}
}

func TestLoadAgentRejectsUnknownOrInvalidAgentFields(t *testing.T) {
	t.Parallel()
	for name, contents := range map[string]string{
		"unknown": `[agents.iteration]
kind = "codex"
surprise = "unsafe"
`,
		"unsupported-kind": `[agents.iteration]
kind = "cursor"
model = "model"
reasoning_effort = "high"
`,
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := configuration.LoadAgent(path, "iteration"); err == nil {
				t.Fatal("invalid agent configuration was accepted")
			}
		})
	}
}
