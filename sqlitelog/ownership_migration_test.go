package sqlitelog_test

import (
	"errors"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/agent-primitives/sessions/sqlitelog"
)

func TestOwnerSchemaDefaultsToUnboundAndOff(t *testing.T) {
	path := filepath.Join(t.TempDir(), "owners.db")
	s := mustOpen(t, path)
	put(t, s, sqlitelog.SessionMeta{UID: "legacy"})
	raw := rawDB(t, path)
	var issuer, subject, mode string
	if err := raw.QueryRow(`SELECT owner_issuer, owner_subject FROM sessions WHERE session='legacy'`).Scan(&issuer, &subject); err != nil {
		t.Fatal(err)
	}
	if err := raw.QueryRow(`SELECT mode FROM journal_authorization WHERE singleton=1`).Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if issuer != "" || subject != "" || mode != "off" {
		t.Fatalf("new journal: owner=(%q,%q), mode=%q", issuer, subject, mode)
	}
}

// ownership-v4.db was created through the public store API at #123's 9bdd477,
// before the owner migration existed. SHA256: 9205653a4eecf595cffcdbacbb807ee8a33fc627c48db07d46acd5b3a984b9a8.
func TestOwnerMigrationPreservesGenuineJournals(t *testing.T) {
	for _, fixture := range []string{"testdata/v0.1.2.db", approvalV3Fixture, "testdata/ownership-v4.db"} {
		t.Run(filepath.Base(fixture), func(t *testing.T) {
			path := copyFixture(t, fixture)
			raw := rawDB(t, path)
			before := queryValues(t, raw, `SELECT * FROM events ORDER BY session,seq`)
			s := mustOpen(t, path)
			if readUserVersion(t, path) != sqlitelog.SchemaVersion || !reflect.DeepEqual(before, queryValues(t, raw, `SELECT * FROM events ORDER BY session,seq`)) {
				t.Fatal("migration changed events or failed to reach current schema")
			}
			for _, row := range queryValues(t, raw, `SELECT session FROM sessions`) {
				uid := row[0].(string)
				var issuer, subject string
				if err := raw.QueryRow(`SELECT owner_issuer,owner_subject FROM sessions WHERE session=?`, uid).Scan(&issuer, &subject); err != nil || issuer != "" || subject != "" {
					t.Fatalf("legacy session acquired owner: (%q,%q), %v", issuer, subject, err)
				}
				if err := s.Session(uid).Verify(); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestOwnerMigrationRollsBack(t *testing.T) {
	path := copyFixture(t, "testdata/ownership-v4.db")
	raw := rawDB(t, path)
	if _, err := raw.Exec(`CREATE TABLE journal_authorization (conflict TEXT)`); err != nil {
		t.Fatal(err)
	}
	before := schemaOf(t, path)
	if s, err := sqlitelog.Open(path); err == nil {
		s.Close()
		t.Fatal("conflicting v5 rung accepted")
	}
	if readUserVersion(t, path) != 4 || !reflect.DeepEqual(before, schemaOf(t, path)) {
		t.Fatal("failed v5 rung changed stamp/schema")
	}
}

func TestOwnerJournalRefusesEnforcementAtSameSchema(t *testing.T) {
	for _, change := range []string{
		`UPDATE journal_authorization SET mode='required'`,
		`UPDATE journal_authorization SET mode='future'`,
		`DELETE FROM journal_authorization`,
	} {
		t.Run(change, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "journal.db")
			s := mustOpen(t, path)
			put(t, s, sqlitelog.SessionMeta{UID: "untouched"})
			s.Close()
			raw := rawDB(t, path)
			if _, err := raw.Exec(change); err != nil {
				t.Fatal(err)
			}
			before := queryValues(t, raw, `SELECT * FROM sessions`)
			if s, err := sqlitelog.Open(path); !errors.Is(err, sqlitelog.ErrUnsupportedAuthorization) {
				if s != nil {
					s.Close()
				}
				t.Fatalf("Open = %v, want ErrUnsupportedAuthorization", err)
			}
			if readUserVersion(t, path) != sqlitelog.SchemaVersion || !reflect.DeepEqual(before, queryValues(t, raw, `SELECT * FROM sessions`)) {
				t.Fatal("refused journal was mutated")
			}
		})
	}
}
