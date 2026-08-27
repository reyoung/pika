package codexprofile_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/reyoung/pika-go/internal/codexprofile"
)

func TestManagedProfileParsesWithInstalledCodex(t *testing.T) {
	codexExecutable, err := exec.LookPath("codex")
	if err != nil {
		t.Skip("Codex is not installed")
	}
	pikaExecutable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	codexHome := filepath.Join(t.TempDir(), "codex")
	instanceBin := filepath.Join(t.TempDir(), "bin")
	if _, err := codexprofile.Install(codexprofile.Options{
		CodexHome: codexHome, InstanceBin: instanceBin,
		PikaExecutable: pikaExecutable, CodexExecutable: codexExecutable,
	}); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(filepath.Join(instanceBin, "codex"), "mcp", "list")
	command.Env = append(os.Environ(), "CODEX_HOME="+codexHome,
		"PIKA_CODEX_DEVELOPER_INSTRUCTIONS_TOML="+strconv.Quote("immutable role prompt\ndynamic Work ID: work-1"))
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("Codex rejected managed profile: %v\n%s", err, output)
	}
}
