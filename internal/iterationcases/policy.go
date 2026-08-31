// Package iterationcases owns the fixed product policy for selecting the
// benchmark Cases exercised by Iteration work.
package iterationcases

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"sort"
)

const (
	InitialLimit   = 10
	ExpansionLimit = 3
)

type rankedCase struct {
	id   string
	hash [sha256.Size]byte
}

// Seed deterministically selects the initial Iteration Case Set. Selection is
// independent of the Baseline's input ordering.
func Seed(fullCaseIDs []string) ([]string, error) {
	if err := validateSet("Full Case Set", fullCaseIDs, true); err != nil {
		return nil, err
	}
	if len(fullCaseIDs) < InitialLimit {
		return append([]string(nil), fullCaseIDs...), nil
	}
	ranked := make([]rankedCase, 0, len(fullCaseIDs))
	for _, id := range fullCaseIDs {
		ranked = append(ranked, rankedCase{id: id, hash: sha256.Sum256([]byte(id))})
	}
	sort.Slice(ranked, func(i, j int) bool {
		if ranked[i].hash == ranked[j].hash {
			return ranked[i].id < ranked[j].id
		}
		return bytes.Compare(ranked[i].hash[:], ranked[j].hash[:]) < 0
	})
	limit := min(InitialLimit, len(ranked))
	result := make([]string, 0, limit)
	for _, item := range ranked[:limit] {
		result = append(result, item.id)
	}
	return result, nil
}

// SelectAdditions validates a ranked Integration report and returns the first
// ExpansionLimit previously unseen Cases, skipping Cases already selected.
func SelectAdditions(fullCaseIDs, currentCaseIDs, rankedRegressionCaseIDs []string) ([]string, error) {
	if err := validateSet("Full Case Set", fullCaseIDs, true); err != nil {
		return nil, err
	}
	if err := validateSet("Iteration Case Set", currentCaseIDs, false); err != nil {
		return nil, err
	}
	full := make(map[string]struct{}, len(fullCaseIDs))
	for _, id := range fullCaseIDs {
		full[id] = struct{}{}
	}
	selected := make(map[string]struct{}, len(currentCaseIDs))
	for _, id := range currentCaseIDs {
		if _, ok := full[id]; !ok {
			return nil, fmt.Errorf("Iteration Case Set contains %q outside the Full Case Set", id)
		}
		selected[id] = struct{}{}
	}
	seenReport := make(map[string]struct{}, len(rankedRegressionCaseIDs))
	additions := make([]string, 0, min(ExpansionLimit, len(rankedRegressionCaseIDs)))
	for index, id := range rankedRegressionCaseIDs {
		if id == "" {
			return nil, fmt.Errorf("Regression Cases[%d] must be non-empty", index)
		}
		if _, ok := full[id]; !ok {
			return nil, fmt.Errorf("Regression Case %q is outside the Full Case Set", id)
		}
		if _, ok := seenReport[id]; ok {
			return nil, fmt.Errorf("Regression Cases contains duplicate %q", id)
		}
		seenReport[id] = struct{}{}
		if _, ok := selected[id]; ok || len(additions) == ExpansionLimit {
			continue
		}
		additions = append(additions, id)
		selected[id] = struct{}{}
	}
	return additions, nil
}

func validateSet(name string, ids []string, requireNonEmpty bool) error {
	if requireNonEmpty && len(ids) == 0 {
		return fmt.Errorf("%s must be non-empty", name)
	}
	seen := make(map[string]struct{}, len(ids))
	for index, id := range ids {
		if id == "" {
			return fmt.Errorf("%s[%d] must be non-empty", name, index)
		}
		if _, ok := seen[id]; ok {
			return fmt.Errorf("%s contains duplicate %q", name, id)
		}
		seen[id] = struct{}{}
	}
	return nil
}
