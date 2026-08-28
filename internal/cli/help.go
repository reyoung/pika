package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
)

type commandHelp struct {
	synopsis string
	summary  string
	details  string
	examples []string
}

var publicCommandHelp = map[string]commandHelp{
	"kick-off": {
		synopsis: "kick-off [options]",
		summary:  "Create a Herdr workspace and start an optimization",
		details:  "Checks Herdr's fresh-session setting and, with confirmation, updates and reloads the Herdr configuration. Then creates a workspace for the repository, opens the Pika-Go daemon pane, waits for it to become healthy, starts `pika-go init` in the new workspace's root pane, and focuses that workspace and tab.",
		examples: []string{"pika-go kick-off", "pika-go kick-off --repository /path/to/repo --defaults"},
	},
	"status": {
		synopsis: "status [options]",
		summary:  "Show the current optimization and daemon status",
		examples: []string{"pika-go status", "pika-go status --json"},
	},
	"draft-baseline": {
		synopsis: "draft-baseline [options]",
		summary:  "Start another baseline draft",
	},
	"back-off": {
		synopsis: "back-off -m MESSAGE [options]",
		summary:  "Reject the current verification or integration work",
		examples: []string{`pika-go back-off -m "The benchmark is noisy; rerun with warmup"`},
	},
	"cancel-work": {
		synopsis: "cancel-work WORK_ID [options]",
		summary:  "Cancel one pending or running work item",
	},
	"shutdown": {
		synopsis: "shutdown [options]",
		summary:  "Gracefully drain and stop the daemon",
	},
	"backup": {
		synopsis: "backup --output ABSOLUTE_PATH [options]",
		summary:  "Create a validated SQLite backup",
	},
	"install": {
		synopsis: "install [options]",
		summary:  "Install and register Pika-Go as a Herdr plugin",
	},
	"init": {
		synopsis: "init --repository ABSOLUTE_PATH [options]",
		summary:  "Initialize a daemon that is already running",
		details:  "Most users should run `pika-go kick-off` instead. Use init when the daemon pane was opened manually.",
	},
	"edit-instruction": {
		synopsis: "edit-instruction NAME [options]",
		summary:  "Edit a role's user-owned instruction overlay",
	},
	"daemon": {
		synopsis: "daemon [options]",
		summary:  "Run the long-lived orchestration daemon",
		details:  "Normally started for you by `pika-go kick-off` or the Herdr plugin pane.",
	},
	"mcp-proxy": {
		synopsis: "mcp-proxy [options]",
		summary:  "Bridge stdio MCP traffic to the daemon",
	},
	"version": {
		synopsis: "version",
		summary:  "Print the Pika-Go version",
	},
}

func newCommandFlagSet(name string, output io.Writer) *flag.FlagSet {
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.SetOutput(output)
	flags.Usage = func() { printCommandUsage(output, name, flags) }
	return flags
}

func parseCommandFlags(flags *flag.FlagSet, args []string) (ok bool, exitCode int) {
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return false, 0
		}
		return false, 2
	}
	return true, 0
}

func printCommandUsage(output io.Writer, name string, flags *flag.FlagSet) {
	help, ok := publicCommandHelp[name]
	if !ok {
		help = commandHelp{synopsis: name + " [options]"}
	}
	_, _ = fmt.Fprintf(output, "Usage:\n  pika-go %s\n", help.synopsis)
	if help.summary != "" {
		_, _ = fmt.Fprintf(output, "\n%s.\n", help.summary)
	}
	if help.details != "" {
		_, _ = fmt.Fprintf(output, "%s\n", help.details)
	}
	if flags != nil {
		_, _ = fmt.Fprintln(output, "\nOptions:")
		printFlagDefaults(output, flags)
	}
	if len(help.examples) != 0 {
		_, _ = fmt.Fprintln(output, "\nExamples:")
		for _, example := range help.examples {
			_, _ = fmt.Fprintf(output, "  %s\n", example)
		}
	}
}

func printFlagDefaults(output io.Writer, flags *flag.FlagSet) {
	flags.VisitAll(func(option *flag.Flag) {
		prefix := "--"
		if len(option.Name) == 1 {
			prefix = "-"
		}
		valueName, usage := flag.UnquoteUsage(option)
		if valueName == "value" {
			valueName = "VALUE"
		} else {
			valueName = strings.ToUpper(valueName)
		}
		_, _ = fmt.Fprintf(output, "  %s%s", prefix, option.Name)
		if valueName != "" {
			_, _ = fmt.Fprintf(output, " %s", valueName)
		}
		_, _ = fmt.Fprintf(output, "\n      %s", usage)
		if option.DefValue != "" && option.DefValue != "false" && option.DefValue != "0" {
			_, _ = fmt.Fprintf(output, " (default: %s)", option.DefValue)
		}
		_, _ = fmt.Fprintln(output)
	})
}

func printUsage(output io.Writer) {
	_, _ = fmt.Fprintln(output, `Pika-Go orchestrates long-running coding-agent optimization in Herdr.

Usage:
  pika-go kick-off [options]
  pika-go COMMAND [options]

Get started:
  pika-go install                 # once per installation or upgrade
  cd /path/to/repository
  pika-go kick-off

Workflow commands:
  kick-off         Create a Herdr workspace and start an optimization
  status           Show the current optimization and daemon status
  draft-baseline   Start another baseline draft
  back-off         Reject the current verification or integration work
  cancel-work      Cancel one pending or running work item

Operations:
  shutdown         Gracefully drain and stop the daemon
  backup           Create a validated SQLite backup
  edit-instruction Edit a role's user-owned instruction overlay

Setup and advanced commands:
  install          Install and register the Herdr plugin
  init             Initialize a daemon that is already running
  daemon           Run the orchestration daemon
  mcp-proxy        Bridge stdio MCP traffic to the daemon
  version          Print the Pika-Go version

Examples:
  pika-go kick-off
  pika-go kick-off --repository /path/to/repo --defaults
  pika-go status --json
  pika-go backup --output /absolute/path/to/pika.db

Run pika-go COMMAND --help for command-specific options.`)
}
