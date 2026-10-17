package sqlitelog

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/agent-primitives/sessions/api"
	"github.com/agent-primitives/sessions/eventlog"
)

func forkParent(t *testing.T, s *Store) {
	t.Helper()
	if err := s.PutSession(SessionMeta{UID: "parent", Project: "tenant"}); err != nil {
		t.Fatal(err)
	}
	approvalAppend(t, s.Session("parent"), 0, 0, api.Event{Kind: api.EventInput, Message: api.TextMessage("user", "retained prefix")})
}

func forkChildren() []SessionMeta {
	return []SessionMeta{
		{UID: "a", Project: "tenant", Name: "first", Harness: "h", Model: "m", ParentUID: "parent", ForkSeq: 1, CreatedAt: time.Unix(100, 0), Labels: `{"k":"v"}`},
		{UID: "b", Project: "tenant", Name: "second", ParentUID: "parent", ForkSeq: 1, CreatedAt: time.Unix(101, 0)},
	}
}

func copyFork(t *testing.T, s *Store, uid string) {
	t.Helper()
	records, err := s.Session("parent").Read(1)
	if err != nil {
		t.Fatal(err)
	}
	approvalAppend(t, s.Session(uid), 0, 0, records[0].Event)
	approvalAppend(t, s.Session(uid), 1, 0, api.Event{Kind: api.EventLifecycle, Lifecycle: &api.Lifecycle{Kind: api.LifecycleFork}})
}

// Hidden rows must not consume list slots or admit runtimes, while the trusted copier can write.
func TestForkPreparingHiddenAndAtomicCommitSurvivesRecovery(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "fork.db")
	s := deletionStore(t, path)
	forkParent(t, s)
	batch, err := s.BeginFork(ctx, "parent", forkChildren())
	if err != nil || batch == "" {
		t.Fatalf("begin=%q %v", batch, err)
	}
	for _, uid := range []string{"a", "b"} {
		if _, err := s.SessionInfo(uid); !errors.Is(err, ErrSessionNotFound) {
			t.Fatalf("PREPARING info=%v", err)
		}
		if err := s.Session(uid).CheckAdmissible(); !errors.Is(err, eventlog.ErrUnpublished) {
			t.Fatalf("PREPARING admission=%v", err)
		}
		if err := s.Session(uid).CheckWritable(); err != nil {
			t.Fatal(err)
		}
		copyFork(t, s, uid)
	}
	page, err := s.ListSessions(ListOptions{PageSize: 1})
	if err != nil || len(page.Sessions) != 1 || page.Sessions[0].UID != "parent" || page.NextPageToken != "" {
		t.Fatalf("hidden paging=%+v %v", page, err)
	}
	// A failed publication leaves every child hidden, never just one.
	approvalExec(t, s.db, `CREATE TRIGGER fail_publish BEFORE UPDATE OF status ON fork_batches WHEN NEW.status='COMMITTED' BEGIN SELECT RAISE(ABORT,'publish failed'); END`)
	if err := s.CommitFork(ctx, batch); err == nil {
		t.Fatal("publication trigger ignored")
	}
	for _, uid := range []string{"a", "b"} {
		if _, err := s.SessionInfo(uid); !errors.Is(err, ErrSessionNotFound) {
			t.Fatalf("partial publication %s=%v", uid, err)
		}
	}
	approvalExec(t, s.db, `DROP TRIGGER fail_publish`)
	if err := s.CommitFork(ctx, batch); err != nil {
		t.Fatal(err)
	}
	if err := s.CommitFork(ctx, batch); err != nil {
		t.Fatal(err)
	}
	before := captureDeletion(t, s, "a")
	s.Close()
	s = deletionStore(t, path)
	if err := s.RecoverForks(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.AbortFork(ctx, batch); !errors.Is(err, ErrForkState) {
		t.Fatalf("abort committed=%v", err)
	}
	assertDeletionUnchanged(t, s, "a", before)
	for _, m := range forkChildren() {
		info := deletionInfo(t, s, m.UID)
		if info.Name != m.Name || info.ParentUID != "parent" || info.ForkSeq != 1 || info.LastSeq != 2 || info.DeletionState != api.DeletionActive || info.ComputeState != api.ComputeCold || info.Labels != m.Labels {
			t.Fatalf("published=%+v", info)
		}
		if err := s.Session(m.UID).CheckAdmissible(); err != nil {
			t.Fatal(err)
		}
		if err := s.Session(m.UID).Verify(); err != nil {
			t.Fatal(err)
		}
		parent, _ := s.Session("parent").Read(1)
		child, _ := s.Session(m.UID).Read(1)
		if child[0].Hash != parent[0].Hash || child[1].PrevHash != parent[0].Hash {
			t.Fatal("committed replay lost prefix evidence")
		}
	}
}

// Restart aborts unfinished batches atomically and preserves attempts plus journals for cleanup.
func TestForkRecoveryAbortsPermanentTombstones(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "recover.db")
	s := deletionStore(t, path)
	forkParent(t, s)
	batch, err := s.BeginFork(ctx, "parent", forkChildren())
	if err != nil {
		t.Fatal(err)
	}
	copyFork(t, s, "a")
	attempt, err := s.Session("a").BeginAllocation(ctx, "route", "fork")
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	s = deletionStore(t, path)
	if err := s.RecoverForks(ctx); err != nil {
		t.Fatal(err)
	}
	for _, uid := range []string{"a", "b"} {
		info := deletionInfo(t, s, uid)
		want := int64(1)
		if uid == "a" {
			want = 3
		}
		if info.DeletionState != api.DeletionPending || info.LastSeq != want || info.DeletionSeq != want {
			t.Fatalf("aborted=%+v", info)
		}
		if err := s.Session(uid).CheckAdmissible(); !errors.Is(err, eventlog.ErrDeleted) {
			t.Fatalf("aborted admission=%v", err)
		}
		if _, err := s.Session(uid).NewFence(); !errors.Is(err, eventlog.ErrDeleted) {
			t.Fatalf("aborted fence=%v", err)
		}
		if err := s.PutSession(SessionMeta{UID: uid}); !errors.Is(err, eventlog.ErrDeleted) {
			t.Fatalf("aborted recreation=%v", err)
		}
		if err := s.Session(uid).Verify(); err != nil {
			t.Fatal(err)
		}
	}
	before := captureDeletion(t, s, "a")
	if err := s.AbortFork(ctx, batch); err != nil {
		t.Fatal(err)
	}
	if err := s.RecoverForks(ctx); err != nil {
		t.Fatal(err)
	}
	assertDeletionUnchanged(t, s, "a", before)
	if err := s.CommitFork(ctx, batch); !errors.Is(err, ErrForkState) {
		t.Fatalf("publish aborted=%v", err)
	}
	if _, err := s.BeginFork(ctx, "parent", forkChildren()); !errors.Is(err, ErrForkState) {
		t.Fatalf("reuse aborted UIDs=%v", err)
	}
	allocs, err := s.Allocations(ctx, "a")
	if err != nil || len(allocs) != 1 || allocs[0].ID != attempt {
		t.Fatalf("abort erased attempt=%+v %v", allocs, err)
	}
}

// A DELETE inside an unfinished batch is still private until the batch abort transaction commits.
func TestForkPreparingTombstoneRemainsHidden(t *testing.T) {
	ctx := context.Background()
	s := deletionStore(t, ":memory:")
	forkParent(t, s)
	batch, err := s.BeginFork(ctx, "parent", forkChildren())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DeleteSession(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SessionInfo("a"); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("PREPARING tombstone leaked: %v", err)
	}
	if err := s.CompleteDeletion(ctx, "a"); !errors.Is(err, ErrCleanupPending) {
		t.Fatalf("unfinished child completion: %v", err)
	}
	if err := s.AbortFork(ctx, batch); err != nil {
		t.Fatal(err)
	}
	info := deletionInfo(t, s, "a")
	if info.DeletionState != api.DeletionPending || info.LastSeq != 1 {
		t.Fatalf("abort duplicated DELETE: %+v", info)
	}
	if err := s.CompleteDeletion(ctx, "a"); err != nil {
		t.Fatal(err)
	}
}

func TestForkBeginValidationAndRollback(t *testing.T) {
	ctx := context.Background()
	s := deletionStore(t, ":memory:")
	forkParent(t, s)
	if _, err := s.BeginFork(ctx, "missing", forkChildren()); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("missing parent=%v", err)
	}
	for _, children := range [][]SessionMeta{nil, {{UID: "parent", ParentUID: "parent"}}, {{UID: "x", ParentUID: "other"}}, {{UID: "x", ParentUID: "parent", ForkSeq: 2}}, {{UID: "x", ParentUID: "parent"}, {UID: "x", ParentUID: "parent"}}} {
		if _, err := s.BeginFork(ctx, "parent", children); !errors.Is(err, ErrForkState) {
			t.Fatalf("bad children %+v=%v", children, err)
		}
	}
	approvalExec(t, s.db, `CREATE TRIGGER fail_child BEFORE INSERT ON sessions WHEN NEW.session='b' BEGIN SELECT RAISE(ABORT,'second child failed'); END`)
	if _, err := s.BeginFork(ctx, "parent", forkChildren()); err == nil {
		t.Fatal("metadata trigger ignored")
	}
	var count int
	if err := s.db.QueryRow(`SELECT (SELECT COUNT(*) FROM fork_batches)+(SELECT COUNT(*) FROM sessions WHERE session IN ('a','b'))`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("begin leaked batch/child=%d %v", count, err)
	}
	approvalExec(t, s.db, `DROP TRIGGER fail_child`)
	batch, err := s.BeginFork(ctx, "parent", forkChildren())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.BeginFork(ctx, "a", []SessionMeta{{UID: "nested", ParentUID: "a"}}); !errors.Is(err, eventlog.ErrUnpublished) {
		t.Fatalf("hidden parent=%v", err)
	}
	if err := s.CommitFork(ctx, batch); !errors.Is(err, ErrForkState) {
		t.Fatalf("empty logs published=%v", err)
	}
	copyFork(t, s, "a")
	if err := s.CommitFork(ctx, batch); !errors.Is(err, ErrForkState) {
		t.Fatalf("partial logs published=%v", err)
	}
	copyFork(t, s, "b")
	approvalExec(t, s.db, `UPDATE sessions SET parent_uid='wrong' WHERE session='b'`)
	if err := s.CommitFork(ctx, batch); !errors.Is(err, ErrForkState) {
		t.Fatalf("invalid metadata published=%v", err)
	}
	approvalExec(t, s.db, `UPDATE sessions SET parent_uid='parent' WHERE session='b'`)
	if _, err := s.DeleteSession(ctx, "parent"); err != nil {
		t.Fatal(err)
	}
	if err := s.CompleteDeletion(ctx, "parent"); !errors.Is(err, ErrCleanupPending) {
		t.Fatalf("parent complete with unfinished batch=%v", err)
	}
	if err := s.CommitFork(ctx, batch); !errors.Is(err, eventlog.ErrDeleted) {
		t.Fatalf("deleted parent publication=%v", err)
	}
	if err := s.AbortFork(ctx, batch); err != nil {
		t.Fatal(err)
	}
	if err := s.CompleteDeletion(ctx, "parent"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.BeginFork(ctx, "parent", []SessionMeta{{UID: "new", ParentUID: "parent"}}); !errors.Is(err, eventlog.ErrDeleted) {
		t.Fatalf("deleted parent begin=%v", err)
	}
}

func TestForkAbortRollbackAndUnknownStatesFailClosed(t *testing.T) {
	ctx := context.Background()
	s := deletionStore(t, ":memory:")
	forkParent(t, s)
	batch, err := s.BeginFork(ctx, "parent", forkChildren())
	if err != nil {
		t.Fatal(err)
	}
	copyFork(t, s, "a")
	copyFork(t, s, "b")
	approvalExec(t, s.db, `CREATE TRIGGER fail_abort BEFORE UPDATE OF deletion_state ON sessions WHEN NEW.session='b' BEGIN SELECT RAISE(ABORT,'abort failed'); END`)
	if err := s.AbortFork(ctx, batch); err == nil {
		t.Fatal("abort trigger ignored")
	}
	var status string
	if err := s.db.QueryRow(`SELECT status FROM fork_batches WHERE id=?`, batch).Scan(&status); err != nil || status != "PREPARING" {
		t.Fatalf("partial batch abort=%s %v", status, err)
	}
	for _, uid := range []string{"a", "b"} {
		head, _ := s.Session(uid).Head()
		if head != 2 {
			t.Fatalf("partial DELETE for %s=%d", uid, head)
		}
		if err := s.Session(uid).Verify(); err != nil {
			t.Fatal(err)
		}
	}
	approvalExec(t, s.db, `DROP TRIGGER fail_abort`)
	approvalExec(t, s.db, `UPDATE sessions SET deletion_state='FUTURE' WHERE session='b'`)
	if err := s.AbortFork(ctx, batch); !errors.Is(err, eventlog.ErrDeleted) {
		t.Fatalf("unknown child state=%v", err)
	}
	head, _ := s.Session("a").Head()
	if head != 2 {
		t.Fatal("unknown state partially aborted sibling")
	}
	approvalExec(t, s.db, `UPDATE sessions SET deletion_state='ACTIVE' WHERE session='b'`)
	approvalExec(t, s.db, `UPDATE fork_batches SET status='FUTURE' WHERE id=?`, batch)
	for _, err := range []error{s.AbortFork(ctx, batch), s.CommitFork(ctx, batch)} {
		if !errors.Is(err, ErrForkState) {
			t.Fatalf("unknown batch=%v", err)
		}
	}
	if err := s.Session("a").CheckAdmissible(); !errors.Is(err, eventlog.ErrUnpublished) {
		t.Fatalf("unknown batch admitted=%v", err)
	}
	approvalExec(t, s.db, `UPDATE fork_batches SET status='PREPARING' WHERE id=?`, batch)
	if err := s.AbortFork(ctx, batch); err != nil {
		t.Fatal(err)
	}
	before := captureDeletion(t, s, "a")
	snap, _ := s.Session("a").Read(1)
	if len(snap) != 3 || snap[2].Event.Lifecycle.Kind != api.LifecycleDelete {
		t.Fatalf("abort journal=%+v", snap)
	}
	if !reflect.DeepEqual(before.Info.PendingApproval, api.ApprovalRef{}) {
		t.Fatal("abort invented approval")
	}
}
