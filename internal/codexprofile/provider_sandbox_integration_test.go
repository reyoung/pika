package codexprofile_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reyoung/pika-go/internal/codexprofile"
	"github.com/reyoung/pika-go/internal/provider"
)

func TestCodexAndCursorDefaultToYoloWhileCursorSandboxRemainsConfigurable(t *testing.T) {
	root := t.TempDir()
	recorder := providerFixtureExecutable(t, "provider-argv-recorder")
	pikaExecutable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}

	codexBin := filepath.Join(root, "codex-bin")
	rollback, err := codexprofile.Install(codexprofile.Options{
		CodexHome: filepath.Join(root, "codex-home"), InstanceBin: codexBin,
		PikaExecutable: pikaExecutable, CodexExecutable: recorder,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rollback() })
	codexOutput := runProviderWrapper(t, filepath.Join(codexBin, "codex"), nil)
	if !hasArg(codexOutput, "--yolo") || hasArg(codexOutput, "--ask-for-approval") {
		t.Fatalf("Codex sandbox argv:\n%s", codexOutput)
	}

	cursorRoot := filepath.Join(root, "cursor-runtime")
	cursor := provider.NewCursorAdapter(provider.CursorOptions{
		Executable: recorder, RuntimeRoot: cursorRoot,
		InstanceBin: filepath.Join(cursorRoot, "bin"), PikaExecutable: pikaExecutable,
	})
	defaultLaunch := prepareCursorSandboxSession(t, cursor, root, "cursor-default", nil)
	defaultOutput := runProviderWrapper(t, filepath.Join(cursorRoot, "bin", "cursor-agent"), defaultLaunch.Environment)
	if !hasArg(defaultOutput, "--yolo") || hasArg(defaultOutput, "--sandbox") {
		t.Fatalf("default Cursor sandbox argv:\n%s", defaultOutput)
	}

	restrictedLaunch := prepareCursorSandboxSession(t, cursor, root, "cursor-restricted", []string{"--sandbox", "enabled"})
	restrictedOutput := runProviderWrapper(t, filepath.Join(cursorRoot, "bin", "cursor-agent"), restrictedLaunch.Environment)
	if hasArg(restrictedOutput, "--yolo") || !hasArgSequence(restrictedOutput, "--sandbox", "enabled") {
		t.Fatalf("configured Cursor sandbox argv:\n%s", restrictedOutput)
	}
}

func prepareCursorSandboxSession(t *testing.T, adapter provider.CursorAdapter, root, sessionID string, args []string) provider.Launch {
	t.Helper()
	launch, err := adapter.PrepareSession(context.Background(), provider.SessionActivation{
		AgentSessionID: sessionID, Repository: filepath.Join(root, "repo"),
		Configuration: provider.AgentConfiguration{Kind: "cursor", Model: "auto", Args: args},
		SystemPrompt:  []byte("baseline prompt"), InitialPrompt: "run baseline",
	})
	if err != nil {
		t.Fatal(err)
	}
	return launch
}

func runProviderWrapper(t *testing.T, wrapper string, environment map[string]string) string {
	t.Helper()
	command := exec.Command(wrapper)
	command.Env = os.Environ()
	for key, value := range environment {
		command.Env = append(command.Env, key+"="+value)
	}
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("run %s: %v: %s", wrapper, err, output)
	}
	return strings.TrimSpace(string(output))
}

func hasArg(output, want string) bool {
	for _, argument := range strings.Split(output, "\n") {
		if argument == want {
			return true
		}
	}
	return false
}

func hasArgSequence(output string, wants ...string) bool {
	arguments := strings.Split(output, "\n")
	for index := 0; index+len(wants) <= len(arguments); index++ {
		if strings.Join(arguments[index:index+len(wants)], "\x00") == strings.Join(wants, "\x00") {
			return true
		}
	}
	return false
}
