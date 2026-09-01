package symphony

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

const (
	KernelWikiRepository           = "https://github.com/mit-han-lab/KernelWiki.git"
	KernelWikiBranch               = "master"
	NCUReportSkillRepository       = "https://github.com/mit-han-lab/ncu-report-skill.git"
	NCUReportSkillBranch           = "main"
	SkillSnapshotSchemaV1    int64 = 1
)

var expectedSkillSources = []struct{ name, repository, branch, path string }{
	{"KernelWiki", KernelWikiRepository, KernelWikiBranch, "skills/KernelWiki"},
	{"ncu-report-skill", NCUReportSkillRepository, NCUReportSkillBranch, "skills/ncu-report-skill"},
}

// expectedSkillSourcesForCurrentProcess keeps the persisted contract tied to
// the two product skills. Test-only local Git remotes are a fetch seam only;
// they never alter the frozen manifest or persisted provenance.
func expectedSkillSourcesForCurrentProcess() []struct{ name, repository, branch, path string } {
	return expectedSkillSources
}

func normalizedFlowVersion(version FlowVersion) (FlowVersion, error) {
	if version == 0 {
		return FlowVersion1, nil
	}
	if version != FlowVersion1 && version != FlowVersion2 {
		return 0, fmt.Errorf("unsupported flow version %d", version)
	}
	return version, nil
}

func validateSkillSnapshotInput(snapshot *SkillSnapshotInput) error {
	expectedSources := expectedSkillSourcesForCurrentProcess()
	if snapshot == nil {
		return errors.New("flow v2 requires a frozen skill snapshot")
	}
	if snapshot.SchemaVersion != SkillSnapshotSchemaV1 || !isSHA256(snapshot.SnapshotID) || !filepath.IsAbs(snapshot.RootPath) ||
		!json.Valid(snapshot.Manifest) || !isSHA256(snapshot.ManifestSHA256) || len(snapshot.Entries) != len(expectedSources) {
		return errors.New("skill snapshot identity is incomplete")
	}
	digest := sha256.Sum256(snapshot.Manifest)
	if hex.EncodeToString(digest[:]) != snapshot.ManifestSHA256 {
		return errors.New("skill snapshot manifest digest does not match its content")
	}
	for index, expected := range expectedSources {
		entry := snapshot.Entries[index]
		if entry.Name != expected.name || entry.Repository != expected.repository || entry.Branch != expected.branch || entry.RelativePath != expected.path ||
			!isGitSHA(entry.CommitSHA) || !isSHA256(entry.ContentSHA256) {
			return fmt.Errorf("invalid skill snapshot entry %d", index)
		}
	}
	return nil
}

func isGitSHA(value string) bool {
	if len(value) != 40 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func isSHA256(value string) bool {
	if len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func safeSnapshotRelativePath(value string) bool {
	if value == "" || filepath.IsAbs(value) || filepath.Clean(value) != value || value == "." {
		return false
	}
	return !strings.HasPrefix(value, ".."+string(filepath.Separator)) && value != ".."
}
