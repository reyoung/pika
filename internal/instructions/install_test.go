package instructions_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/reyoung/pika-go/internal/instructions"
)

func TestInstallCreatesStaticCatalogWithoutOverwritingEdits(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "instructions")
	if err := instructions.Install(root); err != nil {
		t.Fatalf("install instructions: %v", err)
	}
	baseline, err := instructions.Path(root, "baseline")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(baseline, []byte("operator edit\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := instructions.Install(root); err != nil {
		t.Fatalf("reinstall instructions: %v", err)
	}
	contents, err := os.ReadFile(baseline)
	if err != nil || string(contents) != "operator edit\n" {
		t.Fatalf("edited instruction=%q err=%v", contents, err)
	}
	for _, name := range instructions.LogicalNames() {
		path, err := instructions.Path(root, name)
		if err != nil {
			t.Fatal(err)
		}
		if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("instruction %s: info=%v err=%v", name, info, err)
		}
	}
}

func TestDefaultInstructionsAreEmpty(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "instructions")
	if err := instructions.Install(root); err != nil {
		t.Fatal(err)
	}
	for _, logicalName := range instructions.LogicalNames() {
		path, err := instructions.Path(root, logicalName)
		if err != nil {
			t.Fatal(err)
		}
		contents, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if len(contents) != 0 {
			t.Errorf("default instructions %s are not empty: %q", logicalName, contents)
		}
	}
}
