package configuration_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/reyoung/pika-go/internal/configuration"
)

func TestLoadIterationAgentsUsesOrderedArrayAsConcurrency(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.toml")
	contents := `[scheduler]
max_pending_attempts = 8

[[agents.iteration]]
kind = "codex"
model = "gpt-codex"
reasoning_effort = "high"

[[agents.iteration]]
kind = "cursor"
model = "gpt-cursor-1"
reasoning_effort = "high"
args = ["--force"]

[[agents.iteration]]
kind = "cursor"
model = "gpt-cursor-2"
reasoning_effort = "max"
args = ["--auto-review"]
`
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	agents, err := configuration.LoadIterationAgents(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(agents) != 3 || agents[0].Kind != "codex" || agents[1].Kind != "cursor" || agents[2].Model != "gpt-cursor-2" {
		t.Fatalf("iteration agents = %+v", agents)
	}
	selected, err := configuration.LoadAgentForWork(path, "iteration", 1)
	if err != nil {
		t.Fatal(err)
	}
	if selected.Kind != "cursor" || selected.Model != "gpt-cursor-1" {
		t.Fatalf("slot 1 Agent = %+v", selected)
	}
	if _, err := configuration.LoadAgentForWork(path, "iteration", 3); err == nil {
		t.Fatal("out-of-range iteration slot was accepted")
	}
}

func TestLoadIterationAgentsExpandsLegacySingletonConcurrency(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.toml")
	contents := `[scheduler]
iteration_concurrency = 2
max_pending_attempts = 8

[agents.iteration]
kind = "codex"
model = "gpt-legacy"
reasoning_effort = "high"
`
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	agents, err := configuration.LoadIterationAgents(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(agents) != 2 || agents[0].Kind != agents[1].Kind || agents[0].Model != agents[1].Model || agents[0].Model != "gpt-legacy" {
		t.Fatalf("legacy iteration agents = %+v", agents)
	}
}

func TestRenderedConfigurationDerivesConcurrencyFromOneIterationAgent(t *testing.T) {
	t.Parallel()
	contents, err := configuration.RenderConfiguration("/repo", configuration.DefaultAgents())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(contents, "iteration_concurrency") {
		t.Fatalf("rendered configuration still has independent concurrency:\n%s", contents)
	}
	if strings.Count(contents, "[[agents.iteration]]") != 1 {
		t.Fatalf("rendered configuration does not contain one Iteration Agent:\n%s", contents)
	}
	schedulerPath := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(schedulerPath, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	scheduler, err := configuration.LoadScheduler(schedulerPath)
	if err != nil {
		t.Fatal(err)
	}
	if scheduler.IterationConcurrency != 0 || scheduler.MaxPendingAttempts != 8 {
		t.Fatalf("scheduler = %+v", scheduler)
	}
}

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

func TestLoadAgentReadsCursorConfigurationAndArgs(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.toml")
	contents := `[agents.iteration]
kind = "cursor"
model = "gpt-5.6-sol"
reasoning_effort = "max"
args = ["--force", "--approve-mcps", "--trust", "--header", "X-Test: value"]
`
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := configuration.LoadAgent(path, "iteration")
	if err != nil {
		t.Fatal(err)
	}
	if config.Kind != "cursor" || config.Model != "gpt-5.6-sol" || config.ReasoningEffort != "max" ||
		len(config.Args) != 5 || config.Args[4] != "X-Test: value" {
		t.Fatalf("agent config = %+v", config)
	}
}

func TestLoadAgentAcceptsExplicitProviderDefaults(t *testing.T) {
	t.Parallel()
	for name, contents := range map[string]string{
		"codex": `[agents.iteration]
kind = "codex"
model = ""
reasoning_effort = ""
`,
		"cursor-auto-routing": `[agents.iteration]
kind = "cursor"
model = "auto"
reasoning_effort = ""
`,
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := configuration.LoadAgent(path, "iteration"); err != nil {
				t.Fatalf("load provider default: %v", err)
			}
		})
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

func TestLoadContextReadsIterationHistoryLimit(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("[context.iteration]\nhistory_limit = 0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := configuration.LoadContext(path)
	if err != nil {
		t.Fatal(err)
	}
	if config.IterationHistoryLimit != 0 {
		t.Fatalf("context config = %+v", config)
	}
}

func TestLoadContextDefaultsMissingIterationHistoryLimit(t *testing.T) {
	t.Parallel()
	for name, contents := range map[string]string{
		"missing-section": "[scheduler]\niteration_concurrency = 2\nmax_pending_attempts = 6\n",
		"empty-section":   "[context.iteration]\n",
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
				t.Fatal(err)
			}
			config, err := configuration.LoadContext(path)
			if err != nil {
				t.Fatal(err)
			}
			if config.IterationHistoryLimit != configuration.DefaultIterationHistoryLimit {
				t.Fatalf("context config = %+v", config)
			}
		})
	}
}

func TestLoadContextRejectsUnknownNegativeEmptyOrDuplicateHistoryLimit(t *testing.T) {
	t.Parallel()
	for name, contents := range map[string]string{
		"unknown":   "[context.iteration]\nother = 1\n",
		"negative":  "[context.iteration]\nhistory_limit = -1\n",
		"not-int":   "[context.iteration]\nhistory_limit = twenty\n",
		"empty":     "[context.iteration]\nhistory_limit =\n",
		"duplicate": "[context.iteration]\nhistory_limit = 1\nhistory_limit = 2\n",
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := configuration.LoadContext(path); err == nil {
				t.Fatal("invalid context configuration was accepted")
			}
		})
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
kind = "unknown"
model = "model"
reasoning_effort = "high"
`,
		"cursor-ultra": `[agents.iteration]
kind = "cursor"
model = "model"
reasoning_effort = "ultra"
`,
		"cursor-resume": `[agents.iteration]
kind = "cursor"
model = "model"
reasoning_effort = "high"
args = ["--resume", "conversation"]
`,
		"cursor-disabled-sandbox": `[agents.iteration]
kind = "cursor"
model = "model"
reasoning_effort = "high"
args = ["--sandbox=disabled"]
`,
		"cursor-positional-prompt": `[agents.iteration]
kind = "cursor"
model = "model"
reasoning_effort = "high"
args = ["start now"]
`,
		"cursor-value-cannot-hide-reserved-option": `[agents.iteration]
kind = "cursor"
model = "model"
reasoning_effort = "high"
args = ["--api-key", "--resume"]
`,
		"codex-args": `[agents.iteration]
kind = "codex"
model = "model"
reasoning_effort = "high"
args = ["--force"]
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
