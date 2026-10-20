package sqlitelog_test

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/agent-primitives/sessions/sqlitelog"
)

func TestSessionModelPolicyIndependentFlags(t *testing.T) {
	path := filepath.Join(t.TempDir(), "flags.db")
	store := mustOpen(t, path)
	raw := rawDB(t, path)
	for flags := 0; flags < 4; flags++ {
		uid := fmt.Sprintf("bits%d", flags)
		put(t, store, sqlitelog.SessionMeta{UID: uid})
		if _, err := raw.Exec(`UPDATE sessions SET model_policy=? WHERE session=?`, flags, uid); err != nil {
			t.Fatal(err)
		}
		put(t, store, sqlitelog.SessionMeta{UID: uid, Name: "ordinary update"})
	}
	put(t, store, sqlitelog.SessionMeta{UID: "combined", ModelPolicy: true})
	put(t, store, sqlitelog.SessionMeta{UID: "combined", ModelPolicyEligible: true})
	put(t, store, sqlitelog.SessionMeta{UID: "combined"})
	var combined int
	if err := raw.QueryRow(`SELECT model_policy FROM sessions WHERE session='combined'`).Scan(&combined); err != nil || combined != 3 {
		t.Fatalf("independent sticky flags=%d,%v,want3", combined, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store = mustOpen(t, path)
	page, err := store.ListSessions(sqlitelog.ListOptions{})
	if err != nil || len(page.Sessions) != 5 {
		t.Fatalf("listed flags: %+v,%v", page, err)
	}
	for _, listed := range page.Sessions {
		want := 3
		if listed.UID != "combined" {
			if _, err := fmt.Sscanf(listed.UID, "bits%d", &want); err != nil {
				t.Fatal(err)
			}
		}
		got, err := store.SessionInfo(listed.UID)
		if err != nil {
			t.Fatal(err)
		}
		for _, info := range []sqlitelog.SessionInfo{listed, got} {
			if info.ModelPolicy != (want&1 != 0) || info.ModelPolicyEligible != (want != 0) {
				t.Fatalf("flags%d decoded wrong: %+v", want, info)
			}
		}
	}
}

func TestSessionModelPolicyUnknownFlagsRefuseReads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "flags.db")
	store := mustOpen(t, path)
	put(t, store, sqlitelog.SessionMeta{UID: "future"})
	raw := rawDB(t, path)
	for _, flags := range []int{-1, 4} {
		if _, err := raw.Exec(`UPDATE sessions SET model_policy=? WHERE session='future'`, flags); err != nil {
			t.Fatal(err)
		}
		if _, err := store.SessionInfo("future"); err == nil {
			t.Fatal("unknown admission flags read as legacy")
		}
		if _, err := store.ListSessions(sqlitelog.ListOptions{}); err == nil {
			t.Fatal("unknown admission flags accepted by listing")
		}
	}
}
