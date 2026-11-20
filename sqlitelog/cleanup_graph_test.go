package sqlitelog

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/agent-primitives/sessions/api"
)

func cleanupPlan() []api.CleanupResource {
	return []api.CleanupResource{
		{Reference: "opaque:tag:child", Borrower: "opaque:actor:child"},
		{Reference: "opaque:actor:child"},
	}
}

func cleanupEvidence(t *testing.T, resources ...api.CleanupResource) api.Incarnation {
	t.Helper()
	e, err := api.NewCleanupEvidence(resources)
	if err != nil {
		t.Fatal(err)
	}
	return api.Incarnation{Cleanup: e}
}

func TestCleanupGraphFreshAndMigratedSchema(t *testing.T) {
	for _, path := range []string{":memory:", filepath.Join(t.TempDir(), "migrated.db")} {
		if path != ":memory:" {
			makeV9Database(t, path).Close()
		}
		s := deletionStore(t, path)
		for _, table := range []string{"allocations", "allocation_resources", "allocation_borrowers"} {
			var count int
			if err := s.db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&count); err != nil || count != 1 {
				t.Fatalf("table %s count=%d err=%v", table, count, err)
			}
		}
		var oldColumns int
		if err := s.db.QueryRow(`SELECT count(*) FROM pragma_table_info('allocations') WHERE name IN ('resource_kind','resource_namespace','resource_uid')`).Scan(&oldColumns); err != nil || oldColumns != 0 {
			t.Fatalf("old allocation identity columns=%d err=%v", oldColumns, err)
		}
	}
}

func TestCleanupGraphBeginPersistsPlanWithoutPublishing(t *testing.T) {
	s := deletionStore(t, ":memory:")
	attempt, err := s.Session("child").BeginUnpublishedAllocation(t.Context(), "route-a", "fork", cleanupPlan()...)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SessionInfo("child"); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("unpublished child found: %v", err)
	}
	page, err := s.ListSessions(ListOptions{})
	if err != nil || len(page.Sessions) != 0 {
		t.Fatalf("unpublished child listed: %+v, %v", page.Sessions, err)
	}
	resources, err := s.AllocationResources(t.Context(), "child", attempt)
	if err != nil || len(resources) != 2 {
		t.Fatalf("resources = %+v, %v", resources, err)
	}
	byRef := map[string]AllocationResource{}
	for _, resource := range resources {
		byRef[resource.Reference] = resource
	}
	if byRef["opaque:tag:child"].Borrower != "opaque:actor:child" {
		t.Fatalf("borrower = %q", byRef["opaque:tag:child"].Borrower)
	}
	if byRef["opaque:tag:child"].Confirmed || byRef["opaque:actor:child"].Confirmed {
		t.Fatal("new targets confirmed")
	}
}

func TestCleanupGraphConfirmsPartialEvidenceAndRejectsMutation(t *testing.T) {
	s := deletionStore(t, ":memory:")
	attempt, err := s.Session("child").BeginAllocation(t.Context(), "route-a", "fork", cleanupPlan()...)
	if err != nil {
		t.Fatal(err)
	}
	tag := api.CleanupResource{Reference: "opaque:tag:child", UID: "tag-uid", Borrower: "opaque:actor:child"}
	if err := s.Session("child").ConfirmAllocation(t.Context(), attempt, cleanupEvidence(t, tag)); err != nil {
		t.Fatal(err)
	}
	resources, err := s.AllocationResources(t.Context(), "child", attempt)
	if err != nil {
		t.Fatal(err)
	}
	for _, resource := range resources {
		switch resource.Reference {
		case tag.Reference:
			if !resource.Confirmed || resource.UID != tag.UID {
				t.Fatalf("tag = %+v", resource)
			}
		case "opaque:actor:child":
			if resource.Confirmed {
				t.Fatalf("unobserved actor confirmed: %+v", resource)
			}
		}
	}
	if err := s.Session("child").ConfirmAllocation(t.Context(), attempt, cleanupEvidence(t, tag)); err != nil {
		t.Fatalf("identical retry: %v", err)
	}
	changed := tag
	changed.UID = "replacement"
	if err := s.Session("child").ConfirmAllocation(t.Context(), attempt, cleanupEvidence(t, changed)); !errors.Is(err, ErrAllocationConflict) {
		t.Fatalf("changed uid = %v", err)
	}
	unknown := api.CleanupResource{Reference: "opaque:other", UID: "other-uid"}
	if err := s.Session("child").ConfirmAllocation(t.Context(), attempt, cleanupEvidence(t, unknown)); !errors.Is(err, ErrAllocationConflict) {
		t.Fatalf("unknown resource = %v", err)
	}
}

func TestCleanupGraphAliasKeepsLateConfirmation(t *testing.T) {
	s := deletionStore(t, ":memory:")
	actor := api.CleanupResource{Reference: "opaque:actor:session"}
	first, err := s.Session("session").BeginAllocation(t.Context(), "route", "create", actor)
	if err != nil {
		t.Fatal(err)
	}
	owned := actor
	owned.UID = "actor-uid"
	if err := s.Session("session").ConfirmAllocation(t.Context(), first, cleanupEvidence(t, owned)); err != nil {
		t.Fatal(err)
	}
	second, err := s.Session("session").BeginAllocation(t.Context(), "route", "restore", actor)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Session("session").ConfirmAllocation(t.Context(), second, cleanupEvidence(t, owned)); err != nil {
		t.Fatal(err)
	}
	if err := s.Session("session").ConfirmAllocation(t.Context(), first, cleanupEvidence(t, owned)); err != nil {
		t.Fatalf("late first confirmation: %v", err)
	}
	for _, attempt := range []string{first, second} {
		resources, err := s.AllocationResources(t.Context(), "session", attempt)
		if err != nil || len(resources) != 1 {
			t.Fatalf("attempt %s resources=%+v err=%v", attempt, resources, err)
		}
	}
	secondResources, _ := s.AllocationResources(t.Context(), "session", second)
	if secondResources[0].CanonicalID != "" {
		firstResources, _ := s.AllocationResources(t.Context(), "session", first)
		if secondResources[0].CanonicalID != firstResources[0].ID {
			t.Fatalf("alias = %q, first = %q", secondResources[0].CanonicalID, firstResources[0].ID)
		}
		continueAlias := true
		_ = continueAlias
	} else {
		firstResources, _ := s.AllocationResources(t.Context(), "session", first)
		if firstResources[0].CanonicalID != secondResources[0].ID {
			t.Fatalf("alias = %q, second = %q", firstResources[0].CanonicalID, secondResources[0].ID)
		}
	}
}

func TestCleanupGraphBorrowerFollowsCanonicalAliases(t *testing.T) {
	s := deletionStore(t, ":memory:")
	plan := cleanupPlan()
	one, err := s.Session("child").BeginAllocation(t.Context(), "route", "fork", plan...)
	if err != nil {
		t.Fatal(err)
	}
	owned := []api.CleanupResource{
		{Reference: "opaque:tag:child", UID: "tag-uid", Borrower: "opaque:actor:child"},
		{Reference: "opaque:actor:child", UID: "actor-uid"},
	}
	if err := s.Session("child").ConfirmAllocation(t.Context(), one, cleanupEvidence(t, owned...)); err != nil {
		t.Fatal(err)
	}
	two, err := s.Session("child").BeginAllocation(t.Context(), "route", "restore", plan...)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Session("child").ConfirmAllocation(t.Context(), two, cleanupEvidence(t, owned...)); err != nil {
		t.Fatal(err)
	}
	blocked, err := s.CleanupResourceBlocked(t.Context(), "child", two, "opaque:tag:child")
	if err != nil || !blocked {
		t.Fatalf("tag blocked=%t err=%v", blocked, err)
	}
	if err := s.ReleaseAllocation(t.Context(), "child", one); err != nil {
		t.Fatal(err)
	}
	blocked, err = s.CleanupResourceBlocked(t.Context(), "child", two, "opaque:tag:child")
	if err != nil || blocked {
		t.Fatalf("tag still blocked=%t err=%v", blocked, err)
	}
}

func TestCleanupGraphAliasRejectsCanonicalCycle(t *testing.T) {
	s := deletionStore(t, ":memory:")
	planOne := []api.CleanupResource{{Reference: "a", Borrower: "b"}, {Reference: "b"}}
	planTwo := []api.CleanupResource{{Reference: "a"}, {Reference: "b", Borrower: "a"}}
	one, err := s.Session("session").BeginAllocation(t.Context(), "route", "create", planOne...)
	if err != nil {
		t.Fatal(err)
	}
	ownedOne := []api.CleanupResource{{Reference: "a", UID: "a-uid", Borrower: "b"}, {Reference: "b", UID: "b-uid"}}
	if err := s.Session("session").ConfirmAllocation(t.Context(), one, cleanupEvidence(t, ownedOne...)); err != nil {
		t.Fatal(err)
	}
	two, err := s.Session("session").BeginAllocation(t.Context(), "route", "restore", planTwo...)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Session("session").ConfirmAllocation(t.Context(), two, cleanupEvidence(t, api.CleanupResource{Reference: "a", UID: "a-uid"})); err != nil {
		t.Fatal(err)
	}
	if err := s.Session("session").ConfirmAllocation(t.Context(), two, cleanupEvidence(t, api.CleanupResource{Reference: "b", UID: "b-uid", Borrower: "a"})); !errors.Is(err, ErrAllocationConflict) {
		t.Fatalf("canonical cycle accepted: %v", err)
	}
	resources, err := s.AllocationResources(t.Context(), "session", two)
	if err != nil {
		t.Fatal(err)
	}
	for _, resource := range resources {
		if resource.Reference == "b" && resource.Confirmed {
			t.Fatalf("failed alias confirmation committed: %+v", resource)
		}
	}
}

func TestCleanupGraphRejectsPlanCycleBeforeIO(t *testing.T) {
	s := deletionStore(t, ":memory:")
	plan := []api.CleanupResource{{Reference: "a", Borrower: "b"}, {Reference: "b", Borrower: "a"}}
	if _, err := s.Session("session").BeginAllocation(t.Context(), "route", "fork", plan...); !errors.Is(err, api.ErrCleanupResource) {
		t.Fatalf("cycle = %v", err)
	}
	allocations, err := s.Allocations(t.Context(), "session")
	if err != nil || len(allocations) != 0 {
		t.Fatalf("allocations after refused cycle=%+v err=%v", allocations, err)
	}
}

func TestCleanupGraphLateConfirmationDoesNotResurrectSession(t *testing.T) {
	s := deletionStore(t, ":memory:")
	actor := api.CleanupResource{Reference: "opaque:actor:session"}
	attempt, err := s.Session("session").BeginAllocation(t.Context(), "route", "create", actor)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DeleteSession(t.Context(), "session"); err != nil {
		t.Fatal(err)
	}
	owned := actor
	owned.UID = "actor-uid"
	if err := s.Session("session").ConfirmAllocation(t.Context(), attempt, cleanupEvidence(t, owned)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SessionInfo("session"); err != nil {
		info := deletionInfo(t, s, "session")
		if info.DeletionState != api.DeletionPending {
			t.Fatalf("deletion state = %v", info.DeletionState)
		}
	}
}

func TestCleanupGraphLegacyAllocationRemainsCompatible(t *testing.T) {
	s := deletionStore(t, ":memory:")
	attempt, err := s.Session("legacy").BeginAllocation(t.Context(), "local", "create")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Session("legacy").ConfirmAllocation(t.Context(), attempt, api.Incarnation{ID: "handle"}); err != nil {
		t.Fatal(err)
	}
	allocations, err := s.Allocations(t.Context(), "legacy")
	if err != nil || len(allocations) != 1 || allocations[0].ResourceID != "handle" || !allocations[0].Confirmed {
		t.Fatalf("legacy allocation=%+v err=%v", allocations, err)
	}
	resources, err := s.AllocationResources(t.Context(), "legacy", attempt)
	if err != nil || len(resources) != 0 {
		t.Fatalf("legacy resource rows=%+v err=%v", resources, err)
	}
}
