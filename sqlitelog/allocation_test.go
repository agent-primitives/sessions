package sqlitelog

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/agent-primitives/sessions/api"
)

// Losing an unconfirmed attempt on reopen would let cleanup falsely certify absence.
func TestAllocationIntentAndConfirmationSurviveReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "ledger.db")
	s := deletionStore(t, path)
	attempt, err := s.Session("implicit").BeginAllocation(ctx, "route-a", "create")
	if err != nil || attempt == "" {
		t.Fatalf("begin = %q, %v", attempt, err)
	}
	info := deletionInfo(t, s, "implicit")
	if info.InventoryComplete || info.Project != "tenant" || info.LastSeq != 0 {
		t.Fatalf("implicit metadata = %+v", info)
	}
	second, err := s.Session("implicit").BeginAllocation(ctx, "route-b", "restore")
	if err != nil || second == attempt {
		t.Fatalf("second = %q, %v", second, err)
	}
	s.Close()
	s = deletionStore(t, path)
	got, err := s.Allocations(ctx, "implicit")
	if err != nil || len(got) != 2 {
		t.Fatalf("reopened allocations = %+v, %v", got, err)
	}
	var found bool
	for _, a := range got {
		if a.ID == attempt {
			found = a.SessionUID == "implicit" && a.Backend == "route-a" && a.Operation == "create" && !a.Confirmed && a.ResourceID == ""
		}
	}
	if !found {
		t.Fatalf("lost exact intent: %+v", got)
	}
	if err := s.Session("other").ConfirmAllocation(ctx, attempt, api.Incarnation{ID: "owned"}); !errors.Is(err, ErrAllocationNotFound) {
		t.Fatalf("foreign confirmation = %v", err)
	}
	if err := s.Session("implicit").ConfirmAllocation(ctx, attempt, api.Incarnation{}); !errors.Is(err, ErrAllocationConflict) {
		t.Fatalf("empty identity = %v", err)
	}
	if err := s.Session("implicit").ConfirmAllocation(ctx, attempt, api.Incarnation{ID: "owned", Address: "secret", CallMetadata: map[string]string{"token": "private"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.Session("implicit").ConfirmAllocation(ctx, attempt, api.Incarnation{ID: "owned", Address: "changed"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Session("implicit").ConfirmAllocation(ctx, attempt, api.Incarnation{ID: "replacement"}); !errors.Is(err, ErrAllocationConflict) {
		t.Fatalf("identity mutation = %v", err)
	}
	s.Close()
	s = deletionStore(t, path)
	got, err = s.Allocations(ctx, "implicit")
	if err != nil {
		t.Fatal(err)
	}
	found = false
	for _, a := range got {
		if a.ID == attempt {
			found = a.Confirmed && a.ResourceID == "owned"
		}
	}
	if !found {
		t.Fatalf("confirmation lost on reopen: %+v", got)
	}
	if err := s.ReleaseAllocation(ctx, "other", attempt); !errors.Is(err, ErrAllocationNotFound) {
		t.Fatalf("foreign release = %v", err)
	}
	if err := s.ReleaseAllocation(ctx, "implicit", attempt); err != nil {
		t.Fatal(err)
	}
	if err := s.ReleaseAllocation(ctx, "implicit", attempt); err != nil {
		t.Fatal(err)
	}
	got, err = s.Allocations(ctx, "implicit")
	if err != nil || len(got) != 1 || got[0].ID != second {
		t.Fatalf("unreleased = %+v, %v", got, err)
	}
	s.Close()
	s = deletionStore(t, path)
	got, err = s.Allocations(ctx, "implicit")
	if err != nil || len(got) != 1 || got[0].ID != second {
		t.Fatalf("reopened release = %+v, %v", got, err)
	}
}

func openV4Fixture(t *testing.T, path string) {
	t.Helper()
	blob, err := os.ReadFile("testdata/approval-v3.db")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, blob, 0600); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	// Preserve the historical rung: this fixture has never passed through a post-v4 Open.
	if err := migrateV3ToV4(tx); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`INSERT INTO sessions(session, project) VALUES('legacy','tenant'); PRAGMA user_version=4`); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

// A migration failure during the old-row update rolls back the new column and schema stamp.
func TestCleanupMigrationRollback(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v4.db")
	openV4Fixture(t, path)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	approvalExec(t, db, `CREATE TRIGGER fail_inventory BEFORE UPDATE ON sessions BEGIN SELECT RAISE(ABORT,'inventory migration failed'); END`)
	s, err := Open(path)
	if err == nil {
		s.Close()
		t.Fatal("migration ignored inventory trigger")
	}
	var version int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != 4 {
		t.Fatalf("failed migration stamp=%d %v", version, err)
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('sessions') WHERE name='inventory_complete'`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("failed migration leaked column=%d %v", count, err)
	}
	approvalExec(t, db, `DROP TRIGGER fail_inventory`)
	s = deletionStore(t, path)
	if deletionInfo(t, s, "legacy").InventoryComplete {
		t.Fatal("migration retry certified old inventory")
	}
}
