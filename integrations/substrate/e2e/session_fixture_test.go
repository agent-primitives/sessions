package e2e

import (
	"context"
	"net"
	"path/filepath"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/test/bufconn"

	v1 "github.com/agent-primitives/sessions/api/genpb"
	"github.com/agent-primitives/sessions/client"
	"github.com/agent-primitives/sessions/eventlog"
	"github.com/agent-primitives/sessions/placement"
	"github.com/agent-primitives/sessions/session"
	"github.com/agent-primitives/sessions/sqlitelog"
)

type sessionAPI struct {
	client   *client.Client
	store    *sqlitelog.Store
	path     string
	registry *placement.Registry
	closeRPC func()
}

func newSessionAPI(t *testing.T, registry *placement.Registry) *sessionAPI {
	t.Helper()
	a := &sessionAPI{path: filepath.Join(t.TempDir(), "sessions.db"), registry: registry}
	a.open(t)
	t.Cleanup(func() { a.close(t) })
	return a
}

func (a *sessionAPI) open(t *testing.T) {
	t.Helper()
	var err error
	a.store, err = sqlitelog.Open(a.path)
	if err != nil {
		t.Fatal(err)
	}
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	v1.RegisterSessionsServer(srv, session.NewService(a.store, a.registry))
	go func() { _ = srv.Serve(lis) }()
	a.client, err = client.Dial("passthrough:///session-fixture", client.WithDialOptions(grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) })))
	if err != nil {
		srv.Stop()
		_ = a.store.Close()
		t.Fatal(err)
	}
	a.closeRPC = func() { _ = a.client.Close(); srv.Stop(); _ = lis.Close() }
}

func (a *sessionAPI) close(t *testing.T) {
	t.Helper()
	if a.closeRPC != nil {
		a.closeRPC()
		a.closeRPC = nil
	}
	if a.store != nil {
		if err := a.store.Close(); err != nil {
			t.Error(err)
		}
		a.store = nil
	}
}

func (a *sessionAPI) reopen(t *testing.T) { t.Helper(); a.close(t); a.open(t) }

func (a *sessionAPI) records(t *testing.T, uid string) []eventlog.Record {
	t.Helper()
	r, err := a.store.Session(uid).Read(1)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.store.Session(uid).Verify(); err != nil {
		t.Fatalf("Verify %s: %v", uid, err)
	}
	return r
}
