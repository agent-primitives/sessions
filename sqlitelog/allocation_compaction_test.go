package sqlitelog

import (
	"errors"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/agent-primitives/sessions/api"
)

func beginCompactionAttempt(t *testing.T, s *Store, uid, backend, operation string) string {
	t.Helper()
	id, err := s.Session(uid).BeginAllocation(t.Context(), backend, operation)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func confirmCompactionAttempt(t *testing.T, s *Store, uid, attempt, resource string) {
	t.Helper()
	if err := s.Session(uid).ConfirmAllocation(t.Context(), attempt, api.Incarnation{ID: resource}); err != nil {
		t.Fatal(err)
	}
}

// Inspect stored rows, including released history: hiding duplicates from Allocations is not enough.
func assertCompactionRows(t *testing.T, s *Store, want ...string) {
	t.Helper()
	rows, err := s.db.Query(`SELECT id FROM allocations ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		got = append(got, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("stored attempts = %v, want %v", got, want)
	}
}

func assertCompactionState(t *testing.T, s *Store, attempt, resource string, confirmed, released bool) {
	t.Helper()
	var gotResource string
	var gotConfirmed, gotReleased bool
	if err := s.db.QueryRow(`SELECT resource_id,confirmed,released FROM allocations WHERE id=?`, attempt).
		Scan(&gotResource, &gotConfirmed, &gotReleased); err != nil {
		t.Fatal(err)
	}
	if gotResource != resource || gotConfirmed != confirmed || gotReleased != released {
		t.Fatalf("attempt %s = (%q,%t,%t), want (%q,%t,%t)", attempt,
			gotResource, gotConfirmed, gotReleased, resource, confirmed, released)
	}
}

// Operation is not resource identity; unknown outcomes and different tuples must survive.
func TestAllocationCompactionExactIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "compaction.db")
	s := deletionStore(t, path)
	old := beginCompactionAttempt(t, s, "s", "route", "create")
	confirmCompactionAttempt(t, s, "s", old, "resource")
	unknown := beginCompactionAttempt(t, s, "s", "route", "create")
	otherBackend := beginCompactionAttempt(t, s, "s", "other-route", "create")
	confirmCompactionAttempt(t, s, "s", otherBackend, "resource")
	otherResource := beginCompactionAttempt(t, s, "s", "route", "create")
	confirmCompactionAttempt(t, s, "s", otherResource, "other-resource")
	otherSession := beginCompactionAttempt(t, s, "other", "route", "create")
	confirmCompactionAttempt(t, s, "other", otherSession, "resource")
	current := beginCompactionAttempt(t, s, "s", "route", "restore")
	confirmCompactionAttempt(t, s, "s", current, "resource")
	assertCompactionRows(t, s, current, unknown, otherBackend, otherResource, otherSession)
	assertCompactionState(t, s, unknown, "", false, false)

	confirmCompactionAttempt(t, s, "s", current, "resource")
	if err := s.Session("s").ConfirmAllocation(t.Context(), current, api.Incarnation{ID: "changed"}); !errors.Is(err, ErrAllocationConflict) {
		t.Fatalf("retained identity change = %v", err)
	}
	for _, resource := range []string{"resource", "changed"} {
		if err := s.Session("s").ConfirmAllocation(t.Context(), old, api.Incarnation{ID: resource}); !errors.Is(err, ErrAllocationNotFound) {
			t.Fatalf("compacted confirmation %q = %v", resource, err)
		}
	}
	if err := s.ReleaseAllocation(t.Context(), "s", old); !errors.Is(err, ErrAllocationNotFound) {
		t.Fatalf("compacted release = %v", err)
	}
	s.Close()
	s = deletionStore(t, path)
	assertCompactionRows(t, s, current, unknown, otherBackend, otherResource, otherSession)
	assertCompactionState(t, s, current, "resource", true, false)
}

// A retry of a released confirmation must not compact the new live allocation.
func TestAllocationCompactionPreservesReleasedHistory(t *testing.T) {
	s := deletionStore(t, ":memory:")
	old := beginCompactionAttempt(t, s, "s", "route", "create")
	confirmCompactionAttempt(t, s, "s", old, "resource")
	if err := s.ReleaseAllocation(t.Context(), "s", old); err != nil {
		t.Fatal(err)
	}
	current := beginCompactionAttempt(t, s, "s", "route", "restore")
	confirmCompactionAttempt(t, s, "s", current, "resource")
	confirmCompactionAttempt(t, s, "s", old, "resource")
	if err := s.Session("s").ConfirmAllocation(t.Context(), old, api.Incarnation{ID: "changed"}); !errors.Is(err, ErrAllocationConflict) {
		t.Fatalf("released identity change = %v", err)
	}
	if err := s.ReleaseAllocation(t.Context(), "s", old); err != nil {
		t.Fatal(err)
	}
	assertCompactionRows(t, s, old, current)
	assertCompactionState(t, s, old, "resource", true, true)
	assertCompactionState(t, s, current, "resource", true, false)
}

// Confirmation order, not random attempt-ID order or begin order, chooses the retained row.
func TestAllocationCompactionLateConfirmation(t *testing.T) {
	s := deletionStore(t, ":memory:")
	a := beginCompactionAttempt(t, s, "s", "route", "create")
	b := beginCompactionAttempt(t, s, "s", "route", "restore")
	// Pin opposing ID order so sorting random IDs cannot accidentally satisfy this test.
	if _, err := s.db.Exec(`UPDATE allocations SET id=CASE id WHEN ? THEN 'a' ELSE 'z' END WHERE id IN (?,?)`, a, a, b); err != nil {
		t.Fatal(err)
	}
	confirmCompactionAttempt(t, s, "s", "z", "resource")
	assertCompactionRows(t, s, "a", "z")
	assertCompactionState(t, s, "a", "", false, false)
	confirmCompactionAttempt(t, s, "s", "a", "resource")
	assertCompactionRows(t, s, "a")
	assertCompactionState(t, s, "a", "resource", true, false)
}

// DELETE cannot discard a late known handle or make incomplete inventory look reconciled.
func TestAllocationCompactionAfterTombstone(t *testing.T) {
	s := deletionStore(t, ":memory:")
	old := beginCompactionAttempt(t, s, "s", "route", "create")
	confirmCompactionAttempt(t, s, "s", old, "resource")
	current := beginCompactionAttempt(t, s, "s", "route", "restore")
	before, err := s.DeleteSession(t.Context(), "s")
	if err != nil {
		t.Fatal(err)
	}
	confirmCompactionAttempt(t, s, "s", current, "resource")
	assertCompactionRows(t, s, current)
	assertCompactionState(t, s, current, "resource", true, false)
	if after := deletionInfo(t, s, "s"); after.InventoryComplete || after.DeletionState != api.DeletionPending || after.DeletionSeq != before.DeletionSeq {
		t.Fatalf("late confirmation changed tombstone or certified inventory: %+v", after)
	}
}

// A failed compaction must roll back the current confirmation as well as preserve the prior row.
func TestAllocationCompactionDeleteFailureRollsBack(t *testing.T) {
	s := deletionStore(t, ":memory:")
	old := beginCompactionAttempt(t, s, "s", "route", "create")
	confirmCompactionAttempt(t, s, "s", old, "resource")
	current := beginCompactionAttempt(t, s, "s", "route", "restore")
	approvalExec(t, s.db, `CREATE TRIGGER fail_compaction BEFORE DELETE ON allocations BEGIN SELECT RAISE(ABORT,'compaction failed'); END`)
	err := s.Session("s").ConfirmAllocation(t.Context(), current, api.Incarnation{ID: "resource"})
	if err == nil || !strings.Contains(err.Error(), "compaction failed") {
		t.Fatalf("compaction trigger = %v", err)
	}
	assertCompactionRows(t, s, old, current)
	assertCompactionState(t, s, current, "", false, false)
	assertCompactionState(t, s, old, "resource", true, false)
	approvalExec(t, s.db, `DROP TRIGGER fail_compaction`)
	confirmCompactionAttempt(t, s, "s", current, "resource")
	assertCompactionRows(t, s, current)
}
