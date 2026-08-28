//go:build integration

package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"syscall"
	"testing"
	"time"

	"github.com/reyoung/pika-go/internal/control"
	"github.com/reyoung/pika-go/internal/daemonupdate"
	"github.com/reyoung/pika-go/internal/optimizationworkspace"
	"github.com/reyoung/pika-go/internal/symphony"
	"github.com/reyoung/pika-go/internal/toolapp"
)

func TestHotUpdateHandsOffLiveSocketToNewProcess(t *testing.T) {
	isolateHerdrEnvironment(t)
	repository := newCommittedRepository(t)
	workspace, err := optimizationworkspace.Create(context.Background(), filepath.Join(t.TempDir(), "workspace"), repository)
	if err != nil {
		t.Fatal(err)
	}
	seed, err := symphony.Open(context.Background(), workspace.DatabasePath, symphony.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := seed.Apply(context.Background(), symphony.Init{Meta: symphony.CommandMeta{RequestID: "init"}, OptimizationID: workspace.Identity.ID, Repository: repository}); err != nil {
		t.Fatal(err)
	}
	view, _ := seed.Inspect(context.Background(), symphony.Status{})
	session := symphony.AgentSession{ID: "live-session", WorkID: view.Works[0].ID, Generation: view.Works[0].Generation, Role: view.Works[0].Role, AgentKind: "codex", AgentName: "live-agent", Status: symphony.AgentSessionRunning}
	if err := seed.EnsureAgentSession(context.Background(), session); err != nil {
		t.Fatal(err)
	}
	if err := seed.BindPane(context.Background(), session.ID, symphony.PaneBinding{WorkspaceID: "w1", TabID: "t1", PaneID: "p2", TerminalID: "terminal-live"}); err != nil {
		t.Fatal(err)
	}
	grant, err := seed.MintAgentGrant(context.Background(), session.ID, toolapp.CatalogForRole(session.Role), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	wantSessions, _ := seed.ActiveAgentSessions(context.Background())
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	binDir := t.TempDir()
	oldBinary := filepath.Join(binDir, "pika-go-v1")
	newBinary := filepath.Join(binDir, "pika-go-v2")
	for _, build := range []struct{ version, path string }{{"integration-v1", oldBinary}, {"integration-v2", newBinary}} {
		command := exec.Command("go", "build", "-trimpath", "-ldflags", fmt.Sprintf("-X main.version=%s", build.version), "-o", build.path, "./cmd/pika-go")
		command.Dir = root
		if output, buildErr := command.CombinedOutput(); buildErr != nil {
			t.Fatalf("build %s: %v: %s", build.version, buildErr, output)
		}
	}
	socketDir, err := os.MkdirTemp("/tmp", "pika-go-hot-update-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	socketPath := filepath.Join(socketDir, "pika.sock")
	logFile, err := os.Create(filepath.Join(t.TempDir(), "daemon.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer logFile.Close()
	command := exec.Command(oldBinary, "daemon", "--socket", socketPath, "--workspace", workspace.Root)
	command.Stdout, command.Stderr = logFile, logFile
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	var finalPID int
	t.Cleanup(func() {
		if finalPID != 0 {
			if process, findErr := os.FindProcess(finalPID); findErr == nil {
				_ = process.Signal(syscall.SIGTERM)
			}
		}
		if command.ProcessState == nil {
			_ = command.Process.Kill()
			_, _ = command.Process.Wait()
		}
	})
	waitForHealth(t, socketPath, &bytes.Buffer{})
	before, err := control.Health(context.Background(), socketPath)
	if err != nil {
		t.Fatal(err)
	}
	socketInfo, err := os.Stat(socketPath)
	if err != nil {
		t.Fatal(err)
	}
	beforeInode := socketInfo.Sys().(*syscall.Stat_t).Ino
	candidate, err := daemonupdate.Stage(context.Background(), workspace.Root, newBinary)
	if err != nil {
		t.Fatal(err)
	}
	if err := workspace.WriteHerdrBinding(optimizationworkspace.HerdrBinding{
		SocketPath: socketPath, WorkspaceID: "w1", TabID: "t1", ControlPane: "p1", DaemonPane: "p2",
		DaemonSocket: socketPath, UpdatedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}); err != nil {
		t.Fatal(err)
	}
	updateCommand := exec.Command(oldBinary, "update", "--workspace", workspace.Root, "--binary", newBinary, "--json")
	updateOutput, err := updateCommand.CombinedOutput()
	if err != nil {
		contents, _ := os.ReadFile(logFile.Name())
		t.Fatalf("public update command: %v: %s\n%s", err, updateOutput, contents)
	}
	var updateStatus daemonupdate.Status
	if err := json.Unmarshal(updateOutput, &updateStatus); err != nil || updateStatus.State != daemonupdate.StateCommitted {
		t.Fatalf("public update output = %q, status=%+v, err=%v", updateOutput, updateStatus, err)
	}
	after, err := control.Health(context.Background(), socketPath)
	if err != nil {
		t.Fatal(err)
	}
	finalPID = after.PID
	if before.Version != "integration-v1" || after.Version != "integration-v2" || before.PID == after.PID {
		t.Fatalf("health handoff = before %+v, after %+v", before, after)
	}
	if after.BinaryDigest != candidate.Digest {
		t.Fatalf("new digest = %s, want %s", after.BinaryDigest, candidate.Digest)
	}
	inspect, err := symphony.Open(context.Background(), workspace.DatabasePath, symphony.Options{})
	if err != nil {
		t.Fatal(err)
	}
	gotSessions, err := inspect.ActiveAgentSessions(context.Background())
	if err != nil || !reflect.DeepEqual(gotSessions, wantSessions) {
		_ = inspect.Close()
		t.Fatalf("active sessions changed across handoff: got=%+v want=%+v err=%v", gotSessions, wantSessions, err)
	}
	resolvedGrant, err := inspect.ResolveAgentGrant(context.Background(), grant.Token)
	_ = inspect.Close()
	if err != nil || resolvedGrant.ID != grant.ID || resolvedGrant.Revoked {
		t.Fatalf("active grant changed across handoff: %+v, %v", resolvedGrant, err)
	}
	socketInfo, err = os.Stat(socketPath)
	if err != nil {
		t.Fatal(err)
	}
	if afterInode := socketInfo.Sys().(*syscall.Stat_t).Ino; afterInode != beforeInode {
		t.Fatalf("socket inode changed across handoff: %d != %d", afterInode, beforeInode)
	}
	if err := command.Wait(); err != nil {
		t.Fatalf("old generation exit: %v", err)
	}
}

func TestHotUpdateRollsBackAcrossActivationCheckpoints(t *testing.T) {
	isolateHerdrEnvironment(t)
	repository := newCommittedRepository(t)
	workspace, err := optimizationworkspace.Create(context.Background(), filepath.Join(t.TempDir(), "workspace"), repository)
	if err != nil {
		t.Fatal(err)
	}
	root, _ := filepath.Abs(filepath.Join("..", ".."))
	binDir := t.TempDir()
	oldBinary := filepath.Join(binDir, "pika-go-stable")
	failingBeforeReady := filepath.Join(binDir, "pika-go-failing-before-ready")
	failingAfterCommit := filepath.Join(binDir, "pika-go-failing-after-commit")
	builds := []struct {
		path, flags string
	}{
		{oldBinary, "-X main.version=rollback-v1"},
		{failingBeforeReady, "-X main.version=rollback-before -X main.updateFailBeforeReadyVersion=rollback-before"},
		{failingAfterCommit, "-X main.version=rollback-after -X main.updateFailAfterCommitVersion=rollback-after"},
	}
	for _, build := range builds {
		command := exec.Command("go", "build", "-trimpath", "-ldflags", build.flags, "-o", build.path, "./cmd/pika-go")
		command.Dir = root
		if output, buildErr := command.CombinedOutput(); buildErr != nil {
			t.Fatalf("build: %v: %s", buildErr, output)
		}
	}
	socketDir, _ := os.MkdirTemp("/tmp", "pika-go-hot-rollback-")
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	socketPath := filepath.Join(socketDir, "pika.sock")
	logFile, _ := os.Create(filepath.Join(t.TempDir(), "daemon.log"))
	defer logFile.Close()
	command := exec.Command(oldBinary, "daemon", "--socket", socketPath, "--workspace", workspace.Root)
	command.Stdout, command.Stderr = logFile, logFile
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	var finalPID int
	t.Cleanup(func() {
		if finalPID != 0 {
			if process, findErr := os.FindProcess(finalPID); findErr == nil {
				_ = process.Signal(syscall.SIGTERM)
			}
		}
		if command.ProcessState == nil {
			_ = command.Process.Kill()
			_, _ = command.Process.Wait()
		}
	})
	waitForHealth(t, socketPath, &bytes.Buffer{})
	for _, failingBinary := range []string{failingBeforeReady, failingAfterCommit} {
		before, _ := control.Health(context.Background(), socketPath)
		candidate, stageErr := daemonupdate.Stage(context.Background(), workspace.Root, failingBinary)
		if stageErr != nil {
			t.Fatal(stageErr)
		}
		accepted, updateErr := control.Update(context.Background(), socketPath, candidate)
		if updateErr != nil {
			t.Fatal(updateErr)
		}
		deadline := time.Now().Add(30 * time.Second)
		for {
			status, readErr := daemonupdate.ReadStatus(workspace.Root)
			if readErr == nil && status.ID == accepted.ID && status.State == daemonupdate.StateRolledBack {
				break
			}
			if readErr == nil && status.State == daemonupdate.StateFailed {
				contents, _ := os.ReadFile(logFile.Name())
				t.Fatalf("rollback failed: %s\n%s", status.Failure, contents)
			}
			if time.Now().After(deadline) {
				t.Fatal("rollback did not finish")
			}
			time.Sleep(50 * time.Millisecond)
		}
		after, healthErr := control.Health(context.Background(), socketPath)
		if healthErr != nil {
			t.Fatal(healthErr)
		}
		finalPID = after.PID
		if after.Version != "rollback-v1" || after.PID == before.PID {
			t.Fatalf("rollback health = before %+v, after %+v", before, after)
		}
		current, currentErr := daemonupdate.ReadCurrent(workspace.Root)
		if currentErr != nil || current.Version != "rollback-v1" {
			t.Fatalf("current generation after rollback = %+v, %v", current, currentErr)
		}
	}
	if err := command.Wait(); err != nil {
		t.Fatal(err)
	}
}
