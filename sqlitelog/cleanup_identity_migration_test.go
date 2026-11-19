package sqlitelog

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	"github.com/agent-primitives/sessions/api"
)

func makeV9Database(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		t.Fatal(err)
	}
	tx, err := db.Begin()
	if err != nil {
		db.Close()
		t.Fatal(err)
	}
	for _, migrate := range migrations[:len(migrations)-1] {
		if err := migrate(tx); err != nil {
			tx.Rollback()
			db.Close()
			t.Fatal(err)
		}
	}
	if _, err := tx.Exec(`PRAGMA user_version=9`); err != nil {
		tx.Rollback()
		db.Close()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		db.Close()
		t.Fatal(err)
	}
	return db
}

func TestCleanupIdentityMigratesFreshAndV9Databases(t *testing.T) {
	for _, migrated := range []bool{false, true} {
		name := "fresh"
		if migrated {
			name = "v9"
		}
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "journal.db")
			if migrated {
				db := makeV9Database(t, path)
				if _, err := db.Exec(`INSERT INTO sessions(session,project) VALUES('legacy','tenant')`); err != nil {
					t.Fatal(err)
				}
				db.Close()
			}
			s, err := Open(path, WithDefaultProject("tenant"))
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			if SchemaVersion != 10 {
				t.Fatalf("schema version = %d, want 10", SchemaVersion)
			}
			for _, column := range []string{"resource_kind", "resource_namespace", "resource_uid"} {
				var n int
				if err := s.db.QueryRow(`SELECT count(*) FROM pragma_table_info('allocations') WHERE name=?`, column).Scan(&n); err != nil || n != 1 {
					t.Fatalf("column %s count=%d err=%v", column, n, err)
				}
			}
			for _, object := range []string{"fork_batches", "sessions_deletion"} {
				var n int
				if err := s.db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name=?`, object).Scan(&n); err != nil || n != 0 {
					t.Fatalf("future object %s count=%d err=%v", object, n, err)
				}
			}
			if migrated && deletionInfo(t, s, "legacy").InventoryComplete {
				t.Fatal("v9 row was auto-certified")
			}
		})
	}
}

func TestCleanupIdentityPersistsAndRejectsReplacement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.db")
	s := deletionStore(t, path)
	target := api.CleanupResource{Kind: api.CleanupResourceActor, Namespace: "space", Name: "session-1"}
	attempt, err := s.Session("session-1").BeginAllocation(t.Context(), "route-a", "create", target)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.Allocations(t.Context(), "session-1")
	if err != nil || len(got) != 1 {
		t.Fatalf("allocations = %+v, %v", got, err)
	}
	if got[0].ResourceKind != target.Kind || got[0].ResourceNamespace != target.Namespace || got[0].ResourceID != target.Name || got[0].ResourceUID != "" || got[0].Confirmed {
		t.Fatalf("target = %+v", got[0])
	}

	owned := target
	owned.UID = "actor-uid-1"
	if err := s.Session("session-1").ConfirmAllocation(t.Context(), attempt, api.Incarnation{CleanupResource: &owned}); err != nil {
		t.Fatal(err)
	}
	for _, changed := range []api.CleanupResource{
		{Kind: api.CleanupResourceTag, Namespace: owned.Namespace, Name: owned.Name, UID: owned.UID},
		{Kind: owned.Kind, Namespace: "other", Name: owned.Name, UID: owned.UID},
		{Kind: owned.Kind, Namespace: owned.Namespace, Name: "other", UID: owned.UID},
		{Kind: owned.Kind, Namespace: owned.Namespace, Name: owned.Name, UID: "replacement"},
	} {
		if err := s.Session("session-1").ConfirmAllocation(t.Context(), attempt, api.Incarnation{CleanupResource: &changed}); !errors.Is(err, ErrAllocationConflict) {
			t.Fatalf("changed identity %+v = %v", changed, err)
		}
	}
	if err := s.Session("session-1").ConfirmAllocation(t.Context(), attempt, api.Incarnation{CleanupResource: &owned}); err != nil {
		t.Fatalf("idempotent identity = %v", err)
	}
	s.Close()
	s = deletionStore(t, path)
	got, err = s.Allocations(t.Context(), "session-1")
	if err != nil || len(got) != 1 || got[0].ResourceUID != owned.UID || !got[0].Confirmed {
		t.Fatalf("reopened identity = %+v, %v", got, err)
	}
}

func TestCleanupIdentityCompactionKeepsReplacementUID(t *testing.T) {
	s := deletionStore(t, ":memory:")
	target := api.CleanupResource{Kind: api.CleanupResourceActor, Namespace: "space", Name: "session-1"}
	old, err := s.Session("session-1").BeginAllocation(t.Context(), "route", "create", target)
	if err != nil {
		t.Fatal(err)
	}
	oldOwned := target
	oldOwned.UID = "old-uid"
	if err := s.Session("session-1").ConfirmAllocation(t.Context(), old, api.Incarnation{CleanupResource: &oldOwned}); err != nil {
		t.Fatal(err)
	}
	current, err := s.Session("session-1").BeginAllocation(t.Context(), "route", "restore", target)
	if err != nil {
		t.Fatal(err)
	}
	newOwned := target
	newOwned.UID = "replacement-uid"
	if err := s.Session("session-1").ConfirmAllocation(t.Context(), current, api.Incarnation{CleanupResource: &newOwned}); err != nil {
		t.Fatal(err)
	}
	got, err := s.Allocations(t.Context(), "session-1")
	if err != nil || len(got) != 2 {
		t.Fatalf("replacement compacted: %+v, %v", got, err)
	}
}

func TestCleanupIdentityOldWriterStaysUnresolved(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.db")
	oldDB := makeV9Database(t, path)
	if _, err := oldDB.Exec(`INSERT INTO sessions(session,project) VALUES('session-1','tenant')`); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path, WithDefaultProject("tenant"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := oldDB.Exec(`INSERT INTO allocations(id,session,backend,operation,resource_id,confirmed) VALUES('old-writer','session-1','route','create','session-1',1)`); err != nil {
		t.Fatal(err)
	}
	got, err := s.Allocations(t.Context(), "session-1")
	if err != nil || len(got) != 1 || got[0].ResourceUID != "" || got[0].ResourceKind != "" {
		t.Fatalf("old writer row = %+v, %v", got, err)
	}
	if deletionInfo(t, s, "session-1").InventoryComplete {
		t.Fatal("old writer row certified inventory")
	}
}
