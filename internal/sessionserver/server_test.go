package sessionserver_test

import (
	"context"
	"net"
	"path/filepath"
	"testing"
	"time"

	v1 "github.com/agent-primitives/sessions/api/genpb"
	"github.com/agent-primitives/sessions/client"
	"github.com/agent-primitives/sessions/harness/echoagent"
	"github.com/agent-primitives/sessions/internal/sessionserver"
	"github.com/agent-primitives/sessions/placement"
	"github.com/agent-primitives/sessions/runtime/local"
	"github.com/agent-primitives/sessions/sqlitelog"
)

func TestServeRefusesRegistrationCollision(t *testing.T) {
	backend := local.New(echoagent.Harness{})
	defer func() { _ = backend.Close() }()
	for _, tc := range []struct {
		name    string
		retire  bool
		wantErr bool
	}{
		{name: "echo", wantErr: true},
		{name: "echo", retire: true, wantErr: true},
		{name: "chat", wantErr: true},
		{name: "shouty", wantErr: true},
		{name: "suite-agent"},
	} {
		name := tc.name
		if tc.retire {
			name += "/retired"
		}
		t.Run(name, func(t *testing.T) {
			registry, err := placement.NewRegistry("echo", map[string]*placement.Placer{
				"echo":   placement.New(backend, echoagent.Model),
				"shouty": placement.New(backend, echoagent.Model),
			}, placement.WithReservedNames("chat"))
			if err != nil {
				t.Fatal(err)
			}
			journal := filepath.Join(t.TempDir(), "journal.db")
			store, err := sqlitelog.Open(journal)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = store.Close() }()
			if _, _, err := store.RegisterHarness(sqlitelog.HarnessRecord{
				Name: tc.name, UID: "u", Spec: "{}", SpecDigest: "d",
			}); err != nil {
				t.Fatal(err)
			}
			if tc.retire {
				if _, err := store.RetireHarness(tc.name, "test"); err != nil {
					t.Fatal(err)
				}
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			lis, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			err = sessionserver.Serve(ctx, lis, sessionserver.Config{Journal: journal}, registry, nil)
			if (err != nil) != tc.wantErr {
				t.Fatalf("Serve = %v, want error %v", err, tc.wantErr)
			}
		})
	}
}

func TestServePersistsSessionsAcrossRestart(t *testing.T) {
	journal := filepath.Join(t.TempDir(), "journal.db")
	uid := serveOnce(t, journal, "")
	serveOnce(t, journal, uid)
}

func serveOnce(t *testing.T, journal, uid string) string {
	t.Helper()
	backend := local.New(echoagent.Harness{})
	defer func() { _ = backend.Close() }()
	registry, err := placement.NewRegistry("echo", map[string]*placement.Placer{
		"echo": placement.New(backend, echoagent.Model),
	})
	if err != nil {
		t.Fatal(err)
	}

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- sessionserver.Serve(ctx, lis, sessionserver.Config{
			Journal:          journal,
			Project:          "test",
			ModelDescription: "echo",
		}, registry, nil)
	}()

	c, err := client.Dial(lis.Addr().String(), client.WithProject("test"))
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()

	callCtx, callCancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer callCancel()
	if uid == "" {
		turn, err := c.Exec(callCtx, client.ExecOptions{
			Inputs: []string{"hello"},
		})
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		uid = turn.Session.GetMetadata().GetUid()
	} else {
		session, err := c.GetSession(callCtx, uid)
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		if session.GetLastSeq() == 0 {
			cancel()
			t.Fatal("persisted session has no committed records")
		}
		records, err := c.Replay(callCtx, uid, 1, 0)
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		hasInput := false
		for _, record := range records {
			if record.GetEvent().GetKind() == v1.EventKind_EVENT_INPUT {
				hasInput = true
				break
			}
		}
		if !hasInput {
			cancel()
			t.Fatalf("persisted records = %v", records)
		}
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server did not stop after cancellation")
	}
	return uid
}
