package session_test

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/agent-primitives/sessions/api"
	pb "github.com/agent-primitives/sessions/api/genpb"
	"github.com/agent-primitives/sessions/eventlog"
	"github.com/agent-primitives/sessions/harness/echoagent"
	"github.com/agent-primitives/sessions/placement"
	"github.com/agent-primitives/sessions/runtime/local"
	"github.com/agent-primitives/sessions/session"
	"github.com/agent-primitives/sessions/sqlitelog"
	"github.com/agent-primitives/sessions/wire"
)

// confirmCanary is in the error a failed allocation confirmation returns, so a test can tell
// whether cause text reached the client.
const confirmCanary = "confirm-canary-3f9c"

// confirmFailStore fails every allocation confirmation while fail is set.
type confirmFailStore struct {
	*sqlitelog.Store
	fail *atomic.Bool
}

func (s confirmFailStore) SessionLog(uid string) eventlog.SessionJournal {
	return confirmFailLog{Log: s.Session(uid), fail: s.fail}
}

type confirmFailLog struct {
	*sqlitelog.Log
	fail *atomic.Bool
}

func (l confirmFailLog) ConfirmAllocation(ctx context.Context, attempt string, in api.Incarnation) error {
	if l.fail.Load() {
		return errors.New(confirmCanary)
	}
	return l.Log.ConfirmAllocation(ctx, attempt, in)
}

// A failed Exec or Resume that left compute running reports that compute under WithAuthorization,
// in fixed text naming the incarnation and nothing from the cause; every other INTERNAL failure is
// still sanitized, and without WithAuthorization the message keeps the cause as before.
func TestComputeLeftRunningSurvivesAuthorizationSanitizing(t *testing.T) {
	const left = "may still be running"
	for _, tc := range []struct {
		name       string
		authorized bool
		releasable bool
	}{
		{name: "enforced, compute left running", authorized: true},
		{name: "enforced, compute released", authorized: true, releasable: true},
		{name: "off, compute left running"},
		{name: "off, compute released", releasable: true},
	} {
		for _, op := range []string{"exec", "resume"} {
			t.Run(tc.name+"/"+op, func(t *testing.T) {
				inner := local.New(echoagent.Harness{})
				t.Cleanup(func() { _ = inner.Close() })
				var backend placement.Backend = inner
				if !tc.releasable {
					backend = struct{ placement.Backend }{inner}
				}
				fail := new(atomic.Bool)
				store := confirmFailStore{Store: openStore(t, ":memory:"), fail: fail}
				var opts []session.Option
				if tc.authorized {
					opts = append(opts, session.WithAuthorization(policyFunc(func(context.Context, api.AuthorizationRequest) error { return nil })))
				}
				svc := session.NewService(store, echoRegistry(t, backend), opts...)
				ctx := callerContext(t, authOwner)
				created, err := svc.CreateSession(ctx, &pb.CreateSessionRequest{Session: &pb.Session{Harness: "echo"}})
				if err != nil {
					t.Fatal(err)
				}
				uid := created.GetMetadata().GetUid()
				exec := func() error {
					return svc.Exec(&pb.ExecRequest{
						Session: uid,
						Inputs:  []*pb.Message{wire.MessageToProto(api.TextMessage("user", "hi"))},
					}, &rpcStream{ctx: ctx})
				}
				if op == "resume" {
					if err := exec(); err != nil {
						t.Fatalf("first Exec: %v", err)
					}
					if _, err := svc.Suspend(ctx, &pb.SuspendRequest{Session: uid}); err != nil {
						t.Fatalf("Suspend: %v", err)
					}
				}
				fail.Store(true)
				if op == "exec" {
					err = exec()
				} else {
					_, err = svc.Resume(ctx, &pb.ResumeRequest{Session: uid})
				}
				if status.Code(err) != codes.Internal {
					t.Fatalf("%s = %v, want INTERNAL", op, err)
				}
				msg := status.Convert(err).Message()
				if tc.authorized {
					want := "internal error"
					if !tc.releasable {
						// The local runtime's incarnation ID is the session UID.
						want += `; compute instance "` + uid + `" ` + left
					}
					if msg != want {
						t.Fatalf("%s message = %q, want %q", op, msg, want)
					}
					return
				}
				if !strings.Contains(msg, confirmCanary) || strings.Contains(msg, left) == tc.releasable {
					t.Fatalf("%s message = %q, want the cause, reporting compute left running only when unreleasable", op, msg)
				}
			})
		}
	}
}
