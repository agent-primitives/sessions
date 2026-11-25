package api

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

// A plan names each resource once, with no UID yet, and its borrower edges form no dangling
// reference or cycle. Errors never echo a reference, which RPC errors could carry outside
// authorization.
func TestCleanupPlanValidation(t *testing.T) {
	valid := []CleanupResource{{Reference: "tag", Borrower: "actor"}, {Reference: "actor"}}
	if err := ValidateCleanupPlan(valid); err != nil {
		t.Fatalf("valid plan: %v", err)
	}
	for name, plan := range map[string][]CleanupResource{
		"missing reference":   {{}},
		"target with uid":     {{Reference: "token=secret", UID: "uid"}},
		"duplicate reference": {{Reference: "actor"}, {Reference: "actor"}},
		"self borrower":       {{Reference: "actor", Borrower: "actor"}},
		"missing borrower":    {{Reference: "tag", Borrower: "actor"}},
		"borrower cycle":      {{Reference: "a", Borrower: "b"}, {Reference: "b", Borrower: "a"}},
	} {
		err := ValidateCleanupPlan(plan)
		if !errors.Is(err, ErrCleanupResource) || strings.Contains(err.Error(), "secret") {
			t.Errorf("%s: err=%v, want ErrCleanupResource without the reference", name, err)
		}
	}
}

// Evidence may be partial (a borrower outside it), but every resource has a UID and appears once.
// Callers cannot change stored evidence through the slice Resources returns.
func TestCleanupEvidence(t *testing.T) {
	partial := []CleanupResource{{Reference: "tag", UID: "tag-uid", Borrower: "actor"}}
	evidence, err := NewCleanupEvidence(partial)
	if err != nil {
		t.Fatalf("partial evidence: %v", err)
	}
	evidence.Resources()[0].UID = "replacement"
	if got := evidence.Resources(); !reflect.DeepEqual(got, partial) {
		t.Fatalf("stored evidence changed through Resources: %+v", got)
	}
	for name, resources := range map[string][]CleanupResource{
		"missing reference":   {{UID: "uid"}},
		"missing uid":         {{Reference: "actor"}},
		"duplicate reference": {{Reference: "actor", UID: "a"}, {Reference: "actor", UID: "b"}},
		"self borrower":       {{Reference: "actor", UID: "uid", Borrower: "actor"}},
	} {
		if _, err := NewCleanupEvidence(resources); !errors.Is(err, ErrCleanupResource) {
			t.Errorf("%s: err=%v, want ErrCleanupResource", name, err)
		}
	}
}
