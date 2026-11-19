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
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(schema); err != nil {
		t.Fatal(err)
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for _, migrate := range migrations[:len(migrations)-1] {
		if err := migrate(tx); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := tx.Exec(`PRAGMA user_version=9`); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return db
}

func confirmIdentity(t *testing.T, s *Store, attempt string, resource api.CleanupResource) error {
	t.Helper()
	return s.Session("session-1").ConfirmAllocation(t.Context(), attempt, api.Incarnation{CleanupResource: &resource})
}

func TestCleanupIdentityMigratesFreshAndV9(t *testing.T) {
	for _, migrated := range []bool{false, true} {
		path := filepath.Join(t.TempDir(), "journal.db")
		var oldDB *sql.DB
		if migrated {
			oldDB = makeV9Database(t, path)
			if _, err := oldDB.Exec(`INSERT INTO sessions(session,project) VALUES('legacy','tenant')`); err != nil {
				t.Fatal(err)
			}
		}
		s := deletionStore(t, path)
		if SchemaVersion != 10 {
			t.Fatalf("schema version = %d, want 10", SchemaVersion)
		}
		for _, column := range []string{"resource_kind", "resource_namespace", "resource_uid"} {
			var n int
			if err := s.db.QueryRow(`SELECT count(*) FROM pragma_table_info('allocations') WHERE name=?`, column).Scan(&n); err != nil || n != 1 {
				t.Fatalf("column %s count=%d err=%v", column, n, err)
			}
		}
		if !migrated {
			continue
		}
		if _, err := oldDB.Exec(`INSERT INTO allocations(id,session,backend,operation,resource_id,confirmed) VALUES('old-writer','legacy','route','create','legacy',1)`); err != nil {
			t.Fatal(err)
		}
		got, err := s.Allocations(t.Context(), "legacy")
		if err != nil || len(got) != 1 || got[0].ResourceUID != "" || got[0].ResourceKind != "" {
			t.Fatalf("old writer row = %+v, %v", got, err)
		}
		if deletionInfo(t, s, "legacy").InventoryComplete {
			t.Fatal("v9 row certified inventory")
		}
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
	if err != nil || len(got) != 1 || got[0].Confirmed || got[0].ResourceUID != "" {
		t.Fatalf("target = %+v, %v", got, err)
	}
	owned := target
	owned.UID = "actor-uid-1"
	if err := confirmIdentity(t, s, attempt, owned); err != nil {
		t.Fatal(err)
	}
	for _, changed := range []api.CleanupResource{
		{Kind: api.CleanupResourceTag, Namespace: owned.Namespace, Name: owned.Name, UID: owned.UID},
		{Kind: owned.Kind, Namespace: "other", Name: owned.Name, UID: owned.UID},
		{Kind: owned.Kind, Namespace: owned.Namespace, Name: "other", UID: owned.UID},
		{Kind: owned.Kind, Namespace: owned.Namespace, Name: owned.Name, UID: "replacement"},
	} {
		if err := confirmIdentity(t, s, attempt, changed); !errors.Is(err, ErrAllocationConflict) {
			t.Fatalf("changed identity %+v = %v", changed, err)
		}
	}
	if err := confirmIdentity(t, s, attempt, owned); err != nil {
		t.Fatal(err)
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
	for i, uid := range []string{"old-uid", "replacement-uid"} {
		operation := []string{"create", "restore"}[i]
		attempt, err := s.Session("session-1").BeginAllocation(t.Context(), "route", operation, target)
		if err != nil {
			t.Fatal(err)
		}
		owned := target
		owned.UID = uid
		if err := confirmIdentity(t, s, attempt, owned); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.Allocations(t.Context(), "session-1")
	if err != nil || len(got) != 2 {
		t.Fatalf("replacement compacted: %+v, %v", got, err)
	}
}
