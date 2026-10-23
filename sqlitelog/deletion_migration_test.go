package sqlitelog_test

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/agent-primitives/sessions/api"
	"github.com/agent-primitives/sessions/sqlitelog"
)

// This is the original v0.1.x CREATE, not a fixture made through the current Open.
const originalV1Create = `
CREATE TABLE IF NOT EXISTS sessions (
  session       TEXT PRIMARY KEY,
  fence         INTEGER NOT NULL DEFAULT 0,
  project       TEXT    NOT NULL DEFAULT '',
  name          TEXT    NOT NULL DEFAULT '',
  harness       TEXT    NOT NULL DEFAULT '',
  model         TEXT    NOT NULL DEFAULT '',
  parent_uid    TEXT    NOT NULL DEFAULT '',
  fork_seq      INTEGER NOT NULL DEFAULT 0,
  compute_state TEXT    NOT NULL DEFAULT '',
  created_at    INTEGER NOT NULL DEFAULT 0,
  updated_at    INTEGER NOT NULL DEFAULT 0,
  -- Caller-supplied metadata, stored as proto3-JSON. These are opaque to the store: it round-trips
  -- them so a session reports what its creator attached, and nothing here is interpreted or
  -- enforced. identity in particular is provenance, not authorization -- see docs/security.md.
  labels        TEXT    NOT NULL DEFAULT '',
  annotations   TEXT    NOT NULL DEFAULT '',
  origin        TEXT    NOT NULL DEFAULT '',
  identity      TEXT    NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS events (
  session   TEXT    NOT NULL,
  seq       INTEGER NOT NULL,
  prev_hash TEXT    NOT NULL,
  hash      TEXT    NOT NULL,
  fence     INTEGER NOT NULL,
  event     BLOB    NOT NULL,
  PRIMARY KEY (session, seq)
);
CREATE INDEX IF NOT EXISTS sessions_project_created
  ON sessions(project, created_at DESC, session);`

// Literal pre-deletion v2 shape: do not use current migration helpers to construct legacy fixtures.
const originalV2Rung = `CREATE TABLE IF NOT EXISTS harnesses (
  name          TEXT    PRIMARY KEY,
  uid           TEXT    NOT NULL,
  spec          TEXT    NOT NULL, -- canonical proto3-JSON of HarnessSpec
  spec_digest   TEXT    NOT NULL,
  state         TEXT    NOT NULL, -- 'active' | 'retired'
  retire_reason TEXT    NOT NULL DEFAULT '',
  retired_at    INTEGER NOT NULL DEFAULT 0,
  created_at    INTEGER NOT NULL,
  updated_at    INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS reserved_harness_names (
  name          TEXT    PRIMARY KEY,
  reserved_at   INTEGER NOT NULL
);
INSERT OR IGNORE INTO reserved_harness_names(name, reserved_at)
  SELECT DISTINCT harness, CAST(strftime('%s', 'now') AS INTEGER) * 1000000000
  FROM sessions WHERE harness != '';
CREATE INDEX IF NOT EXISTS sessions_harness ON sessions(harness);`

func rawDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func queryValues(t *testing.T, db *sql.DB, query string) [][]any {
	t.Helper()
	rows, err := db.Query(query)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	var result [][]any
	for rows.Next() {
		values := make([]any, len(cols))
		args := make([]any, len(cols))
		for i := range values {
			args[i] = &values[i]
		}
		if err := rows.Scan(args...); err != nil {
			t.Fatal(err)
		}
		result = append(result, values)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestDeletionMigrationAddsActiveRows(t *testing.T) {
	for _, version := range []int{0, 1, 2} {
		t.Run(map[int]string{0: "unstamped", 1: "v1", 2: "v2"}[version], func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "legacy.db")
			db := rawDB(t, path)
			if _, err := db.Exec(originalV1Create); err != nil {
				t.Fatal(err)
			}
			if version == 2 {
				if _, err := db.Exec(originalV2Rung); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := db.Exec(`INSERT INTO sessions(session, fence, project, compute_state, created_at, updated_at)
				VALUES ('empty',0,'acme','',11,12), ('implicit',3,'default','',21,22),
				       ('live',4,'acme','LIVE',31,32), ('cold',5,'acme','COLD',41,42)`); err != nil {
				t.Fatal(err)
			}
			setUserVersion(t, path, version)
			s := mustOpen(t, path)
			if got := readUserVersion(t, path); got != sqlitelog.SchemaVersion {
				t.Fatalf("stamp = %d, want %d", got, sqlitelog.SchemaVersion)
			}
			rows, err := db.Query(`SELECT session, fence, compute_state, created_at, updated_at,
				deletion_state, deletion_requested_at, deletion_completed_at, deletion_seq FROM sessions ORDER BY session`)
			if err != nil {
				t.Fatalf("migration did not add deletion shape: %v", err)
			}
			defer rows.Close()
			var got [][]any
			for rows.Next() {
				var uid, compute, state string
				var fence, created, updated, requested, completed, seq int64
				if err := rows.Scan(&uid, &fence, &compute, &created, &updated, &state, &requested, &completed, &seq); err != nil {
					t.Fatal(err)
				}
				got = append(got, []any{uid, fence, compute, created, updated, state, requested, completed, seq})
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			want := [][]any{
				{"cold", int64(5), "COLD", int64(41), int64(42), "ACTIVE", int64(0), int64(0), int64(0)},
				{"empty", int64(0), "", int64(11), int64(12), "ACTIVE", int64(0), int64(0), int64(0)},
				{"implicit", int64(3), "", int64(21), int64(22), "ACTIVE", int64(0), int64(0), int64(0)},
				{"live", int64(4), "LIVE", int64(31), int64(32), "ACTIVE", int64(0), int64(0), int64(0)},
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("rows after migration = %v, want %v", got, want)
			}
			rows.Close()
			for _, uid := range []string{"cold", "empty", "implicit", "live"} {
				info, err := s.SessionInfo(uid)
				if err != nil {
					t.Fatal(err)
				}
				if info.DeletionState != api.DeletionActive || !info.DeletionRequestedAt.IsZero() || info.DeletionSeq != 0 || info.LastSeq != 0 {
					t.Fatalf("migrated deletion metadata invented history: %+v", info)
				}
			}
			fresh := filepath.Join(t.TempDir(), "fresh.db")
			mustOpen(t, fresh).Close()
			if got, want := schemaOf(t, path), schemaOf(t, fresh); !reflect.DeepEqual(got, want) {
				t.Fatalf("legacy/fresh schema drift: %v != %v", got, want)
			}
			if got, want := queryValues(t, db, `PRAGMA table_info(sessions)`), queryValues(t, rawDB(t, fresh), `PRAGMA table_info(sessions)`); !reflect.DeepEqual(got, want) || len(got) != 31 {
				t.Fatalf("legacy/fresh columns drift: %v != %v", got, want)
			}
		})
	}
}

func TestDeletionMigrationRetainsReleaseFixtureBytesAndRegistry(t *testing.T) {
	original, err := os.ReadFile(v012Fixture)
	if err != nil {
		t.Fatal(err)
	}
	for _, version := range []int{1, 2} {
		t.Run(map[int]string{1: "release-v1", 2: "genuine-v2"}[version], func(t *testing.T) {
			path := copyFixture(t, v012Fixture)
			db := rawDB(t, path)
			if version == 2 {
				if _, err := db.Exec(originalV2Rung + `
					INSERT INTO harnesses(name,uid,spec,spec_digest,state,retire_reason,retired_at,created_at,updated_at)
					VALUES ('retained','h-uid','{}','digest','retired','audit',10,9,11);
					INSERT INTO reserved_harness_names(name,reserved_at) VALUES ('static',12);
					PRAGMA user_version = 2;`); err != nil {
					t.Fatal(err)
				}
			}
			const oldSessionQuery = `SELECT session,fence,project,name,harness,model,parent_uid,fork_seq,compute_state,
				created_at,updated_at,labels,annotations,origin,identity FROM sessions ORDER BY session`
			const oldEventQuery = `SELECT session,seq,prev_hash,hash,fence,event FROM events ORDER BY session,seq`
			metadata := queryValues(t, db, oldSessionQuery)
			events := queryValues(t, db, oldEventQuery)
			var registry, reserved [][]any
			if version == 2 {
				registry = queryValues(t, db, `SELECT * FROM harnesses ORDER BY name`)
				reserved = queryValues(t, db, `SELECT * FROM reserved_harness_names ORDER BY name`)
			}
			s := mustOpen(t, path)
			if got := readUserVersion(t, path); got != sqlitelog.SchemaVersion {
				t.Fatalf("migration stamp = %d, want %d", got, sqlitelog.SchemaVersion)
			}
			if !reflect.DeepEqual(queryValues(t, db, oldSessionQuery), metadata) || !reflect.DeepEqual(queryValues(t, db, oldEventQuery), events) {
				t.Fatal("migration changed release metadata/fence/blob/hash bytes")
			}
			if version == 2 && (!reflect.DeepEqual(queryValues(t, db, `SELECT * FROM harnesses ORDER BY name`), registry) || !reflect.DeepEqual(queryValues(t, db, `SELECT * FROM reserved_harness_names ORDER BY name`), reserved)) {
				t.Fatal("migration changed registry reservations/retirement history")
			}
			for _, tc := range []struct {
				uid         string
				head, fence int64
			}{{v012Parent, 5, 2}, {v012Child, 6, 1}} {
				info, err := s.SessionInfo(tc.uid)
				if err != nil {
					t.Fatal(err)
				}
				if info.LastSeq != tc.head || info.ComputeState != api.ComputeCold || info.DeletionState != api.DeletionActive || !info.DeletionRequestedAt.IsZero() || info.DeletionSeq != 0 {
					t.Fatalf("legacy projection/history = %+v", info)
				}
				if err := s.Session(tc.uid).Verify(); err != nil {
					t.Fatal(err)
				}
				deleted, err := s.DeleteSession(context.Background(), tc.uid)
				if err != nil || deleted.LastSeq != tc.head+1 || deleted.DeletionSeq != tc.head+1 || deleted.ComputeState != api.ComputeCold {
					t.Fatalf("legacy delete = %+v,%v", deleted, err)
				}
				var fence int64
				if err := db.QueryRow(`SELECT fence FROM sessions WHERE session = ?`, tc.uid).Scan(&fence); err != nil || fence != tc.fence+1 {
					t.Fatalf("legacy delete fence = %d,%v", fence, err)
				}
				if err := s.Session(tc.uid).Verify(); err != nil {
					t.Fatal(err)
				}
			}
			// Only the two new DELETE records may differ; the eleven release records stay byte exact.
			retained := queryValues(t, db, `SELECT session,seq,prev_hash,hash,fence,event FROM events
				WHERE (session = '`+v012Parent+`' AND seq <= 5) OR (session = '`+v012Child+`' AND seq <= 6) ORDER BY session,seq`)
			if !reflect.DeepEqual(retained, events) {
				t.Fatal("delete rewrote release prefix")
			}
			s.Close()
			mustOpen(t, path)
			if got := readUserVersion(t, path); got <= 2 {
				t.Fatalf("v2 downgrade would accept stamp %d", got)
			}
		})
	}
	if after, err := os.ReadFile(v012Fixture); err != nil || !reflect.DeepEqual(after, original) {
		t.Fatal("original binary fixture changed")
	}
}

func TestDeletionMigrationFailureRollsBackColumnsAndStamp(t *testing.T) {
	path := filepath.Join(t.TempDir(), "broken-v2.db")
	db := rawDB(t, path)
	if _, err := db.Exec(originalV1Create + originalV2Rung + `
		ALTER TABLE sessions ADD COLUMN deletion_completed_at INTEGER NOT NULL DEFAULT 0;
		PRAGMA user_version = 2;`); err != nil {
		t.Fatal(err)
	}
	before := schemaOf(t, path)
	s, err := sqlitelog.Open(path)
	if err == nil {
		s.Close()
		t.Fatal("Open accepted a partially conflicting v2 shape")
	}
	if got := readUserVersion(t, path); got != 2 {
		t.Fatalf("failed migration stamp = %d, want 2", got)
	}
	if got := schemaOf(t, path); !reflect.DeepEqual(got, before) {
		t.Fatalf("failed migration changed shape: %v -> %v", before, got)
	}
}

// deletion_completed_at remains reserved for cleanup in current-schema journals. Reopening them
// needs no migration, and this build neither reads nor writes the column.
func TestDeletionV4ReservedCompletionColumn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v4.db")
	s := mustOpen(t, path)
	for _, uid := range []string{"deleted", "seeded"} {
		if err := s.PutSession(sqlitelog.SessionMeta{UID: uid}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.DeleteSession(context.Background(), "deleted"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	db := rawDB(t, path)
	const reserved = `SELECT session, deletion_completed_at FROM sessions ORDER BY session`
	if got, want := queryValues(t, db, reserved), [][]any{{"deleted", int64(0)}, {"seeded", int64(0)}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("DeleteSession wrote the reserved column: %v", got)
	}
	// A nonzero value stands in for one this build did not write; it must be ignored and kept.
	if _, err := db.Exec(`UPDATE sessions SET deletion_completed_at = 42 WHERE session = 'seeded'`); err != nil {
		t.Fatal(err)
	}
	if got := readUserVersion(t, path); got != sqlitelog.SchemaVersion {
		t.Fatalf("stamp = %d, want %d", got, sqlitelog.SchemaVersion)
	}
	schema := schemaOf(t, path)

	s = mustOpen(t, path)
	if got := readUserVersion(t, path); got != sqlitelog.SchemaVersion {
		t.Fatalf("reopen stamp = %d, want %d", got, sqlitelog.SchemaVersion)
	}
	if got := schemaOf(t, path); !reflect.DeepEqual(got, schema) {
		t.Fatalf("reopen changed the schema: %v != %v", got, schema)
	}
	if info, err := s.SessionInfo("deleted"); err != nil || info.DeletionState != api.DeletionPending || info.DeletionRequestedAt.IsZero() {
		t.Fatalf("reopened deleted session = %+v, %v", info, err)
	}
	seeded, err := s.SessionInfo("seeded")
	if err != nil || seeded.DeletionState != api.DeletionActive || !seeded.DeletionRequestedAt.IsZero() {
		t.Fatalf("reopened seeded session = %+v, %v", seeded, err)
	}
	if deleted, err := s.DeleteSession(context.Background(), "seeded"); err != nil || deleted.DeletionState != api.DeletionPending || deleted.DeletionSeq != seeded.LastSeq+1 {
		t.Fatalf("delete after reopen = %+v, %v", deleted, err)
	}
	if got, want := queryValues(t, db, reserved), [][]any{{"deleted", int64(0)}, {"seeded", int64(42)}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("reserved column after reopen = %v, want %v", got, want)
	}
}
