package sqlitelog_test

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/agent-primitives/sessions/api"
	"github.com/agent-primitives/sessions/sqlitelog"
)

func TestOwnedCreationIsImmutableAndDoesNotInherit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "owners.db")
	s := mustOpen(t, path)
	owner := api.CallerIdentity{Issuer: "issuer", Subject: "owner"}
	meta := sqlitelog.SessionMeta{UID: "parent", Harness: "static", Identity: `{"subject":"agent"}`}
	if err := s.CreateOwnedSession(meta, owner, false); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateOwnedSession(meta, api.CallerIdentity{Issuer: "other", Subject: "owner"}, false); !errors.Is(err, sqlitelog.ErrSessionExists) || !strings.Contains(err.Error(), `"parent"`) {
		t.Fatalf("replace owner must retain UID and sentinel: %v", err)
	}
	meta.Name = "updated"
	put(t, s, meta)
	if err := s.PutSessionOnActiveHarness(meta, false); err != nil {
		t.Fatal(err)
	}
	fence, err := s.Session("parent").NewFence()
	if err != nil {
		t.Fatal(err)
	}
	ev := api.Event{Kind: api.EventLifecycle, Lifecycle: &api.Lifecycle{Kind: api.LifecycleSuspend}}
	if _, err := s.Session("parent").Append(0, fence, ev); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Session("child").NewFence(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Session("child").Append(0, 1, ev); err != nil {
		t.Fatal(err)
	}
	put(t, s, sqlitelog.SessionMeta{UID: "child", ParentUID: "parent", ForkSeq: 1, Identity: meta.Identity})
	if err := s.CreateOwnedSession(sqlitelog.SessionMeta{UID: "child"}, owner, false); !errors.Is(err, sqlitelog.ErrSessionExists) {
		t.Fatalf("claim implicit child: %v", err)
	}
	if _, err := s.DeleteSession(context.Background(), "parent"); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateOwnedSession(meta, owner, false); !errors.Is(err, sqlitelog.ErrSessionExists) {
		t.Fatalf("claim tombstone: %v", err)
	}
	s.Close()
	s = mustOpen(t, path)
	parent, err := s.SessionInfo("parent")
	if err != nil || parent.Owner != owner || parent.Name != "updated" || parent.Identity != meta.Identity {
		t.Fatalf("reopened owner: %+v, %v", parent, err)
	}
	child, err := s.SessionInfo("child")
	if err != nil || child.Owner != (api.CallerIdentity{}) || child.ParentUID != "parent" {
		t.Fatalf("inherited owner: %+v, %v", child, err)
	}
}

func TestOwnedCreationAdmissionIsAtomic(t *testing.T) {
	for _, tc := range []struct {
		name       string
		registered bool
		want       error
	}{
		{"static", false, nil}, {"active", true, nil},
		{"missing", true, sqlitelog.ErrHarnessNotFound},
		{"retired", true, sqlitelog.ErrHarnessRetired},
		{"active", false, sqlitelog.ErrHarnessNameCollision},
	} {
		t.Run(tc.name+map[bool]string{true: " registered", false: " static"}[tc.registered], func(t *testing.T) {
			s := mustOpen(t, ":memory:")
			register(t, s, "active", "a", "d")
			register(t, s, "retired", "r", "d")
			if _, err := s.RetireHarness("retired", "done"); err != nil {
				t.Fatal(err)
			}
			owner := api.CallerIdentity{Issuer: "i", Subject: "s"}
			err := s.CreateOwnedSession(sqlitelog.SessionMeta{UID: "new", Harness: tc.name}, owner, tc.registered)
			if !errors.Is(err, tc.want) {
				t.Fatalf("create: %v, want %v", err, tc.want)
			}
			info, err := s.SessionInfo("new")
			if tc.want != nil {
				if !errors.Is(err, sqlitelog.ErrSessionNotFound) {
					t.Fatalf("failed creation published metadata/owner: %+v, %v", info, err)
				}
			} else if err != nil || info.Owner != owner {
				t.Fatalf("created owner: %+v, %v", info, err)
			}
		})
	}
	for _, owner := range []api.CallerIdentity{{}, {Issuer: "i"}, {Subject: "s"}} {
		s := mustOpen(t, ":memory:")
		if err := s.CreateOwnedSession(sqlitelog.SessionMeta{UID: "invalid"}, owner, false); !errors.Is(err, api.ErrInvalidCallerIdentity) {
			t.Fatalf("invalid owner: %v", err)
		}
		if _, err := s.SessionInfo("invalid"); !errors.Is(err, sqlitelog.ErrSessionNotFound) {
			t.Fatalf("invalid owner published session: %v", err)
		}
	}
}

func TestOwnedCreationStoresComputeNone(t *testing.T) {
	path := filepath.Join(t.TempDir(), "compute.db")
	s := mustOpen(t, path)
	if err := s.CreateOwnedSession(sqlitelog.SessionMeta{UID: "new"}, api.CallerIdentity{Issuer: "i", Subject: "s"}, false); err != nil {
		t.Fatal(err)
	}
	var state string
	if err := rawDB(t, path).QueryRow(`SELECT compute_state FROM sessions WHERE session='new'`).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "NONE" {
		t.Fatalf("stored compute state = %q, want NONE", state)
	}
}

func TestOwnedCreationRollsBackMetadataFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollback.db")
	s := mustOpen(t, path)
	raw := rawDB(t, path)
	if _, err := raw.Exec(`CREATE TRIGGER refuse_metadata BEFORE UPDATE ON sessions
		BEGIN SELECT RAISE(ABORT, 'metadata failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateOwnedSession(sqlitelog.SessionMeta{UID: "failed"}, api.CallerIdentity{Issuer: "i", Subject: "s"}, false); err == nil || errors.Is(err, sqlitelog.ErrSessionExists) {
		t.Fatalf("metadata failure misclassified: %v", err)
	}
	if _, err := s.SessionInfo("failed"); !errors.Is(err, sqlitelog.ErrSessionNotFound) {
		t.Fatalf("failed metadata left an owned row: %v", err)
	}
}

func TestOwnedCreationAcrossStoresHasOneOwner(t *testing.T) {
	path := filepath.Join(t.TempDir(), "shared.db")
	stores := []*sqlitelog.Store{mustOpen(t, path), mustOpen(t, path)}
	results := make(chan error, 2)
	for i, s := range stores {
		go func() {
			results <- s.CreateOwnedSession(sqlitelog.SessionMeta{UID: "shared"}, api.CallerIdentity{Issuer: "i", Subject: string(rune('a' + i))}, false)
		}()
	}
	first, second := <-results, <-results
	if first != nil {
		first, second = second, first
	}
	if first != nil || !errors.Is(second, sqlitelog.ErrSessionExists) {
		t.Fatalf("competing creates: %v / %v", first, second)
	}
}

func TestOwnerListingFiltersBeforePaging(t *testing.T) {
	s := mustOpen(t, ":memory:", sqlitelog.WithDefaultProject("custom"))
	owner := api.CallerIdentity{Issuer: "i", Subject: "s"}
	for _, row := range []struct {
		uid   string
		owner api.CallerIdentity
	}{
		{"0-other", api.CallerIdentity{Issuer: "other", Subject: "s"}},
		{"1-mine", owner}, {"2-unbound", api.CallerIdentity{}}, {"3-mine", owner},
		{"4-other", api.CallerIdentity{Issuer: "i", Subject: "other"}}, {"5-deleted", owner},
	} {
		meta := sqlitelog.SessionMeta{UID: row.uid, CreatedAt: time.Unix(10, 0)}
		if row.owner.Valid() {
			if err := s.CreateOwnedSession(meta, row.owner, false); err != nil {
				t.Fatal(err)
			}
		} else {
			put(t, s, meta)
		}
	}
	if _, err := s.DeleteSession(context.Background(), "5-deleted"); err != nil {
		t.Fatal(err)
	}
	var got []string
	for token := ""; ; {
		page, err := s.ListSessions(sqlitelog.ListOptions{Owner: &owner, PageSize: 1, PageToken: token})
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, uids(page)...)
		token = page.NextPageToken
		if token == "" {
			break
		}
	}
	if !reflect.DeepEqual(got, []string{"1-mine", "3-mine"}) {
		t.Fatalf("owned pages: %v", got)
	}
	page, err := s.ListSessions(sqlitelog.ListOptions{})
	if err != nil || len(page.Sessions) != 5 {
		t.Fatalf("nil filter changed listing: %+v, %v", page, err)
	}
	for _, invalid := range []api.CallerIdentity{{}, {Issuer: "i"}, {Subject: "s"}} {
		if _, err := s.ListSessions(sqlitelog.ListOptions{Owner: &invalid}); !errors.Is(err, api.ErrInvalidCallerIdentity) {
			t.Fatalf("invalid filter: %v", err)
		}
	}
}
