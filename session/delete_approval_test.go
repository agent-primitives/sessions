package session_test

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/agent-primitives/sessions/api"
	v1 "github.com/agent-primitives/sessions/api/genpb"
	"github.com/agent-primitives/sessions/eventlog"
	"github.com/agent-primitives/sessions/session"
	"github.com/agent-primitives/sessions/sqlitelog"
)

// Deleted sessions retain the truthful execution axis but never expose an actionable
// reference. Public and shared-host approval entry points reject historical retries too.
func TestDeleteApprovalCapturedRepliesAndAuthority(t *testing.T) {
	for _, shape := range []string{"empty", "parked", "approved"} {
		t.Run(shape, func(t *testing.T) {
			ctx := context.Background()
			store := openStore(t, filepath.Join(t.TempDir(), "session.db"))
			r, b := publicGateRegistry(t, &publicGateHarness{}, nil)
			svc := session.NewService(store, r)
			c := serveRegistry(t, store, r)
			const uid = "deleted-gate"
			if err := store.PutSession(sqlitelog.SessionMeta{UID: uid, Harness: "gate"}); err != nil {
				t.Fatal(err)
			}
			wantState, wantCompute, wantSeq := v1.ExecState_EXEC_PENDING, v1.ComputeState_COMPUTE_NONE, int64(1)
			if shape != "empty" {
				seedPublicGate(t, store.Session(uid), "gate", api.EventApprovalRequest)
				if _, err := store.Session(uid).Append(3, 1, api.Event{Kind: api.EventLifecycle, Lifecycle: &api.Lifecycle{Kind: api.LifecycleSuspend}}); err != nil {
					t.Fatal(err)
				}
				wantState, wantCompute, wantSeq = v1.ExecState_EXEC_AWAITING, v1.ComputeState_COMPUTE_COLD, 5
				if shape == "approved" {
					if _, err := svc.Approve(ctx, publicDecision(uid)); err != nil {
						t.Fatal(err)
					}
					wantState, wantSeq = v1.ExecState_EXEC_RUNNING, 6
				}
			}
			beforeIO := b.ioCounts()
			deleted, err := c.DeleteSession(ctx, &v1.DeleteSessionRequest{Uid: uid})
			if err != nil {
				t.Fatal(err)
			}
			if deleted.LastSeq != wantSeq || deleted.ExecState != wantState || deleted.ComputeState != wantCompute || deleted.PendingApproval != nil {
				t.Errorf("deleted captured reply=%v", deleted)
			}
			for _, read := range []func() (*v1.Session, error){func() (*v1.Session, error) { return c.GetSession(ctx, &v1.GetSessionRequest{Uid: uid}) }, func() (*v1.Session, error) { return c.DeleteSession(ctx, &v1.DeleteSessionRequest{Uid: uid}) }} {
				got, err := read()
				if err != nil || !proto.Equal(got, deleted) {
					t.Errorf("Get/retry=%v %v, want %v", got, err, deleted)
				}
			}
			before, err := store.Session(uid).Snapshot()
			if err != nil {
				t.Fatal(err)
			}
			if _, err := c.Approve(ctx, publicDecision(uid)); status.Code(err) != codes.FailedPrecondition {
				t.Errorf("public deleted decision=%v", err)
			}
			if _, err := svc.Approve(ctx, publicDecision(uid)); !errors.Is(err, eventlog.ErrDeleted) || status.Code(err) != codes.FailedPrecondition {
				t.Errorf("direct deleted decision=%v", err)
			}
			if _, err := r.Approve(ctx, store.Session(uid), uid, api.ApprovalDecision{ExecutionID: "execution", ToolCallID: "call", RequestSeq: 3, Approved: false}); !errors.Is(err, eventlog.ErrDeleted) {
				t.Errorf("Registry deleted decision=%v", err)
			}
			for _, op := range []string{"Exec", "Fork", "Resume", "Suspend"} {
				if err := deleteRaceMutation(ctx, c, op, uid, "unserved"); status.Code(err) != codes.FailedPrecondition {
					t.Errorf("deleted approval %s=%v", op, err)
				}
			}
			after, err := store.Session(uid).Snapshot()
			if err != nil || !reflect.DeepEqual(after, before) {
				t.Fatal("decision refusal changed retained audit")
			}
			page, err := c.ListSessions(ctx, &v1.ListSessionsRequest{})
			if err != nil || len(page.Sessions) != 0 {
				t.Fatalf("deleted listed: %v %v", page, err)
			}
			if b.ioCounts() != beforeIO {
				t.Fatal("deleted approval/delete contacted compute")
			}
		})
	}
}
