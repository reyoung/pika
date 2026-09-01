package fakeagent

import (
	"strings"
	"testing"
)

func TestMCPResponseDecodersFailClosedWithoutNilWrappedErrors(t *testing.T) {
	t.Parallel()
	for name, test := range map[string]func(string) error{
		"experiment": func(value string) error { _, err := recordedExperimentID(value); return err },
		"intent":     func(value string) error { _, err := preparedIntentID(value); return err },
		"best":       func(value string) error { _, err := appliedBestSHA(value); return err },
		"commit":     func(value string) error { _, err := decodeScopedCommit(value); return err },
	} {
		t.Run(name, func(t *testing.T) {
			for _, response := range []string{`{}`, `{"result":{"content":[]}}`, `{"result":{"content":[{"text":"{}"}]}}`} {
				if err := test(response); err == nil || strings.Contains(err.Error(), "%!w") {
					t.Fatalf("response %s error = %v", response, err)
				}
			}
		})
	}
}
