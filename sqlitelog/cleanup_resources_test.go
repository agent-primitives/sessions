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
	if _, err := s.Session("other").BeginAllocation(t.Context(), "key", "fork", cycle...); !errors.Is(err, api.ErrCleanupResource) {
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
	attempt, err := log.BeginAllocation(t.Context(), "key", "fork", plan...)
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
