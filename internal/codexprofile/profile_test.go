package codexprofile_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/reyoung/pika-go/internal/codexprofile"
)

func TestInstallPreservesOnlyMatchingCodexHookTrustState(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	codexHome := filepath.Join(root, "codex")
	instanceBin := filepath.Join(root, "bin")
	if err := os.MkdirAll(codexHome, 0o700); err != nil {
		t.Fatal(err)
	}
	profilePath := filepath.Join(codexHome, codexprofile.ProfileName+".config.toml")
	trustedHash := "sha256:" + strings.Repeat("a", 64)
	foreignHash := "sha256:" + strings.Repeat("b", 64)
	prior := codexprofile.OwnershipMarker + "\n\n[hooks.state]\n\n" +
		"[hooks.state." + strconv.Quote(profilePath+":stop:0:0") + "]\ntrusted_hash = " + strconv.Quote(trustedHash) + "\n\n" +
		"[hooks.state." + strconv.Quote("foreign:stop:0:0") + "]\ntrusted_hash = " + strconv.Quote(foreignHash) + "\n"
	if err := os.WriteFile(profilePath, []byte(prior), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := codexprofile.Install(codexprofile.Options{
		CodexHome:       codexHome,
		InstanceBin:     instanceBin,
		PikaExecutable:  "/bin/echo",
		CodexExecutable: "/bin/sh",
	}); err != nil {
		t.Fatal(err)
	}
	profile, err := os.ReadFile(profilePath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(profile), profilePath+":stop:0:0") || !strings.Contains(string(profile), trustedHash) {
		t.Fatalf("matching Codex hook trust state was lost:\n%s", profile)
	}
	if strings.Contains(string(profile), "foreign:stop:0:0") || strings.Contains(string(profile), foreignHash) {
		t.Fatalf("foreign hook trust state was preserved:\n%s", profile)
	}
}

func TestInstallCreatesOwnedOverlayAndInstanceWrapper(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	codexHome := filepath.Join(root, "codex home")
	instanceBin := filepath.Join(root, "instance", "runtime", "bin")
	pikaExecutable := filepath.Join(root, "pika go")
	codexExecutable := filepath.Join(root, "real codex")
	if err := os.WriteFile(pikaExecutable, []byte("executable\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(codexExecutable, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(codexHome, 0o700); err != nil {
		t.Fatal(err)
	}
	baseConfig := []byte("# user-owned base\nmodel = \"user-model\"\n")
	baseHooks := []byte(`{"hooks":{"Stop":[]}}`)
	if err := os.WriteFile(filepath.Join(codexHome, "config.toml"), baseConfig, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(codexHome, "hooks.json"), baseHooks, 0o600); err != nil {
		t.Fatal(err)
	}

	rollback, err := codexprofile.Install(codexprofile.Options{
		CodexHome:       codexHome,
		InstanceBin:     instanceBin,
		PikaExecutable:  pikaExecutable,
		CodexExecutable: codexExecutable,
	})
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	profile, err := os.ReadFile(filepath.Join(codexHome, codexprofile.ProfileName+".config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Managed by pika-go",
		"[mcp_servers.pika_go]",
		`args = ["mcp-proxy"]`,
		`env_vars = ["PIKA_GO_SOCKET", "PIKA_MCP_GRANT"]`,
		`default_tools_approval_mode = "approve"`,
		"[[hooks.SessionStart]]",
		"[[hooks.SessionEnd]]",
		"[[hooks.UserPromptSubmit]]",
		"[[hooks.PostToolUse]]",
		"[[hooks.Stop]]",
		"hook codex",
	} {
		if !strings.Contains(string(profile), want) {
			t.Fatalf("profile does not contain %q:\n%s", want, profile)
		}
	}
	wrapper, err := os.ReadFile(filepath.Join(instanceBin, "codex"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(wrapper), "--profile "+codexprofile.ProfileName) || !strings.Contains(string(wrapper), codexExecutable) {
		t.Fatalf("wrapper = %q", wrapper)
	}
	if info, err := os.Stat(filepath.Join(instanceBin, "codex")); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("wrapper mode = %v, err = %v", info, err)
	}
	command := exec.Command(filepath.Join(instanceBin, "codex"))
	command.Env = append(os.Environ(), "PIKA_AGENT_MODEL=gpt-test", "PIKA_AGENT_REASONING_EFFORT=xhigh", `PIKA_CODEX_DEVELOPER_INSTRUCTIONS_TOML="role prompt\\ncontext"`, "PIKA_CODEX_INITIAL_PROMPT=initial prompt", "PIKA_CODEX_BYPASS_HOOK_TRUST=1")
	output, err := command.Output()
	if err != nil {
		t.Fatalf("run wrapper: %v", err)
	}
	for _, want := range []string{"--profile\n" + codexprofile.ProfileName, "--yolo", "--dangerously-bypass-hook-trust", "--model\ngpt-test", `model_reasoning_effort="xhigh"`, `developer_instructions="role prompt\\ncontext"`, "initial prompt"} {
		if !strings.Contains(string(output), want) {
			t.Fatalf("wrapper args missing %q: %q", want, output)
		}
	}
	resume := exec.Command(filepath.Join(instanceBin, "codex"), "resume", "session-id")
	if err := resume.Run(); err == nil {
		t.Fatal("Codex wrapper accepted native resume")
	} else if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 64 {
		t.Fatalf("native resume exit = %v", err)
	}
	if err := rollback(); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	gotBase, _ := os.ReadFile(filepath.Join(codexHome, "config.toml"))
	gotHooks, _ := os.ReadFile(filepath.Join(codexHome, "hooks.json"))
	if string(gotBase) != string(baseConfig) || string(gotHooks) != string(baseHooks) {
		t.Fatalf("base Codex layers changed: config=%q hooks=%q", gotBase, gotHooks)
	}
	for _, path := range []string{filepath.Join(codexHome, codexprofile.ProfileName+".config.toml"), filepath.Join(instanceBin, "codex")} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("%s remains after rollback: %v", path, err)
		}
	}
}

func TestInstallRefusesProfileOwnedByAnotherProduct(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	codexHome := filepath.Join(root, "codex")
	if err := os.MkdirAll(codexHome, 0o700); err != nil {
		t.Fatal(err)
	}
	profilePath := filepath.Join(codexHome, codexprofile.ProfileName+".config.toml")
	if err := os.WriteFile(profilePath, []byte("model = \"user-owned\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := codexprofile.Install(codexprofile.Options{
		CodexHome:       codexHome,
		InstanceBin:     filepath.Join(root, "bin"),
		PikaExecutable:  "/bin/echo",
		CodexExecutable: "/bin/sh",
	})
	if err == nil || !strings.Contains(err.Error(), "not owned by pika-go") {
		t.Fatalf("error = %v", err)
	}
	contents, _ := os.ReadFile(profilePath)
	if string(contents) != "model = \"user-owned\"\n" {
		t.Fatalf("foreign profile was changed: %q", contents)
	}
}

func TestInstallRollbackRestoresPreviousOwnedProfileAndWrapper(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	codexHome := filepath.Join(root, "codex")
	instanceBin := filepath.Join(root, "bin")
	if err := os.MkdirAll(codexHome, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(instanceBin, 0o700); err != nil {
		t.Fatal(err)
	}
	profilePath := filepath.Join(codexHome, codexprofile.ProfileName+".config.toml")
	wrapperPath := filepath.Join(instanceBin, "codex")
	oldProfile := []byte(codexprofile.OwnershipMarker + "\nold profile\n")
	oldWrapper := []byte("#!/bin/sh\n# old wrapper\n")
	if err := os.WriteFile(profilePath, oldProfile, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(wrapperPath, oldWrapper, 0o700); err != nil {
		t.Fatal(err)
	}
	rollback, err := codexprofile.Install(codexprofile.Options{
		CodexHome:       codexHome,
		InstanceBin:     instanceBin,
		PikaExecutable:  "/bin/echo",
		CodexExecutable: "/bin/sh",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := rollback(); err != nil {
		t.Fatal(err)
	}
	gotProfile, _ := os.ReadFile(profilePath)
	gotWrapper, _ := os.ReadFile(wrapperPath)
	if string(gotProfile) != string(oldProfile) || string(gotWrapper) != string(oldWrapper) {
		t.Fatalf("rollback did not restore artifacts: profile=%q wrapper=%q", gotProfile, gotWrapper)
	}
}
