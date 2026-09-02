package codexprofile_test

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/reyoung/pika-go/internal/provider"
)

func TestMain(m *testing.M) {
	switch filepath.Base(os.Args[0]) {
	case "cursor-agent":
		os.Exit(provider.RunCursorWrapper(os.Args[1:], os.Environ(), os.Stdin, os.Stdout, os.Stderr))
	case "provider-argv-recorder":
		for _, argument := range os.Args[1:] {
			fmt.Println(argument)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func providerFixtureExecutable(t *testing.T, name string) string {
	t.Helper()
	testExecutable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), name)
	if err := os.Symlink(testExecutable, path); err != nil {
		t.Fatal(err)
	}
	return path
}
