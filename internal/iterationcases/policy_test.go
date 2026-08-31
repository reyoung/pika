package iterationcases_test

import (
	"slices"
	"testing"

	"github.com/reyoung/pika-go/internal/iterationcases"
)

func TestSeedIsStableAndBounded(t *testing.T) {
	full := []string{"case-0", "case-1", "case-2", "case-3", "case-4", "case-5", "case-6", "case-7", "case-8", "case-9", "case-10", "case-11"}
	want, err := iterationcases.Seed(full)
	if err != nil {
		t.Fatal(err)
	}
	got, err := iterationcases.Seed([]string{"case-11", "case-10", "case-9", "case-8", "case-7", "case-6", "case-5", "case-4", "case-3", "case-2", "case-1", "case-0"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != iterationcases.InitialLimit || !slices.Equal(got, want) {
		t.Fatalf("Seed(reordered) = %v, want stable %v", got, want)
	}
	if slices.Equal(got, full[:iterationcases.InitialLimit]) {
		t.Fatalf("Seed = input prefix %v, want hash-ranked selection", got)
	}
}

func TestSeedUsesAllSmallFullCaseSet(t *testing.T) {
	got, err := iterationcases.Seed([]string{"b", "a"})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"b", "a"}; !slices.Equal(got, want) {
		t.Fatalf("Seed = %v, want all Cases in frozen order %v", got, want)
	}
}

func TestSelectAdditionsSkipsSelectedAndContinues(t *testing.T) {
	full := []string{"a", "b", "c", "d", "e", "f"}
	got, err := iterationcases.SelectAdditions(full, []string{"a", "c"}, []string{"a", "b", "c", "d", "e", "f"})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"b", "d", "e"}; !slices.Equal(got, want) {
		t.Fatalf("SelectAdditions = %v, want %v", got, want)
	}
}

func TestSelectAdditionsRejectsInvalidReport(t *testing.T) {
	for _, report := range [][]string{{"missing"}, {"b", "b"}, {""}} {
		if _, err := iterationcases.SelectAdditions([]string{"a", "b"}, []string{"a"}, report); err == nil {
			t.Fatalf("SelectAdditions(report=%v) succeeded", report)
		}
	}
}
