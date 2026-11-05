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
	"github.com/agent-primitives/sessions/controller"
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

// releaseCounter is the local runtime counting its ReleaseSession calls.
type releaseCounter struct {
	*local.Backend
	releases atomic.Int32
}

func (b *releaseCounter) ReleaseSession(ctx context.Context, uid string) error {
	b.releases.Add(1)
	return b.Backend.ReleaseSession(ctx, uid)
}

// A failed Exec or Resume that left compute running reports that compute under WithAuthorization,
// in fixed text naming the incarnation and nothing from the cause; every other INTERNAL failure is
// still sanitized, and without WithAuthorization the message keeps the cause as before. The "after
// close" operations first park a turn on approval and close the gate with CloseApproval, whose
// receipt the host writes without compute, so it must not read as owning the compute either.
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
		for _, op := range []string{"exec", "resume", "exec after close", "resume after close"} {
			t.Run(tc.name+"/"+op, func(t *testing.T) {
				closing := strings.HasSuffix(op, " after close")
				harness := "echo"
				var inner *local.Backend
				if closing {
					harness = "gate"
					inner = local.New(&publicGateHarness{})
				} else {
					inner = local.New(echoagent.Harness{})
				}
				t.Cleanup(func() { _ = inner.Close() })
				counted := &releaseCounter{Backend: inner}
				var backend placement.Backend = counted
				if !tc.releasable {
					backend = struct{ placement.Backend }{inner}
				}
				var model controller.ModelFunc = echoagent.Model
				if closing {
					model = nil
				}
				placers := map[string]*placement.Placer{harness: placement.New(backend, model)}
				registry, err := placement.NewRegistry(harness, placers)
				if err != nil {
					t.Fatal(err)
				}
				fail := new(atomic.Bool)
				store := confirmFailStore{Store: openStore(t, ":memory:"), fail: fail}
				var opts []session.Option
				if tc.authorized {
					opts = append(opts, session.WithAuthorization(policyFunc(func(context.Context, api.AuthorizationRequest) error { return nil })))
				}
				svc := session.NewService(store, registry, opts...)
				ctx := callerContext(t, authOwner)
				created, err := svc.CreateSession(ctx, &pb.CreateSessionRequest{Session: &pb.Session{Harness: harness}})
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
				switch {
				case closing:
					if err := exec(); err != nil {
						t.Fatalf("parking Exec: %v", err)
					}
					parked, err := svc.GetSession(ctx, &pb.GetSessionRequest{Uid: uid})
					if err != nil {
						t.Fatal(err)
					}
					ref := parked.GetPendingApproval()
					if parked.GetComputeState() != pb.ComputeState_COMPUTE_COLD || ref.GetRequestSeq() <= 0 {
						t.Fatalf("parked session = %v, want COLD with a pending approval", parked)
					}
					closed, err := svc.CloseApproval(ctx, &pb.CloseApprovalRequest{Session: uid,
						ExecutionId: ref.GetExecutionId(), ToolCallId: ref.GetToolCallId(), RequestSeq: ref.GetRequestSeq()})
					if err != nil {
						t.Fatalf("CloseApproval: %v", err)
					}
					if code := closed.GetReceipt().GetEvent().GetResult().GetCode(); code != pb.ToolResult_NOT_EXECUTED {
						t.Fatalf("receipt code = %v, want NOT_EXECUTED", code)
					}
				case op == "resume":
					if err := exec(); err != nil {
						t.Fatalf("first Exec: %v", err)
					}
					if _, err := svc.Suspend(ctx, &pb.SuspendRequest{Session: uid}); err != nil {
						t.Fatalf("Suspend: %v", err)
					}
				}
				fail.Store(true)
				releasesBefore := counted.releases.Load()
				if strings.HasPrefix(op, "exec") {
					err = exec()
				} else {
					_, err = svc.Resume(ctx, &pb.ResumeRequest{Session: uid})
				}
				if status.Code(err) != codes.Internal {
					t.Fatalf("%s = %v, want INTERNAL", op, err)
				}
				if released, want := counted.releases.Load()-releasesBefore, map[bool]int32{true: 1}[tc.releasable]; released != want {
					t.Fatalf("%s released %d times, want %d", op, released, want)
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
