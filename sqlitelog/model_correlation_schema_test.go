package sqlitelog_test

import (
	"database/sql"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/agent-primitives/sessions/api"
	"github.com/agent-primitives/sessions/sqlitelog"
)

func TestV10ToV11MigrationIsStampOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v10.db")
	s := mustOpen(t, path)
	log := s.Session("legacy")
	fence, err := log.NewFence()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := log.Append(0, fence, api.Event{ExecutionID: "e", Kind: api.EventModelCall,
		ModelCall: &api.ModelCall{ID: "legacy", InputHash: "hash"}}); err != nil {
		t.Fatal(err)
	}
	s.Close()
	setUserVersion(t, path, 10)
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	beforeSchema := queryValues(t, raw, `SELECT * FROM sqlite_schema ORDER BY name`)
	beforeEvents := queryValues(t, raw, `SELECT * FROM events ORDER BY session,seq`)
	raw.Close()

	s, err = sqlitelog.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	raw, err = sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if got := readUserVersion(t, path); got != sqlitelog.SchemaVersion {
		t.Fatalf("user_version=%d, want %d", got, sqlitelog.SchemaVersion)
	}
	if after := queryValues(t, raw, `SELECT * FROM sqlite_schema ORDER BY name`); !reflect.DeepEqual(after, beforeSchema) {
		t.Fatalf("stamp-only migration changed schema:\nbefore=%v\nafter=%v", beforeSchema, after)
	}
	if after := queryValues(t, raw, `SELECT * FROM events ORDER BY session,seq`); !reflect.DeepEqual(after, beforeEvents) {
		t.Fatalf("stamp-only migration changed events:\nbefore=%v\nafter=%v", beforeEvents, after)
	}
}

func TestJournalStampIsNewerThanVersion10(t *testing.T) {
	if sqlitelog.SchemaVersion <= 10 {
		t.Fatalf("schema version=%d; a v10 build would open ID-correlated model results", sqlitelog.SchemaVersion)
	}
}
