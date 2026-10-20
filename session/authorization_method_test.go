package session_test

import (
	"context"
	"testing"

	"github.com/agent-primitives/sessions/api"
	pb "github.com/agent-primitives/sessions/api/genpb"
	"github.com/agent-primitives/sessions/session"
	"github.com/agent-primitives/sessions/sqlitelog"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func assertAuthorizationMethod(t *testing.T, r api.AuthorizationRequest, want string) {
	t.Helper()
	if r.Method != want {
		t.Errorf("authorization Method=%q, want %q", r.Method, want)
	}
}

func TestAuthorizationLegacyHistoryPolicy(t *testing.T) {
	store := openStore(t, ":memory:")
	r, b := publicGateRegistry(t, &publicGateHarness{}, nil)
	if err := store.PutSession(sqlitelog.SessionMeta{UID: "legacy", Harness: "gate"}); err != nil {
		t.Fatal(err)
	}
	var requests []api.AuthorizationRequest
	policy := policyFunc(func(_ context.Context, q api.AuthorizationRequest) error {
		requests = append(requests, q)
		if q.Owner != (api.CallerIdentity{}) || q.SessionUID != "legacy" || q.Action != api.ActionRead {
			t.Errorf("legacy authorization request: %+v", q)
			return api.ErrPermissionDenied
		}
		return nil
	})
	svc := session.NewService(store, r, session.WithAuthorization(policy))
	ctx := callerContext(t, authOwner)
	if _, err := svc.GetSession(ctx, &pb.GetSessionRequest{Uid: "legacy"}); err != nil {
		t.Errorf("custom policy legacy Get: %v", err)
	}
	if err := svc.Replay(&pb.ReplayRequest{Session: "legacy"}, &replayAuthStream{rpcStream: rpcStream{ctx: ctx}}); err != nil {
		t.Errorf("custom policy legacy Replay: %v", err)
	}
	if len(requests) != 2 {
		t.Fatalf("legacy history reached policy %d times, want 2", len(requests))
	}
	assertAuthorizationMethod(t, requests[0], pb.Sessions_GetSession_FullMethodName)
	assertAuthorizationMethod(t, requests[1], pb.Sessions_Replay_FullMethodName)
	if _, err := svc.GetSession(ctx, &pb.GetSessionRequest{Uid: "missing"}); status.Code(err) != codes.PermissionDenied || len(requests) != 2 {
		t.Fatalf("missing session reached policy or leaked existence: %v", err)
	}
	if b.ioCounts() != ([4]int32{}) {
		t.Fatal("legacy history read touched runtime")
	}
}
