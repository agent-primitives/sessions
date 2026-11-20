package sqlitelog

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
)

// makeV9Database exercises every migration before the normalized resource rung. It is a
// fault-injection helper; release compatibility also uses the checked-in v0.4.0-derived fixture.
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

func TestSchemaV9CleanupFixtureMigrates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cleanup-v9.db")
	blob, err := os.ReadFile("testdata/cleanup-v9.db")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, blob, 0600); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	var version int
	if err := raw.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != 9 {
		raw.Close()
		t.Fatalf("fixture version=%d err=%v", version, err)
	}
	var count int
	if err := raw.QueryRow(`SELECT count(*) FROM allocations`).Scan(&count); err != nil || count != 1 {
		raw.Close()
		t.Fatalf("fixture allocations=%d err=%v", count, err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	s := deletionStore(t, path)
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != SchemaVersion {
		t.Fatalf("migrated version=%d err=%v", version, err)
	}
	allocations, err := s.Allocations(t.Context(), "v9-session")
	if err != nil || len(allocations) != 1 || allocations[0].ResourceID != "v9-handle" || !allocations[0].Confirmed {
		t.Fatalf("migrated allocation=%+v err=%v", allocations, err)
	}
	if deletionInfo(t, s, "v9-session").InventoryComplete {
		t.Fatal("fixture migration certified incomplete inventory")
	}
}

func TestCleanupIdentityMigratesFreshAndV9(t *testing.T) {
	for _, migrated := range []bool{false, true} {
		path := filepath.Join(t.TempDir(), "journal.db")
		var oldDB *sql.DB
		if migrated {
			oldDB = makeV9Database(t, path)
			if _, err := oldDB.Exec(`INSERT INTO sessions(session,project) VALUES('legacy','tenant')`); err != nil {
				oldDB.Close()
				t.Fatal(err)
			}
			if _, err := oldDB.Exec(`INSERT INTO allocations(id,session,backend,operation,resource_id,confirmed) VALUES('old-writer','legacy','route','create','legacy',1)`); err != nil {
				oldDB.Close()
				t.Fatal(err)
			}
			if err := oldDB.Close(); err != nil {
				t.Fatal(err)
			}
		}
		s := deletionStore(t, path)
		if SchemaVersion != 10 {
			t.Fatalf("schema version = %d, want 10", SchemaVersion)
		}
		for _, table := range []string{"allocation_resources", "allocation_borrowers"} {
			var count int
			if err := s.db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&count); err != nil || count != 1 {
				t.Fatalf("table %s count=%d err=%v", table, count, err)
			}
		}
		var predecessorColumns int
		if err := s.db.QueryRow(`SELECT count(*) FROM pragma_table_info('allocations') WHERE name IN ('resource_kind','resource_namespace','resource_uid')`).Scan(&predecessorColumns); err != nil || predecessorColumns != 0 {
			t.Fatalf("predecessor columns=%d err=%v", predecessorColumns, err)
		}
		if migrated {
			rows, err := s.Allocations(t.Context(), "legacy")
			if err != nil || len(rows) != 1 || rows[0].ResourceID != "legacy" || !rows[0].Confirmed {
				t.Fatalf("legacy row=%+v err=%v", rows, err)
			}
			if deletionInfo(t, s, "legacy").InventoryComplete {
				t.Fatal("v9 row certified inventory")
			}
		}
	}
}
