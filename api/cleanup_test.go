package api

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestCleanupPlanValidation(t *testing.T) {
	valid := []CleanupResource{
		{Reference: "opaque:tag:child", Borrower: "opaque:actor:child"},
		{Reference: "opaque:actor:child"},
	}
	if err := ValidateCleanupPlan(valid); err != nil {
		t.Fatalf("valid plan: %v", err)
	}

	for _, tc := range []struct {
		name string
		plan []CleanupResource
	}{
		{"missing reference", []CleanupResource{{}}},
		{"target with uid", []CleanupResource{{Reference: "actor", UID: "uid"}}},
		{"duplicate reference", []CleanupResource{{Reference: "actor"}, {Reference: "actor"}}},
		{"self borrower", []CleanupResource{{Reference: "actor", Borrower: "actor"}}},
		{"missing borrower", []CleanupResource{{Reference: "tag", Borrower: "actor"}}},
		{"borrower cycle", []CleanupResource{{Reference: "a", Borrower: "b"}, {Reference: "b", Borrower: "a"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := ValidateCleanupPlan(tc.plan); err == nil {
				t.Fatalf("invalid plan accepted: %+v", tc.plan)
			}
		})
	}
}

func TestCleanupEvidenceAllowsPartialPlan(t *testing.T) {
	evidence, err := NewCleanupEvidence([]CleanupResource{{
		Reference: "opaque:tag:child",
		UID:       "tag-uid",
		Borrower:  "opaque:actor:child",
	}})
	if err != nil {
		t.Fatalf("partial evidence: %v", err)
	}
	want := []CleanupResource{{
		Reference: "opaque:tag:child",
		UID:       "tag-uid",
		Borrower:  "opaque:actor:child",
	}}
	if got := evidence.Resources(); !reflect.DeepEqual(got, want) {
		t.Fatalf("resources = %#v, want %#v", got, want)
	}
}

func TestCleanupEvidenceRejectsInvalidOwnership(t *testing.T) {
	for _, tc := range []struct {
		name      string
		resources []CleanupResource
	}{
		{"missing reference", []CleanupResource{{UID: "uid"}}},
		{"missing uid", []CleanupResource{{Reference: "actor"}}},
		{"duplicate reference", []CleanupResource{{Reference: "actor", UID: "a"}, {Reference: "actor", UID: "b"}}},
		{"self borrower", []CleanupResource{{Reference: "actor", UID: "uid", Borrower: "actor"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewCleanupEvidence(tc.resources); err == nil {
				t.Fatalf("invalid evidence accepted: %+v", tc.resources)
			}
		})
	}
}

func TestCleanupEvidenceReturnsDefensiveCopy(t *testing.T) {
	original, err := NewCleanupEvidence([]CleanupResource{{Reference: "actor", UID: "uid"}})
	if err != nil {
		t.Fatal(err)
	}
	inc := Incarnation{Cleanup: original}
	got := inc.Cleanup.Resources()
	got[0].Reference = "replacement"
	got[0].UID = "replacement-uid"

	want := []CleanupResource{{Reference: "actor", UID: "uid"}}
	if resources := inc.Cleanup.Resources(); !reflect.DeepEqual(resources, want) {
		t.Fatalf("evidence mutated through Resources copy: %#v", resources)
	}
	if err := inc.Cleanup.validate(); err != nil {
		t.Fatalf("stored evidence invalid after copy mutation: %v", err)
	}
}

func TestCleanupEvidenceDoesNotCarryIncarnationRoutingData(t *testing.T) {
	evidence, err := NewCleanupEvidence([]CleanupResource{{Reference: "opaque:actor", UID: "actor-uid"}})
	if err != nil {
		t.Fatal(err)
	}
	inc := Incarnation{
		ID:           "session-1",
		Worker:       "worker-secret",
		Address:      "private.example:1234",
		CallMetadata: map[string]string{"authorization": "secret"},
		Cleanup:      evidence,
	}
	blob, err := json.Marshal(inc.Cleanup.Resources())
	if err != nil {
		t.Fatal(err)
	}
	got := string(blob)
	want := `[{"Reference":"opaque:actor","UID":"actor-uid","Borrower":""}]`
	if got != want {
		t.Fatalf("cleanup evidence = %s, want %s", got, want)
	}
	for _, forbidden := range []string{"worker-secret", "private.example", "authorization", "secret"} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("cleanup evidence carries routing data %q", forbidden)
		}
	}
}

func TestCleanupValidationErrorsAreCredentialSafe(t *testing.T) {
	secret := "token=do-not-publish"
	_, err := NewCleanupEvidence([]CleanupResource{{Reference: secret}})
	if err == nil {
		t.Fatal("UID-less evidence accepted")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("validation error echoed arbitrary reference: %v", err)
	}
	if !errors.Is(err, ErrCleanupResource) {
		t.Fatalf("validation error = %v, want ErrCleanupResource", err)
	}
}
