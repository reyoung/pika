package plugininstall_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reyoung/pika-go/internal/plugininstall"
)

func TestInstallCopiesBinaryManifestAndRegistersDirectory(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source-pika-go")
	if err := os.WriteFile(source, []byte("pika executable\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	argumentsPath := filepath.Join(root, "herdr-arguments")
	herdr := filepath.Join(root, "herdr")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$PIKA_GO_TEST_ARGUMENTS\"\nprintf 'linked\\n'\n"
	if err := os.WriteFile(herdr, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PIKA_GO_TEST_ARGUMENTS", argumentsPath)
	installDir := filepath.Join(root, "installed", "plugin")
	var stdout, stderr bytes.Buffer

	result, err := plugininstall.Install(context.Background(), plugininstall.Options{
		Executable: source, HerdrExecutable: herdr, InstallDir: installDir, Version: "1.2.3-test",
		Stdout: &stdout, Stderr: &stderr,
	})
	if err != nil {
		t.Fatalf("install: %v; stderr=%q", err, stderr.String())
	}
	if result.InstallDir != installDir || result.Executable != filepath.Join(installDir, "pika-go") || result.ManifestPath != filepath.Join(installDir, "herdr-plugin.toml") {
		t.Fatalf("result = %+v", result)
	}
	installed, err := os.ReadFile(result.Executable)
	if err != nil {
		t.Fatal(err)
	}
	if string(installed) != "pika executable\n" {
		t.Fatalf("installed executable = %q", installed)
	}
	info, err := os.Stat(result.Executable)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("executable mode = %o, want 755", info.Mode().Perm())
	}
	manifest, err := os.ReadFile(result.ManifestPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{`id = "pika-go"`, `version = "1.2.3-test"`, `command = ["./pika-go", "daemon"]`} {
		if !strings.Contains(string(manifest), expected) {
			t.Errorf("manifest does not contain %q:\n%s", expected, manifest)
		}
	}
	arguments, err := os.ReadFile(argumentsPath)
	if err != nil {
		t.Fatal(err)
	}
	wantArguments := "plugin\nlink\n" + installDir + "\n--enabled\n"
	if string(arguments) != wantArguments {
		t.Fatalf("Herdr arguments = %q, want %q", arguments, wantArguments)
	}
	if stdout.String() != "linked\n" || stderr.Len() != 0 {
		t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func TestInstallReplacesExistingFiles(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source-pika-go")
	if err := os.WriteFile(source, []byte("new executable\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	installDir := filepath.Join(root, "plugin")
	if err := os.MkdirAll(installDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(installDir, "pika-go"), []byte("old executable\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(installDir, "herdr-plugin.toml"), []byte("old manifest\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	herdr := filepath.Join(root, "herdr")
	if err := os.WriteFile(herdr, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}

	if _, err := plugininstall.Install(context.Background(), plugininstall.Options{
		Executable: source, HerdrExecutable: herdr, InstallDir: installDir, Version: "2.0.0",
	}); err != nil {
		t.Fatal(err)
	}
	installed, err := os.ReadFile(filepath.Join(installDir, "pika-go"))
	if err != nil {
		t.Fatal(err)
	}
	if string(installed) != "new executable\n" {
		t.Fatalf("installed executable = %q", installed)
	}
	manifest, err := os.ReadFile(filepath.Join(installDir, "herdr-plugin.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(manifest), `version = "2.0.0"`) {
		t.Fatalf("manifest = %s", manifest)
	}
}

func TestDefaultDirUsesXDGDataHome(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_DATA_HOME", root)

	got, err := plugininstall.DefaultDir()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(root, "pika-go", "plugin"); got != want {
		t.Fatalf("DefaultDir() = %q, want %q", got, want)
	}
}

func TestManifestRejectsUnsafeVersion(t *testing.T) {
	if _, err := plugininstall.Manifest("1.0.0\ncommand = [\"bad\"]"); err == nil {
		t.Fatal("Manifest accepted an unsafe version")
	}
}

func TestEmbeddedManifestMatchesSourcePluginRuntimeContract(t *testing.T) {
	source, err := os.ReadFile(filepath.Join("..", "..", "herdr-plugin.toml"))
	if err != nil {
		t.Fatal(err)
	}
	embedded, err := plugininstall.Manifest("embedded-test")
	if err != nil {
		t.Fatal(err)
	}

	var runtimeLines []string
	skippingBuild := false
	for _, line := range strings.Split(string(source), "\n") {
		if line == "[[build]]" {
			skippingBuild = true
			continue
		}
		if skippingBuild {
			if !strings.HasPrefix(line, "[[") {
				continue
			}
			skippingBuild = false
		}
		if strings.HasPrefix(line, "version = ") {
			line = `version = "embedded-test"`
		}
		runtimeLines = append(runtimeLines, line)
	}
	want := strings.Join(runtimeLines, "\n")
	if string(embedded) != want {
		t.Fatalf("embedded manifest drifted from herdr-plugin.toml\nwant:\n%s\ngot:\n%s", want, embedded)
	}
}
