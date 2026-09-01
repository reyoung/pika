package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/reyoung/pika-go/internal/skillsnapshot"
)

// TestMain supplies a deterministic Git transport for the fixed public skill
// remotes. The source identities remain canonical; only this package-private
// test factory maps network fetches to committed local fixtures.
func TestMain(mainTest *testing.M) {
	root, err := os.MkdirTemp("", "pika-cli-kda-sources-")
	if err != nil {
		panic(err)
	}
	kernel := createKDASkillSource(root, "KernelWiki", "master")
	ncu := createKDASkillSource(root, "ncu-report-skill", "main")
	newSkillSnapshotManager = func() skillsnapshot.Manager {
		return skillsnapshot.Manager{Git: localSkillGit{remotes: map[string]string{
			"https://github.com/mit-han-lab/KernelWiki.git":       kernel,
			"https://github.com/mit-han-lab/ncu-report-skill.git": ncu,
		}}}
	}
	code := mainTest.Run()
	_ = os.RemoveAll(root)
	os.Exit(code)
}

type localSkillGit struct{ remotes map[string]string }

func (g localSkillGit) Run(ctx context.Context, directory string, arguments ...string) ([]byte, error) {
	mapped := append([]string{}, arguments...)
	if !(len(arguments) >= 3 && arguments[0] == "remote" && arguments[1] == "set-url") {
		for index, value := range mapped {
			if replacement, found := g.remotes[value]; found {
				mapped[index] = replacement
			}
		}
	}
	command := exec.CommandContext(ctx, "git", mapped...)
	command.Dir = directory
	output, err := command.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("git %v: %w: %s", mapped, err, output)
	}
	if len(arguments) == 3 && arguments[0] == "remote" && arguments[1] == "get-url" && arguments[2] == "origin" {
		for canonical, local := range g.remotes {
			if string(output) == local+"\n" {
				return []byte(canonical + "\n"), nil
			}
		}
	}
	return output, nil
}

func createKDASkillSource(root, name, branch string) string {
	repository := filepath.Join(root, name)
	for _, arguments := range [][]string{{"init", "--quiet", "--initial-branch=" + branch, repository}, {"-C", repository, "config", "user.name", "Pika Test"}, {"-C", repository, "config", "user.email", "pika@example.invalid"}} {
		if output, err := exec.Command("git", arguments...).CombinedOutput(); err != nil {
			panic(fmt.Sprintf("git %v: %v: %s", arguments, err, output))
		}
	}
	contents := []byte("---\nname: " + name + "\ndescription: deterministic CLI test skill\n---\n")
	if err := os.WriteFile(filepath.Join(repository, "SKILL.md"), contents, 0o600); err != nil {
		panic(err)
	}
	if output, err := exec.Command("git", "-C", repository, "add", "SKILL.md").CombinedOutput(); err != nil {
		panic(fmt.Sprintf("git add: %v: %s", err, output))
	}
	if output, err := exec.Command("git", "-C", repository, "commit", "--quiet", "-m", "fixture").CombinedOutput(); err != nil {
		panic(fmt.Sprintf("git commit: %v: %s", err, output))
	}
	return repository
}
