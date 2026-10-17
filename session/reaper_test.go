package session_test

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/agent-primitives/sessions/api"
	v1 "github.com/agent-primitives/sessions/api/genpb"
	"github.com/agent-primitives/sessions/harness/echoagent"
	"github.com/agent-primitives/sessions/placement"
	"github.com/agent-primitives/sessions/runtime/local"
	"github.com/agent-primitives/sessions/session"
	"github.com/agent-primitives/sessions/sqlitelog"
)

func TestReaperCompletesLocalWithoutChangingAudit(t *testing.T) {
	store := openStore(t, ":memory:")
	registry, _ := deletionRegistry(t)
	client := serveRegistry(t, store, registry)
	svc := session.NewService(store, registry)
	uid := mustCreate(t, client)
	if err := deletionExec(t, client, uid, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Suspend(t.Context(), &v1.SuspendRequest{Session: uid}); err != nil {
		t.Fatal(err)
	}
	pending, err := client.DeleteSession(t.Context(), &v1.DeleteSessionRequest{Uid: uid})
	if err != nil {
		t.Fatal(err)
	}
	audit := deletionReplay(t, client, uid)
	if err := svc.ReapOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	complete, err := client.GetSession(t.Context(), &v1.GetSessionRequest{Uid: uid})
	if err != nil {
		t.Fatal(err)
	}
	if complete.GetDeletionState() != v1.DeletionState_DELETION_COMPLETE || complete.GetDeletionCompletedTime() == nil || complete.GetComputeState() != v1.ComputeState_COMPUTE_TERMINATED {
		t.Fatalf("cleanup not completed: %v", complete)
	}
	if complete.LastSeq != pending.LastSeq || complete.ExecState != pending.ExecState || !proto.Equal(complete.Metadata, pending.Metadata) || !proto.Equal(complete.DeletionRequestedTime, pending.DeletionRequestedTime) {
		t.Fatalf("cleanup changed audit projection: pending=%v complete=%v", pending, complete)
	}
	if got := deletionReplay(t, client, uid); len(got) != len(audit) {
		t.Fatal("cleanup appended audit event")
	} else {
		for i := range audit {
			if !proto.Equal(audit[i], got[i]) {
				t.Fatal("cleanup changed audit record")
			}
		}
	}
	if err := svc.ReapOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	retry, err := client.DeleteSession(t.Context(), &v1.DeleteSessionRequest{Uid: uid})
	if err != nil || !proto.Equal(retry, complete) {
		t.Fatalf("complete retry=%v, %v", retry, err)
	}
	if err := deletionExec(t, client, uid, ""); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("completed session admitted: %v", err)
	}
	other := mustCreate(t, client)
	if err := deletionExec(t, client, other, ""); err != nil {
		t.Fatalf("cleanup stopped shared harness: %v", err)
	}
}

// Embedding the Runtime interface deliberately does not opt this backend into safe release.
type unsupportedCleanup struct {
	api.Runtime
	describe func(context.Context) (api.Descriptor, error)
}

func (b unsupportedCleanup) Describe(ctx context.Context) (api.Descriptor, error) {
	return b.describe(ctx)
}

func TestReaperLeavesUnsupportedAllocationsPending(t *testing.T) {
	store := openStore(t, ":memory:")
	inner := local.New(echoagent.Harness{})
	t.Cleanup(func() { _ = inner.Close() })
	registry := echoRegistry(t, unsupportedCleanup{Runtime: inner, describe: inner.Describe})
	client := serveRegistry(t, store, registry)
	svc := session.NewService(store, registry)
	uid := mustCreate(t, client)
	if err := deletionExec(t, client, uid, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := client.DeleteSession(t.Context(), &v1.DeleteSessionRequest{Uid: uid}); err != nil {
		t.Fatal(err)
	}
	if err := svc.ReapOnce(t.Context()); !errors.Is(err, placement.ErrCleanupUnavailable) {
		t.Fatalf("unsupported cleanup=%v", err)
	}
	info, err := store.SessionInfo(uid)
	if err != nil {
		t.Fatal(err)
	}
	if info.DeletionState != api.DeletionPending || info.CleanupError == "" || !info.DeletionCompletedAt.IsZero() {
		t.Fatalf("unsupported allocation lost: %+v", info)
	}
}

var errRelease = errors.New("release temporarily unavailable")

type retryCleanup struct {
	*local.Backend
	mu      sync.Mutex
	fail    bool
	calls   int
	key     string
	entered chan struct{}
	release chan struct{}
}

func (b *retryCleanup) CleanupKey() string {
	if b.key != "" {
		return b.key
	}
	return "retry-no-resource"
}
func (b *retryCleanup) ReleaseSession(ctx context.Context, uid string) error {
	b.mu.Lock()
	b.calls++
	fail := b.fail
	b.mu.Unlock()
	if b.entered != nil {
		select {
		case b.entered <- struct{}{}:
		default:
		}
	}
	if b.release != nil {
		select {
		case <-b.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if fail {
		return errRelease
	}
	return b.Stop(ctx, api.Incarnation{ID: uid})
}

func TestReaperRetriesPartialProgressAcrossServiceRestart(t *testing.T) {
	store := openStore(t, ":memory:")
	b := &retryCleanup{Backend: local.New(echoagent.Harness{}), fail: true}
	t.Cleanup(func() { _ = b.Close() })
	registry := echoRegistry(t, b)
	client := serveRegistry(t, store, registry)
	uid := mustCreate(t, client)
	if err := deletionExec(t, client, uid, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := client.DeleteSession(t.Context(), &v1.DeleteSessionRequest{Uid: uid}); err != nil {
		t.Fatal(err)
	}
	if err := session.NewService(store, registry).ReapOnce(t.Context()); !errors.Is(err, errRelease) {
		t.Fatalf("first reap=%v", err)
	}
	info, err := store.SessionInfo(uid)
	if err != nil {
		t.Fatal(err)
	}
	if info.DeletionState != api.DeletionPending || info.CleanupError == "" {
		t.Fatalf("failure not retained: %+v", info)
	}
	b.mu.Lock()
	b.fail = false
	b.mu.Unlock()
	if err := session.NewService(store, registry).ReapOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	info, err = store.SessionInfo(uid)
	if err != nil {
		t.Fatal(err)
	}
	if info.DeletionState != api.DeletionComplete || info.CleanupError != "" {
		t.Fatalf("retry did not complete: %+v", info)
	}
	b.mu.Lock()
	calls := b.calls
	b.mu.Unlock()
	if calls != 2 {
		t.Fatalf("release calls=%d, want 2", calls)
	}
}

func TestReaperLegacyInventoryRequiresExplicitCertification(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	store := openStore(t, path)
	registry, _ := deletionRegistry(t)
	client := serveRegistry(t, store, registry)
	uid := mustCreate(t, client)
	// Set the same incomplete marker assigned by migration; migration fixtures live in sqlitelog.
	if _, err := deletionSQL(t, path).Exec(`UPDATE sessions SET inventory_complete=0 WHERE session=?`, uid); err != nil {
		t.Fatal(err)
	}
	if _, err := client.DeleteSession(t.Context(), &v1.DeleteSessionRequest{Uid: uid}); err != nil {
		t.Fatal(err)
	}
	svc := session.NewService(store, registry)
	if err := svc.ReapOnce(t.Context()); !errors.Is(err, sqlitelog.ErrCleanupPending) {
		t.Fatalf("legacy reap=%v", err)
	}
	info, err := store.SessionInfo(uid)
	if err != nil {
		t.Fatal(err)
	}
	if info.DeletionState != api.DeletionPending || info.InventoryComplete {
		t.Fatalf("empty legacy ledger completed: %+v", info)
	}
	if err := store.CertifyInventory(t.Context(), uid, ""); err == nil {
		t.Fatal("empty evidence certified inventory")
	}
	if err := store.CertifyInventory(t.Context(), uid, "operator checked all former backend namespaces and failed allocations"); err != nil {
		t.Fatal(err)
	}
	if err := svc.ReapOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	info, err = store.SessionInfo(uid)
	if err != nil {
		t.Fatal(err)
	}
	if info.DeletionState != api.DeletionComplete {
		t.Fatalf("certified legacy ledger not completed: %+v", info)
	}
}

func TestReaperStartRecoversBeforeReturningAndStopJoins(t *testing.T) {
	store := openStore(t, ":memory:")
	b := &retryCleanup{Backend: local.New(echoagent.Harness{}), entered: make(chan struct{}, 1), release: make(chan struct{})}
	t.Cleanup(func() { _ = b.Close() })
	registry := echoRegistry(t, b)
	svc := session.NewService(store, registry)
	if err := store.PutSession(sqlitelog.SessionMeta{UID: "parent"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.BeginFork(t.Context(), "parent", []sqlitelog.SessionMeta{{UID: "unfinished", ParentUID: "parent"}}); err != nil {
		t.Fatal(err)
	}
	attempt, err := store.Session("unfinished").BeginAllocation(t.Context(), b.CleanupKey(), "fork")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Session("unfinished").ConfirmAllocation(t.Context(), attempt, api.Incarnation{ID: "unfinished"}); err != nil {
		t.Fatal(err)
	}
	stop, err := svc.StartReaper(t.Context(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stop)
	info, err := store.SessionInfo("unfinished")
	if err != nil {
		t.Fatal(err)
	}
	if info.DeletionState != api.DeletionPending {
		t.Fatalf("startup returned before recovery: %+v", info)
	}
	select {
	case <-b.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("initial cleanup not attempted")
	}
	done := make(chan struct{})
	go func() { stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("stop did not cancel/join cleanup")
	}
	stop() // joining twice is safe
	if _, err := svc.StartReaper(t.Context(), 0); err == nil {
		t.Fatal("zero interval accepted")
	}
}

func TestReplayHidesPreparingChildrenButKeepsAbortedAudit(t *testing.T) {
	store := openStore(t, ":memory:")
	registry, _ := deletionRegistry(t)
	client := serveRegistry(t, store, registry)
	parent := mustCreate(t, client)
	batch, err := store.BeginFork(t.Context(), parent, []sqlitelog.SessionMeta{{UID: "preparing", ParentUID: parent}})
	if err != nil {
		t.Fatal(err)
	}
	stream, err := client.Replay(t.Context(), &v1.ReplayRequest{Session: "preparing"})
	if err == nil {
		_, err = stream.Recv()
	}
	if status.Code(err) != codes.NotFound {
		t.Fatalf("PREPARING replay=%v, want NotFound", err)
	}
	if _, err := client.DeleteSession(t.Context(), &v1.DeleteSessionRequest{Uid: "preparing"}); status.Code(err) != codes.NotFound {
		t.Fatalf("PREPARING public Delete=%v, want NotFound", err)
	}
	// Even a trusted store-side tombstone must not publish a still-PREPARING journal.
	if _, err := store.DeleteSession(t.Context(), "preparing"); err != nil {
		t.Fatal(err)
	}
	stream, err = client.Replay(t.Context(), &v1.ReplayRequest{Session: "preparing"})
	if err == nil {
		_, err = stream.Recv()
	}
	if status.Code(err) != codes.NotFound {
		t.Fatalf("tombstoned PREPARING replay=%v, want NotFound", err)
	}
	if err := store.AbortFork(t.Context(), batch); err != nil {
		t.Fatal(err)
	}
	stream, err = client.Replay(t.Context(), &v1.ReplayRequest{Session: "preparing"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = stream.Recv(); err != nil {
		t.Fatalf("aborted audit unavailable: %v", err)
	}
	if _, err = stream.Recv(); err != io.EOF {
		t.Fatalf("aborted audit tail=%v", err)
	}
}
