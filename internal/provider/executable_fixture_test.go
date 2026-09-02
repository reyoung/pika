package provider_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reyoung/pika-go/internal/provider"
)

const cursorFixturePrefix = "cursor-agent-fixture-"

func TestMain(m *testing.M) {
	if filepath.Base(os.Args[0]) == "cursor-agent" && os.Getenv("PIKA_GO_TEST_CURSOR_WRAPPER_HELPER") == "1" {
		os.Exit(provider.RunCursorWrapper(os.Args[1:], os.Environ(), os.Stdin, os.Stdout, os.Stderr))
	}
	if os.Getenv("PIKA_GO_TEST_CURSOR_CHILD") == "1" {
		count := 0
		for _, entry := range os.Environ() {
			if strings.HasPrefix(entry, "HERDR_AGENT=") {
				count++
			}
		}
		fmt.Printf("HERDR_AGENT=%s count=%d\n", os.Getenv("HERDR_AGENT"), count)
		os.Exit(0)
	}
	fixture := strings.TrimPrefix(filepath.Base(os.Args[0]), cursorFixturePrefix)
	if fixture != filepath.Base(os.Args[0]) {
		os.Exit(runCursorExecutableFixture(fixture, os.Args[1:]))
	}
	os.Exit(m.Run())
}

func cursorFixtureExecutable(t *testing.T, fixture string) string {
	t.Helper()
	testExecutable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), cursorFixturePrefix+fixture)
	if err := os.Symlink(testExecutable, path); err != nil {
		t.Fatal(err)
	}
	return path
}

func runCursorExecutableFixture(fixture string, arguments []string) int {
	first := ""
	if len(arguments) != 0 {
		first = arguments[0]
	}
	switch fixture {
	case "compatible":
		switch first {
		case "--version":
			fmt.Println("2026.08.31-4057e58")
		case "status":
			fmt.Println("Logged in as test@example.com")
		case "--plugin-dir":
			if len(arguments) < 3 || arguments[2] != "--help" {
				return 1
			}
			fmt.Println("--plugin-dir")
		default:
			return 1
		}
		return 0
	case "authentication-status":
		if first == "--version" {
			fmt.Println("2026.08.31-4057e58")
			return 0
		}
		if first == "status" {
			fmt.Println("Not logged in")
			return 0
		}
	case "version":
		if first == "--version" {
			fmt.Println("other")
			return 0
		}
	case "authentication":
		if first == "--version" {
			fmt.Println("2026.08.31-4057e58")
			return 0
		}
		fmt.Println("logged-out")
	case "empty-authentication-error":
		if first == "--version" {
			fmt.Println("2026.08.31-4057e58")
			return 0
		}
	case "plugin-directory":
		if first == "--version" {
			fmt.Println("2026.08.31-4057e58")
			return 0
		}
		if first == "status" {
			fmt.Println("Logged in")
			return 0
		}
	case "model-catalog":
		if first == "--list-models" {
			fmt.Print("Available models\n\n" +
				"auto - Auto (default)\n" +
				"gpt-5.6-sol-low - GPT-5.6 Sol Low\n" +
				"gpt-5.6-sol-high - GPT-5.6 Sol High\n" +
				"gpt-5.6-sol-high-fast - GPT-5.6 Sol High Fast\n" +
				"gpt-5.5-extra-high - GPT-5.5 Extra High\n" +
				"gpt-5.6-terra-medium - GPT-5.6 Terra Medium\n")
			return 0
		}
	}
	return 1
}

func TestCursorExecutableFixtureRejectsMalformedPluginDiscovery(t *testing.T) {
	for _, arguments := range [][]string{
		{"--plugin-dir"},
		{"--plugin-dir", "/tmp/plugin"},
		{"--plugin-dir", "/tmp/plugin", "status"},
	} {
		if code := runCursorExecutableFixture("compatible", arguments); code == 0 {
			t.Fatalf("fixture accepted malformed plugin discovery arguments %q", arguments)
		}
	}
}
