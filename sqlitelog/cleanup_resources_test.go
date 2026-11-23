package sqlitelog

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/agent-primitives/sessions/api"
)

type resourceRow struct {
	uid, borrower string
	confirmed     bool
}

// resourceRows reads an attempt's plan straight from the v10 tables, keyed by reference.
func resourceRows(t *testing.T, s *Store, attempt string) map[string]resourceRow {
	t.Helper()
	rows, err := s.db.Query(`SELECT r.reference,r.uid,r.confirmed,COALESCE(b.reference,'')
		FROM allocation_resources r
		LEFT JOIN allocation_borrowers e ON e.resource_id=r.id
		LEFT JOIN allocation_resources b ON b.id=e.borrower_resource_id
		WHERE r.allocation_id=?`, attempt)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]resourceRow{}
	for rows.Next() {
		var ref string
		var row resourceRow
		if err := rows.Scan(&ref, &row.uid, &row.confirmed, &row.borrower); err != nil {
			t.Fatal(err)
		}
		out[ref] = row
	}
	return out
}

func owned(t *testing.T, resources ...api.CleanupResource) api.Incarnation {
	t.Helper()
	evidence, err := api.NewCleanupEvidence(resources)
	if err != nil {
		t.Fatal(err)
	}
	return api.Incarnation{ID: "child", Cleanup: evidence}
}

// A journal written by the v0.4.0 release migrates to v10 with its legacy attempt intact and its
// inventory still uncertified.
func TestCleanupV9FixtureMigratesToV10(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cleanup-v9.db")
	blob, err := os.ReadFile("testdata/cleanup-v9.db")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, blob, 0o600); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	var version int
	if err := raw.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != 9 {
		raw.Close()
		t.Fatalf("fixture user_version=%d err=%v, want 9", version, err)
	}
	raw.Close()

	s := deletionStore(t, path)
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != 10 {
		t.Fatalf("migrated user_version=%d err=%v, want 10", version, err)
	}
	allocations, err := s.Allocations(t.Context(), "v9-session")
	if err != nil || len(allocations) != 1 || allocations[0].ResourceID != "v9-handle" || !allocations[0].Confirmed {
		t.Fatalf("migrated allocation=%+v err=%v", allocations, err)
	}
	if got := resourceRows(t, s, allocations[0].ID); len(got) != 0 {
		t.Fatalf("legacy attempt gained resource rows: %+v", got)
	}
	if deletionInfo(t, s, "v9-session").InventoryComplete {
		t.Fatal("migration certified inventory")
	}
}

// The plan is durable before I/O; a fork child's plan does not publish the child.
func TestCleanupPlanIsRecordedBeforeIO(t *testing.T) {
	s := deletionStore(t, ":memory:")
	plan := []api.CleanupResource{{Reference: "tag", Borrower: "actor"}, {Reference: "actor"}}
	attempt, err := s.Session("child").BeginUnpublishedAllocation(t.Context(), "key", "fork", plan...)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]resourceRow{"tag": {borrower: "actor"}, "actor": {}}
	if got := resourceRows(t, s, attempt); len(got) != 2 || got["tag"] != want["tag"] || got["actor"] != want["actor"] {
		t.Fatalf("plan rows=%+v, want %+v", got, want)
	}
	if _, err := s.SessionInfo("child"); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("unpublished fork child is visible: %v", err)
	}

	// An invalid plan writes nothing at all.
	cycle := []api.CleanupResource{{Reference: "a", Borrower: "b"}, {Reference: "b", Borrower: "a"}}
	if _, err := s.Session("other").BeginPlannedAllocation(t.Context(), "key", "fork", cycle...); !errors.Is(err, api.ErrCleanupResource) {
		t.Fatalf("cyclic plan err=%v", err)
	}
	if got, err := s.Allocations(t.Context(), "other"); err != nil || len(got) != 0 {
		t.Fatalf("refused plan left attempts %+v err=%v", got, err)
	}
}

// Partial evidence confirms only what it proves, the attempt stays pending until every planned
// resource is proven, a retry is idempotent, and nothing can rewrite the plan or a recorded UID.
// A late confirmation after DELETE still records, without resurrecting the session.
func TestCleanupConfirmationRecordsOnlyProvenUIDs(t *testing.T) {
	s := deletionStore(t, ":memory:")
	log := s.Session("child")
	plan := []api.CleanupResource{{Reference: "tag", Borrower: "actor"}, {Reference: "actor"}}
	attempt, err := log.BeginPlannedAllocation(t.Context(), "key", "fork", plan...)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DeleteSession(t.Context(), "child"); err != nil {
		t.Fatal(err)
	}
	pending := func(want bool) {
		t.Helper()
		got, err := s.Allocations(t.Context(), "child")
		if err != nil || len(got) != 1 || got[0].Confirmed == want {
			t.Fatalf("attempt=%+v err=%v, want pending=%v", got, err, want)
		}
	}

	// No evidence (an unknown outcome) leaves everything pending without a conflict.
	if err := log.ConfirmAllocation(t.Context(), attempt, api.Incarnation{}); err != nil {
		t.Fatalf("empty evidence: %v", err)
	}
	tag := api.CleanupResource{Reference: "tag", UID: "tag-uid", Borrower: "actor"}
	for range 2 {
		if err := log.ConfirmAllocation(t.Context(), attempt, owned(t, tag)); err != nil {
			t.Fatalf("tag evidence: %v", err)
		}
	}
	if got := resourceRows(t, s, attempt); got["tag"] != (resourceRow{uid: "tag-uid", borrower: "actor", confirmed: true}) || got["actor"].confirmed {
		t.Fatalf("after tag evidence rows=%+v", got)
	}
	pending(true)

	for name, bad := range map[string]api.CleanupResource{
		"changed uid":      {Reference: "tag", UID: "other", Borrower: "actor"},
		"changed borrower": {Reference: "actor", UID: "actor-uid", Borrower: "tag"},
		"outside the plan": {Reference: "volume", UID: "v-uid"},
	} {
		if err := log.ConfirmAllocation(t.Context(), attempt, owned(t, bad)); !errors.Is(err, ErrAllocationConflict) {
			t.Fatalf("%s: err=%v, want ErrAllocationConflict", name, err)
		}
	}

	actor := api.CleanupResource{Reference: "actor", UID: "actor-uid"}
	if err := log.ConfirmAllocation(t.Context(), attempt, owned(t, tag, actor)); err != nil {
		t.Fatal(err)
	}
	pending(false)
	if info := deletionInfo(t, s, "child"); info.DeletionState != api.DeletionPending {
		t.Fatalf("late confirmation changed deletion state to %v", info.DeletionState)
	}
}

// The ledger stays bounded over turns. A success that proves nothing (a re-attaching Exec, a
// stateless Resume) settles as a handle attempt instead of looking like a lost response; a Restore
// repeating a claim replaces the older attempt, but never one holding a claim it does not repeat,
// such as a fork's tag. A failure that proves nothing stays pending.
func TestCleanupLedgerSettlesAndCompactsRepeatedClaims(t *testing.T) {
	s := deletionStore(t, ":memory:")
	log := s.Session("s")
	actor := api.CleanupResource{Reference: "actor"}
	proven := api.CleanupResource{Reference: "actor", UID: "u1"}
	turn := func(operation string, plan []api.CleanupResource, in api.Incarnation) string {
		t.Helper()
		attempt, err := log.BeginPlannedAllocation(t.Context(), "key", operation, plan...)
		if err != nil {
			t.Fatal(err)
		}
		if err := log.ConfirmAllocation(t.Context(), attempt, in); err != nil {
			t.Fatalf("%s: %v", operation, err)
		}
		return attempt
	}
	withEvidence := func(resources ...api.CleanupResource) api.Incarnation {
		in := owned(t, resources...)
		in.ID = "s"
		return in
	}
	fork := turn("fork", []api.CleanupResource{{Reference: "tag", Borrower: "actor"}, actor},
		withEvidence(api.CleanupResource{Reference: "tag", UID: "t1", Borrower: "actor"}, proven))
	// Same claims without the tag's borrower edge: the fork attempt, and its edge, must stay.
	unpaired := turn("fork", []api.CleanupResource{{Reference: "tag"}, actor},
		withEvidence(api.CleanupResource{Reference: "tag", UID: "t1"}, proven))
	turn("create", []api.CleanupResource{actor}, withEvidence(proven))
	turn("create", []api.CleanupResource{actor}, api.Incarnation{ID: "s"})
	handle := turn("create", []api.CleanupResource{actor}, api.Incarnation{ID: "s"})
	turn("restore", []api.CleanupResource{actor}, withEvidence(proven))
	restore := turn("restore", []api.CleanupResource{actor}, withEvidence(proven))
	lost := turn("restore", []api.CleanupResource{actor}, api.Incarnation{})

	got, err := s.Allocations(t.Context(), "s")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{fork: true, unpaired: true, handle: true, restore: true, lost: false}
	if len(got) != len(want) {
		t.Fatalf("attempts=%+v, want both forks, one handle, latest restore and the lost one", got)
	}
	for _, a := range got {
		if confirmed, ok := want[a.ID]; !ok || a.Confirmed != confirmed {
			t.Fatalf("attempt %+v unexpected, want %v", a, want)
		}
	}
	var rows, edges int
	if err := s.db.QueryRow(`SELECT (SELECT COUNT(*) FROM allocation_resources),(SELECT COUNT(*) FROM allocation_borrowers)`).Scan(&rows, &edges); err != nil {
		t.Fatal(err)
	}
	if rows != 6 || edges != 1 || len(resourceRows(t, s, handle)) != 0 {
		t.Fatalf("resource rows=%d edges=%d, want 2 per fork + restore's 1 + lost's 1, the fork's edge", rows, edges)
	}
	// A success that proves nothing cannot erase a UID already recorded.
	partial := turn("fork", []api.CleanupResource{{Reference: "tag", Borrower: "actor"}, actor},
		owned(t, api.CleanupResource{Reference: "tag", UID: "t2", Borrower: "actor"}))
	if err := log.ConfirmAllocation(t.Context(), partial, api.Incarnation{ID: "s"}); !errors.Is(err, ErrAllocationConflict) {
		t.Fatalf("settling a proven attempt: err=%v, want ErrAllocationConflict", err)
	}
}
