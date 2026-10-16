package session_test

import (
	"path/filepath"
	"reflect"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	v1 "github.com/agent-primitives/sessions/api/genpb"
)

// A cascade, inherited-handle teardown, metadata erasure, or prefix rewrite would break these
// real Service families. Each deletion is independent; survivors can Resume, Exec and fork
// their own source after a parent, child, or intermediate ancestor becomes a tombstone.
func TestDeleteHierarchyDoesNotCascadeOrRewriteAudit(t *testing.T) {
	for _, victimName := range []string{"parent", "child", "intermediate"} {
		t.Run(victimName, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "journal.db")
			store := openStore(t, path)
			db := deletionSQL(t, path)
			registry, backend := deletionRegistry(t)
			client := serveRegistry(t, store, registry)
			parent := mustCreate(t, client)
			if err := deletionExec(t, client, parent, ""); err != nil {
				t.Fatal(err)
			}
			// Retain a real SUSPEND handle in every copied descendant prefix. It is evidence,
			// not a resource that Delete may infer belongs to the descendant.
			if _, err := client.Suspend(t.Context(), &v1.SuspendRequest{Session: parent}); err != nil {
				t.Fatal(err)
			}
			fork := func(uid string, count int32) []*v1.Session {
				t.Helper()
				response, err := client.Fork(t.Context(), &v1.ForkRequest{Session: uid, Count: count})
				if err != nil {
					t.Fatal(err)
				}
				if len(response.GetChildren()) != int(count) {
					t.Fatalf("fork children=%d, want %d", len(response.GetChildren()), count)
				}
				return response.GetChildren()
			}
			children := fork(parent, 2)
			child := children[0].GetMetadata().GetUid()
			sibling := children[1].GetMetadata().GetUid()
			intermediate := fork(child, 1)[0].GetMetadata().GetUid()
			leaf := fork(intermediate, 1)[0].GetMetadata().GetUid()
			family := map[string]string{"parent": parent, "child": child, "sibling": sibling, "intermediate": intermediate, "leaf": leaf}
			prefixes := map[string][]*v1.LogRecord{}
			metadata := map[string]*v1.Session{}
			for _, uid := range family {
				prefixes[uid] = deletionReplay(t, client, uid)
				info, err := client.GetSession(t.Context(), &v1.GetSessionRequest{Uid: uid})
				if err != nil {
					t.Fatal(err)
				}
				metadata[uid] = info
			}
			// Compare SQL blobs and hashes for every original event, not only decoded messages.
			before := sessionDeleteRaw(t, db)
			victim := family[victimName]
			backend.reset()
			deleted, err := client.DeleteSession(t.Context(), &v1.DeleteSessionRequest{Uid: victim})
			if err != nil || deleted.GetDeletionState() != v1.DeletionState_DELETION_PENDING {
				t.Fatalf("Delete=%v %v", deleted, err)
			}
			if len(backend.effects()) != 0 {
				t.Fatalf("Delete contacted shared backend: %v", backend.effects())
			}
			after := sessionDeleteRaw(t, db)
			checkedEvents := 0
			for _, row := range before {
				// Events have six columns (session/seq/prev_hash/hash/fence/blob), unlike
				// the retained metadata row. Compare the complete raw record.
				if len(row) != 6 {
					continue
				}
				checkedEvents++
				found := false
				for _, got := range after {
					if reflect.DeepEqual(row, got) {
						found = true
						break
					}
				}
				if !found {
					t.Error("Delete changed an original raw event blob/hash")
				}
			}
			if checkedEvents == 0 {
				t.Fatal("raw audit assertion checked no events")
			}
			for name, uid := range family {
				got, err := client.GetSession(t.Context(), &v1.GetSessionRequest{Uid: uid})
				if err != nil {
					t.Fatal(err)
				}
				wantLength := len(prefixes[uid])
				if uid == victim {
					wantLength++
					if got.GetComputeState() != metadata[uid].GetComputeState() || got.GetParentUid() != metadata[uid].GetParentUid() || got.GetForkSeq() != metadata[uid].GetForkSeq() {
						t.Errorf("victim lost independent compute/lineage: %v", got)
					}
				} else if !proto.Equal(metadata[uid], got) {
					t.Errorf("%s cascaded or changed metadata: before=%v after=%v", name, metadata[uid], got)
				}
				records := deletionReplay(t, client, uid)
				if len(records) != wantLength {
					t.Fatalf("%s audit length=%d want %d", name, len(records), wantLength)
				}
				for i, original := range prefixes[uid] {
					if !proto.Equal(original, records[i]) {
						t.Errorf("%s copied prefix changed at seq %d", name, original.GetSeq())
					}
				}
				if uid == victim && records[len(records)-1].GetEvent().GetLifecycle().GetKind() != v1.Lifecycle_LIFECYCLE_DELETE {
					t.Error("victim missing DELETE audit")
				}
				if err := store.Session(uid).Verify(); err != nil {
					t.Fatal(err)
				}
			}
			page, err := client.ListSessions(t.Context(), &v1.ListSessionsRequest{})
			if err != nil || len(page.GetSessions()) != len(family)-1 {
				t.Fatalf("List cascade=%v %v", page, err)
			}
			if _, err := client.Fork(t.Context(), &v1.ForkRequest{Session: victim, AtSeq: 1}); status.Code(err) != codes.FailedPrecondition {
				t.Errorf("historical victim Fork=%v", err)
			}
			for name, uid := range family {
				if uid == victim {
					continue
				}
				if _, err := client.Resume(t.Context(), &v1.ResumeRequest{Session: uid}); err != nil {
					t.Fatalf("surviving %s Resume=%v", name, err)
				}
				if err := deletionExec(t, client, uid, ""); err != nil {
					t.Fatalf("surviving %s Exec=%v", name, err)
				}
				newChild := fork(uid, 1)[0]
				if newChild.GetParentUid() != uid || newChild.GetDeletionState() != v1.DeletionState_DELETION_ACTIVE {
					t.Errorf("survivor's own fork=%v", newChild)
				}
				if err := store.Session(newChild.GetMetadata().GetUid()).Verify(); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

// Local incarnations share one real Harness.Connect server. Delete must not close it or stop
// another UID; the second session keeps its existing address and can run after the tombstone.
func TestDeleteLeavesOtherUIDOnSharedLocalHarnessUsable(t *testing.T) {
	store := openStore(t, ":memory:")
	registry, backend := deletionRegistry(t)
	client := serveRegistry(t, store, registry)
	first, second := mustCreate(t, client), mustCreate(t, client)
	for _, uid := range []string{first, second} {
		if err := deletionExec(t, client, uid, ""); err != nil {
			t.Fatal(err)
		}
	}
	prefix := deletionReplay(t, client, second)
	backend.reset()
	if _, err := client.DeleteSession(t.Context(), &v1.DeleteSessionRequest{Uid: first}); err != nil {
		t.Fatal(err)
	}
	if len(backend.effects()) != 0 {
		t.Fatal("Delete contacted shared local runtime")
	}
	if _, err := client.Suspend(t.Context(), &v1.SuspendRequest{Session: second}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Resume(t.Context(), &v1.ResumeRequest{Session: second}); err != nil {
		t.Fatal(err)
	}
	if err := deletionExec(t, client, second, ""); err != nil {
		t.Fatal(err)
	}
	if backend.effects()["Stop"] != 0 {
		t.Error("shared harness stopped")
	}
	got := deletionReplay(t, client, second)
	for i, r := range prefix {
		if !proto.Equal(r, got[i]) {
			t.Error("other UID journal prefix changed")
		}
	}
	if err := store.Session(second).Verify(); err != nil {
		t.Fatal(err)
	}
}
