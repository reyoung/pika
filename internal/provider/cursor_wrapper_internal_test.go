package provider

import (
	"reflect"
	"testing"
)

func TestReplaceEnvironmentValueRemovesInheritedDuplicates(t *testing.T) {
	environment := []string{
		"PATH=/bin",
		"HERDR_AGENT=codex",
		"OTHER=value",
		"HERDR_AGENT=stale",
	}
	got := replaceEnvironmentValue(environment, "HERDR_AGENT", "cursor")
	want := []string{
		"PATH=/bin",
		"OTHER=value",
		"HERDR_AGENT=cursor",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("environment = %q, want %q", got, want)
	}
}
