package provider_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reyoung/pika-go/internal/provider"
)

func TestCodexProbeVerifiesRepositorySkillDiscoveryWithoutModelRun(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name      string
		discovery string
		wantErr   string
	}{
		{name: "discovers-reserved-links", discovery: codexPromptInputFixture(
			`- KernelWiki: probe (file: r0/pika-kda-kernelwiki/SKILL.md)\n- ncu-report-skill: probe (file: r0/pika-kda-ncu-report/SKILL.md)\n`)},
		{name: "stderr-warning-with-valid-json", discovery: "echo 'Warning: discovery diagnostic' >&2\n" + codexPromptInputFixture(
			`- KernelWiki: probe (file: r0/pika-kda-kernelwiki/SKILL.md)\n- ncu-report-skill: probe (file: r0/pika-kda-ncu-report/SKILL.md)\n`)},
		{name: "names-only", discovery: codexPromptInputFixture(
			`- KernelWiki: probe\n- ncu-report-skill: probe\n`), wantErr: "is missing"},
		{name: "paths-only", discovery: codexPromptInputFixture(
			`- other-one: probe (file: r0/pika-kda-kernelwiki/SKILL.md)\n- other-two: probe (file: r0/pika-kda-ncu-report/SKILL.md)\n`), wantErr: "is missing"},
		{name: "mismatched-paths", discovery: codexPromptInputFixture(
			`- KernelWiki: probe (file: r0/pika-kda-ncu-report/SKILL.md)\n- ncu-report-skill: probe (file: r0/pika-kda-kernelwiki/SKILL.md)\n`), wantErr: "did not resolve"},
		{name: "duplicate-name", discovery: codexPromptInputFixture(
			`- KernelWiki: probe (file: r0/pika-kda-kernelwiki/SKILL.md)\n- KernelWiki: duplicate (file: r0/pika-kda-kernelwiki/SKILL.md)\n- ncu-report-skill: probe (file: r0/pika-kda-ncu-report/SKILL.md)\n`), wantErr: "duplicate reserved skill"},
		{name: "wrong-path", discovery: codexPromptInputFixture(
			`- KernelWiki: probe (file: r0/missing/SKILL.md)\n- ncu-report-skill: probe (file: r0/pika-kda-ncu-report/SKILL.md)\n`), wantErr: "invalid absolute SKILL.md path"},
		{name: "ambiguous-blocks", discovery: codexPromptInputFixture(
			`- KernelWiki: probe (file: r0/pika-kda-kernelwiki/SKILL.md)\n- ncu-report-skill: probe (file: r0/pika-kda-ncu-report/SKILL.md)\n</skills_instructions>\n<skills_instructions>duplicate\n`), wantErr: "ambiguous"},
	} {
		t.Run(test.name, func(t *testing.T) {
			executable := filepath.Join(t.TempDir(), "codex")
			script := "#!/bin/sh\n" +
				"if [ \"$1\" = --version ]; then echo codex-test; exit 0; fi\n" +
				"if [ \"$1\" = login ] && [ \"$2\" = status ]; then echo Logged-in; exit 0; fi\n" +
				test.discovery + "\nexit 1\n"
			if err := os.WriteFile(executable, []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}
			capabilities, err := provider.NewCodexAdapter().Probe(context.Background(), provider.ProbeRequest{Executable: executable, RequireSkillInjection: true})
			if test.wantErr == "" {
				if err != nil || !capabilities.Compatible {
					t.Fatalf("capabilities=%+v err=%v", capabilities, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("discovery error = %v", err)
			}
		})
	}
}

func codexPromptInputFixture(availableSkills string) string {
	availableSkills = strings.ReplaceAll(availableSkills, `\n`, `\\n`)
	return `if [ "$1" = debug ] && [ "$2" = prompt-input ]; then
root="$(pwd)/.agents/skills"
printf '[{"type":"message","role":"developer","content":[{"type":"input_text","text":"<skills_instructions>\\n## Skills\\n### Skill roots\\n- ` + "`r0`" + ` = ` + "`%s`" + `\\n### Available skills\\n` + availableSkills + `</skills_instructions>"}]}]\n' "$root"
exit 0
fi`
}
