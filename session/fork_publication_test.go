package session_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/agent-primitives/sessions/api"
	v1 "github.com/agent-primitives/sessions/api/genpb"
	"github.com/agent-primitives/sessions/harness/echoagent"
	"github.com/agent-primitives/sessions/runtime/local"
	"github.com/agent-primitives/sessions/session"
)

func TestForkMetadataFailureNeverPublishesPartialChildren(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.db")
	store := openStore(t, path)
	registry, backend := deletionRegistry(t)
	client := serveRegistry(t, store, registry)
	parent := mustCreate(t, client)
	if err := deletionExec(t, client, parent, ""); err != nil {
		t.Fatal(err)
	}
	db := deletionSQL(t, path)
	for _, query := range []string{
		`CREATE TRIGGER fail_child_insert BEFORE INSERT ON sessions WHEN NEW.name='second' BEGIN SELECT RAISE(ABORT,'publication rejected'); END`,
		`CREATE TRIGGER fail_child_update BEFORE UPDATE ON sessions WHEN NEW.name='second' BEGIN SELECT RAISE(ABORT,'publication rejected'); END`,
	} {
		if _, err := db.Exec(query); err != nil {
			t.Fatal(err)
		}
	}
	backend.reset()
	if _, err := client.Fork(t.Context(), &v1.ForkRequest{Session: parent, Count: 2, ChildNames: []string{"first", "second"}}); err == nil {
		t.Fatal("publication fault ignored")
	}
	listed, err := client.ListSessions(t.Context(), &v1.ListSessionsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(listed.Sessions) != 1 || listed.Sessions[0].Metadata.Uid != parent {
		t.Fatalf("partial children published: %v", listed)
	}
	if got := backend.effects()["Fork"]; got != 0 {
		t.Fatalf("metadata preflight provisioned %d children", got)
	}
}

type failedFanout struct {
	*local.Backend
	forks, stops int
}

var errFanout = errors.New("second clone failed")

func (b *failedFanout) Fork(ctx context.Context, ref api.SnapshotRef, opts api.ForkOpts) (api.Incarnation, error) {
	b.forks++
	if b.forks == 2 {
		return api.Incarnation{}, errFanout
	}
	return b.Backend.Fork(ctx, ref, opts)
}
func (b *failedFanout) Stop(ctx context.Context, in api.Incarnation) error {
	b.stops++
	return b.Backend.Stop(ctx, in)
}

func TestTrackedForkAbortDoesNotStopUnsupportedBackend(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.db")
	store := openStore(t, path)
	b := &failedFanout{Backend: local.New(echoagent.Harness{})}
	t.Cleanup(func() { _ = b.Close() })
	registry := echoRegistry(t, unsupportedCleanup{Runtime: b, describe: b.Describe})
	client := serveRegistry(t, store, registry)
	parent := mustCreate(t, client)
	if err := deletionExec(t, client, parent, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Fork(t.Context(), &v1.ForkRequest{Session: parent, Count: 2}); status.Code(err) != codes.Internal || b.forks != 2 {
		t.Fatalf("second-clone failure not reached: calls=%d, err=%v", b.forks, err)
	}
	if b.stops != 0 {
		t.Fatalf("tracked abort called unsafe Stop %d times", b.stops)
	}
	listed, err := client.ListSessions(t.Context(), &v1.ListSessionsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(listed.Sessions) != 1 {
		t.Fatalf("aborted children visible: %v", listed)
	}
	rows, err := deletionSQL(t, path).Query(`SELECT session FROM sessions WHERE session!=?`, parent)
	if err != nil {
		t.Fatal(err)
	}
	var children []string
	for rows.Next() {
		var uid string
		if err := rows.Scan(&uid); err != nil {
			t.Fatal(err)
		}
		children = append(children, uid)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	if len(children) != 2 {
		t.Fatalf("durable child tombstones=%d, want2", len(children))
	}
	for _, uid := range children {
		info, err := store.SessionInfo(uid)
		if err != nil {
			t.Fatal(err)
		}
		if info.DeletionState != api.DeletionPending {
			t.Fatalf("aborted child not tombstoned: %+v", info)
		}
		if err := store.Session(uid).Verify(); err != nil {
			t.Fatalf("invalid retained chain: %v", err)
		}
	}
	if err := session.NewService(store, registry).ReapOnce(t.Context()); err == nil {
		t.Fatal("unsupported allocations silently completed")
	}
	if b.stops != 0 {
		t.Fatal("reaper called unsafe Stop")
	}
}
