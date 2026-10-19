package session_test

import (
	"errors"
	"io"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/agent-primitives/sessions/api"
	v1 "github.com/agent-primitives/sessions/api/genpb"
	"github.com/agent-primitives/sessions/harness/echoagent"
	"github.com/agent-primitives/sessions/runtime/local"
	"github.com/agent-primitives/sessions/sqlitelog"
	"github.com/agent-primitives/sessions/wire"
)

// Writing to a session UID that was never created is NOT_FOUND (OPS-10) and does not materialize
// the session as a side effect.
func TestUnknownSessionWritesAreNotFound(t *testing.T) {
	store, err := sqlitelog.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	backend := local.New(echoagent.Harness{})
	t.Cleanup(func() { _ = backend.Close() })
	client := serveRegistry(t, store, echoRegistry(t, backend))

	const uid = "sess-never-created"
	drainErr := func(recv func() error) error {
		for {
			if err := recv(); err != nil {
				if errors.Is(err, io.EOF) {
					return nil
				}
				return err
			}
		}
	}
	for _, tc := range []struct {
		name string
		call func() error
	}{
		{"Exec", func() error {
			stream, err := client.Exec(t.Context(), &v1.ExecRequest{Session: uid, Inputs: []*v1.Message{wire.MessageToProto(api.TextMessage("user", "hi"))}})
			if err != nil {
				return err
			}
			return drainErr(func() error { _, err := stream.Recv(); return err })
		}},
		{"Suspend", func() error { _, err := client.Suspend(t.Context(), &v1.SuspendRequest{Session: uid}); return err }},
		{"Resume", func() error { _, err := client.Resume(t.Context(), &v1.ResumeRequest{Session: uid}); return err }},
		{"Fork", func() error { _, err := client.Fork(t.Context(), &v1.ForkRequest{Session: uid, Count: 1}); return err }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call()
			if status.Code(err) != codes.NotFound {
				t.Errorf("%s(unknown) = %v, want NotFound", tc.name, err)
			}
			if _, err := store.SessionInfo(uid); !errors.Is(err, sqlitelog.ErrSessionNotFound) {
				t.Errorf("%s materialized the session: SessionInfo = %v", tc.name, err)
			}
			if head, err := store.Session(uid).Head(); err != nil || head != 0 {
				t.Errorf("%s wrote to the journal: head=%d err=%v", tc.name, head, err)
			}
		})
	}
}
