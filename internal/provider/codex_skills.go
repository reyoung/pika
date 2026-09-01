package provider

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

var codexSkillMounts = []struct{ name, mount string }{
	{name: "KernelWiki", mount: "pika-kda-kernelwiki"},
	{name: "ncu-report-skill", mount: "pika-kda-ncu-report"},
}

func validateFrozenSkills(snapshot *FrozenSkillSnapshot) error {
	if snapshot == nil {
		return nil
	}
	if snapshot.SnapshotID == "" || !filepath.IsAbs(snapshot.RootPath) || len(snapshot.Skills) != len(codexSkillMounts) {
		return errors.New("frozen skill snapshot is incomplete")
	}
	for index, expected := range codexSkillMounts {
		skill := snapshot.Skills[index]
		if skill.Name != expected.name || skill.CommitSHA == "" || skill.ContentSHA == "" || !filepath.IsAbs(skill.Path) {
			return fmt.Errorf("frozen skill %d is incomplete", index)
		}
		resolved, err := filepath.EvalSymlinks(skill.Path)
		if err != nil || filepath.Clean(resolved) != filepath.Clean(skill.Path) {
			return fmt.Errorf("frozen skill %s path is not a stable directory", skill.Name)
		}
		info, err := os.Stat(filepath.Join(resolved, "SKILL.md"))
		if err != nil || info.IsDir() {
			return fmt.Errorf("frozen skill %s has no SKILL.md", skill.Name)
		}
	}
	return nil
}

// mountCodexSkills exposes only Pika's two reserved discovery names inside the
// assigned Work repository.  The returned cleanup verifies ownership again;
// it cannot remove a path somebody replaced after launch.
func mountCodexSkills(repository, agentSessionID string, snapshot *FrozenSkillSnapshot) (func() error, error) {
	if snapshot == nil {
		return func() error { return nil }, nil
	}
	if !filepath.IsAbs(repository) || agentSessionID == "" {
		return nil, errors.New("Codex skill mount repository must be absolute")
	}
	if err := validateFrozenSkills(snapshot); err != nil {
		return nil, err
	}
	agentsRoot := filepath.Join(repository, ".agents")
	skillsRoot := filepath.Join(agentsRoot, "skills")
	agentsAbsent, err := inspectCodexDirectory(agentsRoot)
	if err != nil {
		return nil, fmt.Errorf("inspect Codex .agents directory: %w", err)
	}
	skillsAbsent := true
	if !agentsAbsent {
		skillsAbsent, err = inspectCodexDirectory(skillsRoot)
		if err != nil {
			return nil, fmt.Errorf("inspect Codex skills directory: %w", err)
		}
	}
	if err := ensureCodexSkillExcludes(repository); err != nil {
		return nil, err
	}
	if agentsAbsent {
		if err := os.Mkdir(agentsRoot, 0o700); err != nil {
			return nil, fmt.Errorf("create Codex .agents directory: %w", err)
		}
	}
	if skillsAbsent {
		if err := os.Mkdir(skillsRoot, 0o700); err != nil {
			if agentsAbsent {
				_ = os.Remove(agentsRoot)
			}
			return nil, fmt.Errorf("create Codex skill directory: %w", err)
		}
	}
	links := make([]mountedCodexSkill, 0, len(codexSkillMounts))
	for index, expected := range codexSkillMounts {
		path := filepath.Join(skillsRoot, expected.mount)
		target := snapshot.Skills[index].Path
		if err := rejectTrackedCodexPath(repository, filepath.ToSlash(filepath.Join(".agents", "skills", expected.mount))); err != nil {
			_ = cleanupCodexSkills(nil, "", nil, agentsRoot, skillsRoot, agentsAbsent, skillsAbsent)
			return nil, err
		}
		links = append(links, mountedCodexSkill{Path: path, Target: target})
	}
	receiptPath := filepath.Join(skillsRoot, ".pika-kda-ownership.json")
	if err := rejectTrackedCodexPath(repository, ".agents/skills/.pika-kda-ownership.json"); err != nil {
		_ = cleanupCodexSkills(nil, "", nil, agentsRoot, skillsRoot, agentsAbsent, skillsAbsent)
		return nil, err
	}
	receipt, err := json.Marshal(struct {
		SchemaVersion  int                 `json:"schema_version"`
		AgentSessionID string              `json:"agent_session_id"`
		SnapshotID     string              `json:"snapshot_id"`
		Links          []mountedCodexSkill `json:"links"`
	}{SchemaVersion: 1, AgentSessionID: agentSessionID, SnapshotID: snapshot.SnapshotID, Links: links})
	if err != nil {
		return nil, err
	}
	receiptExisted, err := ensureOwnershipReceipt(receiptPath, receipt)
	if err != nil {
		_ = cleanupCodexSkills(nil, "", nil, agentsRoot, skillsRoot, agentsAbsent, skillsAbsent)
		return nil, err
	}
	mounted := make([]mountedCodexSkill, 0, len(links))
	for _, link := range links {
		if err := ensureOwnedSkillLink(link.Path, link.Target, receiptExisted); err != nil {
			_ = cleanupCodexSkills(mounted, receiptPath, receipt, agentsRoot, skillsRoot, agentsAbsent, skillsAbsent)
			return nil, err
		}
		mounted = append(mounted, link)
	}
	return func() error {
		return cleanupCodexSkills(mounted, receiptPath, receipt, agentsRoot, skillsRoot, agentsAbsent, skillsAbsent)
	}, nil
}

func inspectCodexDirectory(path string) (bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return false, errors.New("symlink is not allowed")
	}
	if !info.IsDir() {
		return false, errors.New("path is not a directory")
	}
	return false, nil
}

type mountedCodexSkill struct {
	Path   string `json:"path"`
	Target string `json:"target"`
}

type codexSkillOwnershipReceipt struct {
	SchemaVersion  int                 `json:"schema_version"`
	AgentSessionID string              `json:"agent_session_id"`
	SnapshotID     string              `json:"snapshot_id"`
	Links          []mountedCodexSkill `json:"links"`
}

func ensureOwnedSkillLink(path, target string, allowExisting bool) error {
	if info, err := os.Lstat(path); err == nil {
		if !allowExisting || info.Mode()&os.ModeSymlink == 0 || !sameLinkTarget(path, target) {
			return fmt.Errorf("Codex skill discovery path is not Pika-owned: %s", path)
		}
		return nil // stale owned link from a crashed Session
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect Codex skill discovery path: %w", err)
	}
	temporary := path + ".pika-link-tmp"
	if err := os.Symlink(target, temporary); err != nil {
		return fmt.Errorf("create Codex skill link: %w", err)
	}
	if err := os.Rename(temporary, path); err != nil {
		_ = os.Remove(temporary)
		return fmt.Errorf("publish Codex skill link: %w", err)
	}
	return nil
}

func sameLinkTarget(path, target string) bool {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return false
	}
	want, err := filepath.EvalSymlinks(target)
	return err == nil && filepath.Clean(resolved) == filepath.Clean(want)
}

func ensureOwnershipReceipt(path string, expected []byte) (bool, error) {
	if contents, err := os.ReadFile(path); err == nil {
		if bytes.Equal(contents, expected) {
			return true, nil
		}
		var previous, next codexSkillOwnershipReceipt
		if decodeStrictJSON(contents, &previous) != nil || decodeStrictJSON(expected, &next) != nil || !sameCodexSkillOwnership(previous, next) {
			return false, fmt.Errorf("Codex skill ownership receipt is not Pika-owned: %s", path)
		}
		if err := replaceOwnershipReceipt(path, expected); err != nil {
			return false, err
		}
		return true, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, fmt.Errorf("inspect Codex skill ownership receipt: %w", err)
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return false, fmt.Errorf("create Codex skill ownership receipt: %w", err)
	}
	if _, err := file.Write(expected); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return false, fmt.Errorf("write Codex skill ownership receipt: %w", err)
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return false, err
	}
	return false, nil
}

func decodeStrictJSON(contents []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(contents))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("JSON receipt has trailing content")
	}
	return nil
}

func sameCodexSkillOwnership(previous, next codexSkillOwnershipReceipt) bool {
	if previous.SchemaVersion != 1 || next.SchemaVersion != 1 || previous.AgentSessionID == "" || next.AgentSessionID == "" || previous.SnapshotID != next.SnapshotID || len(previous.Links) != len(next.Links) {
		return false
	}
	for index := range next.Links {
		if previous.Links[index] != next.Links[index] {
			return false
		}
	}
	return true
}

func replaceOwnershipReceipt(path string, contents []byte) error {
	file, err := os.CreateTemp(filepath.Dir(path), ".pika-kda-ownership-")
	if err != nil {
		return fmt.Errorf("stage Codex skill ownership receipt: %w", err)
	}
	temporary := file.Name()
	defer os.Remove(temporary)
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return err
	}
	if _, err := file.Write(contents); err != nil {
		_ = file.Close()
		return fmt.Errorf("write Codex skill ownership receipt: %w", err)
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporary, path); err != nil {
		return fmt.Errorf("publish Codex skill ownership receipt: %w", err)
	}
	return nil
}

func cleanupCodexSkills(links []mountedCodexSkill, receiptPath string, expectedReceipt []byte, agentsRoot, skillsRoot string, removeAgents, removeSkills bool) error {
	var failures []error
	if receiptPath != "" {
		contents, err := os.ReadFile(receiptPath)
		if err != nil || !bytes.Equal(contents, expectedReceipt) {
			if err == nil {
				err = errors.New("receipt contents changed")
			}
			return fmt.Errorf("refuse to clean Codex skills without unchanged ownership receipt: %w", err)
		}
	}
	for _, link := range links {
		info, err := os.Lstat(link.Path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			failures = append(failures, err)
			continue
		}
		if info.Mode()&os.ModeSymlink == 0 || !sameLinkTarget(link.Path, link.Target) {
			failures = append(failures, fmt.Errorf("refuse to remove non-owned Codex skill path: %s", link.Path))
			continue
		}
		if err := os.Remove(link.Path); err != nil {
			failures = append(failures, err)
		}
	}
	if len(failures) == 0 && receiptPath != "" {
		if err := os.Remove(receiptPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			failures = append(failures, err)
		}
	}
	if removeSkills && len(failures) == 0 {
		_ = os.Remove(skillsRoot)
	}
	if removeAgents && len(failures) == 0 {
		_ = os.Remove(agentsRoot)
	}
	return errors.Join(failures...)
}

func rejectTrackedCodexPath(repository, path string) error {
	command := exec.Command("git", "-C", repository, "ls-files", "--error-unmatch", "--", path)
	if output, err := command.CombinedOutput(); err == nil {
		return fmt.Errorf("Codex skill discovery path is tracked: %s", strings.TrimSpace(string(output)))
	} else if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 1 {
		return fmt.Errorf("inspect tracked Codex skill path: %w", err)
	}
	return nil
}

func ensureCodexSkillExcludes(repository string) error {
	gitPath := filepath.Join(repository, ".git")
	info, err := os.Stat(gitPath)
	if err != nil {
		return fmt.Errorf("inspect repository Git metadata: %w", err)
	}
	if !info.IsDir() {
		contents, readErr := os.ReadFile(gitPath)
		if readErr != nil {
			return fmt.Errorf("read worktree Git metadata: %w", readErr)
		}
		line := strings.TrimSpace(string(contents))
		prefix := "gitdir:"
		if !strings.HasPrefix(line, prefix) {
			return errors.New("worktree Git metadata is malformed")
		}
		gitPath = strings.TrimSpace(strings.TrimPrefix(line, prefix))
		if !filepath.IsAbs(gitPath) {
			gitPath = filepath.Join(repository, gitPath)
		}
	}
	exclude := filepath.Join(gitPath, "info", "exclude")
	contents, err := os.ReadFile(exclude)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read repository exclude file: %w", err)
	}
	lines := string(contents)
	changed := false
	for _, expected := range codexSkillMounts {
		pattern := ".agents/skills/" + expected.mount
		if !containsExactLine(lines, pattern) {
			lines += pattern + "\n"
			changed = true
		}
	}
	if !containsExactLine(lines, ".agents/skills/.pika-kda-ownership.json") {
		lines += ".agents/skills/.pika-kda-ownership.json\n"
		changed = true
	}
	if !changed {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(exclude), 0o700); err != nil {
		return fmt.Errorf("create repository exclude directory: %w", err)
	}
	if err := os.WriteFile(exclude, []byte(lines), 0o600); err != nil {
		return fmt.Errorf("update repository exclude file: %w", err)
	}
	return nil
}

func containsExactLine(contents, target string) bool {
	for _, line := range strings.Split(contents, "\n") {
		if strings.TrimSpace(line) == target {
			return true
		}
	}
	return false
}
