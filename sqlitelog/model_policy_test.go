package sqlitelog_test

import (
	"path/filepath"
	"reflect"
	"testing"

	"github.com/agent-primitives/sessions/api"
	"github.com/agent-primitives/sessions/sqlitelog"
)

func TestSessionModelPolicyPersistsAcrossUpsertsAndReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.db")
	s := mustOpen(t, path)
	// Fork creates the child row by appending events before it attaches inherited metadata.
	log := s.Session("child")
	fence, err := log.NewFence()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := log.Append(0, fence, api.Event{Kind: api.EventLifecycle, Lifecycle: &api.Lifecycle{Kind: api.LifecycleFork}}); err != nil {
		t.Fatal(err)
	}
	for _, uid := range []string{"new", "child"} {
		put(t, s, sqlitelog.SessionMeta{UID: uid, ModelPolicy: true})
		put(t, s, sqlitelog.SessionMeta{UID: uid, Name: "ordinary metadata update"})
	}
	put(t, s, sqlitelog.SessionMeta{UID: "legacy"})
	s.Close()
	s = mustOpen(t, path)
	page, err := s.ListSessions(sqlitelog.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Sessions) != 3 {
		t.Fatalf("sessions=%d, want 3", len(page.Sessions))
	}
	for _, info := range page.Sessions {
		want := info.UID != "legacy"
		got, err := s.SessionInfo(info.UID)
		if err != nil {
			t.Fatal(err)
		}
		if info.ModelPolicy != want || got.ModelPolicy != want {
			t.Fatalf("%s eligibility: list=%t get=%t want=%t", info.UID, info.ModelPolicy, got.ModelPolicy, want)
		}
	}
}

// The genuine pre-policy fixtures include sessions with no events. Migration must not infer
// eligibility from a model string or from an execution, nor rewrite any existing journal bytes.
func TestModelPolicyMigrationDefaultsLegacySessionsOff(t *testing.T) {
	for _, fixture := range []string{"testdata/v0.1.2.db", approvalV3Fixture, "testdata/ownership-v4.db"} {
		t.Run(filepath.Base(fixture), func(t *testing.T) {
			path := copyFixture(t, fixture)
			raw := rawDB(t, path)
			if _, err := raw.Exec(`INSERT INTO sessions(session) VALUES('legacy-empty-policy-check')`); err != nil {
				t.Fatal(err)
			}
			before := queryValues(t, raw, `SELECT * FROM events ORDER BY session,seq`)
			s := mustOpen(t, path)
			var optedIn int
			if err := raw.QueryRow(`SELECT COUNT(*) FROM sessions WHERE model_policy != 0`).Scan(&optedIn); err != nil {
				t.Fatal(err)
			}
			var emptyFlags, emptyEvents int
			if err := raw.QueryRow(`SELECT model_policy, (SELECT COUNT(*) FROM events WHERE session='legacy-empty-policy-check') FROM sessions WHERE session='legacy-empty-policy-check'`).Scan(&emptyFlags, &emptyEvents); err != nil {
				t.Fatal(err)
			}
			if emptyFlags != 0 || emptyEvents != 0 {
				t.Fatalf("pre-v6 empty session changed: flags=%d events=%d", emptyFlags, emptyEvents)
			}
			if optedIn != 0 || !reflect.DeepEqual(before, queryValues(t, raw, `SELECT * FROM events ORDER BY session,seq`)) {
				t.Fatal("migration opted legacy sessions in or changed events")
			}
			if readUserVersion(t, path) != sqlitelog.SchemaVersion {
				t.Fatal("migration did not reach current schema")
			}
			s.Close()
		})
	}
}
