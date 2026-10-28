package session_test

import (
	"context"
	"sync/atomic"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/agent-primitives/sessions/api"
	pb "github.com/agent-primitives/sessions/api/genpb"
	"github.com/agent-primitives/sessions/controller"
	"github.com/agent-primitives/sessions/session"
	"github.com/agent-primitives/sessions/sqlitelog"
)

// An enforcing service journals the caller its authentication verified as the decision's approver,
// whatever identity the request claims. An exact retry by that caller returns the decision; the
// same tuple from another authorized caller is a changed decision. Recovery runs the approved call
// once and the chain, approver included, verifies.
func TestApproveJournalsVerifiedApprover(t *testing.T) {
	store := openStore(t, ":memory:")
	var effects atomic.Int32
	r, _ := publicGateRegistry(t, &publicGateHarness{}, func(context.Context, controller.ToolCallContext, api.ToolCall) (api.ToolResult, error) {
		effects.Add(1)
		return api.ToolResult{Output: map[string]any{"ok": true}}, nil
	})
	if err := store.CreateOwnedSession(sqlitelog.SessionMeta{UID: "owned", Harness: "gate"}, authOwner, false); err != nil {
		t.Fatal(err)
	}
	allowAll := policyFunc(func(context.Context, api.AuthorizationRequest) error { return nil })
	svc := session.NewService(store, r, session.WithAuthorization(allowAll))
	owner := callerContext(t, authOwner)
	if err := svc.Exec(&pb.ExecRequest{Session: "owned"}, &rpcStream{ctx: owner}); err != nil {
		t.Fatal(err)
	}
	state, err := controller.InspectApproval(store.Session("owned"), 0)
	if err != nil || state == nil || state.Request == nil {
		t.Fatalf("no parked request: %+v, %v", state, err)
	}
	q := &pb.ApproveRequest{
		Session: "owned", ExecutionId: state.ExecutionID, ToolCallId: state.Call.ID, RequestSeq: state.Request.Seq, Approved: proto.Bool(true),
		Identity: &pb.IdentityRef{Principal: "forged", Issuer: "https://forged.example", Subject: "someone-else"},
	}
	decision, err := svc.Approve(owner, q)
	if err != nil {
		t.Fatal(err)
	}
	result := decision.GetDecision().GetEvent().GetApprovalResult()
	if got := result.GetVerifiedApprover(); got.GetIssuer() != authOwner.Issuer || got.GetSubject() != authOwner.Subject {
		t.Fatalf("verified approver = %v, want %+v", got, authOwner)
	}
	if actor := decision.GetDecision().GetEvent().GetActor(); actor != nil {
		t.Fatalf("request identity reached the journal: %v", actor)
	}
	journaled := routingEvent(t, routingRecords(t, store.Session("owned")), api.EventApprovalResult)
	if journaled.ApprovalResult.VerifiedApprover != authOwner || journaled.Actor != (api.IdentityRef{}) {
		t.Fatalf("journaled decision = %+v actor %+v", journaled.ApprovalResult, journaled.Actor)
	}

	q.Identity = &pb.IdentityRef{Issuer: "https://other.example", Subject: "forged-again"}
	retry, err := svc.Approve(owner, q)
	if err != nil || !proto.Equal(retry.GetDecision(), decision.GetDecision()) {
		t.Fatalf("exact retry by the same approver = %v, %v", retry, err)
	}
	other := callerContext(t, api.CallerIdentity{Issuer: authOwner.Issuer, Subject: "other-approver"})
	if _, err := svc.Approve(other, q); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("retry by a different approver = %v, want FAILED_PRECONDITION", err)
	}
	head, err := store.Session("owned").Head()
	if err != nil || head != decision.GetDecision().GetSeq() {
		t.Fatalf("retries appended: head %d, %v", head, err)
	}

	if _, err := svc.Resume(owner, &pb.ResumeRequest{Session: "owned"}); err != nil {
		t.Fatal(err)
	}
	if effects.Load() != 1 {
		t.Fatalf("approved call ran %d times, want 1", effects.Load())
	}
	if err := controller.VerifyJournal(store.Session("owned")); err != nil {
		t.Fatalf("VerifyJournal: %v", err)
	}
	if _, err := svc.Approve(owner, q); err != nil {
		t.Fatalf("exact retry after the receipt: %v", err)
	}
}

// Authentication alone records the verified approver; the request identity stays unverified
// provenance on Event.actor, as it is without authorization. Without authentication the decision
// records no approver.
func TestApproveVerifiedApproverFollowsAuthentication(t *testing.T) {
	for _, tc := range []struct {
		name string
		ctx  func(*testing.T) context.Context
		want api.CallerIdentity
	}{
		{"authenticated", func(t *testing.T) context.Context { return callerContext(t, authOwner) }, authOwner},
		{"unauthenticated", func(t *testing.T) context.Context { return t.Context() }, api.CallerIdentity{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := openStore(t, ":memory:")
			r, _ := publicGateRegistry(t, &publicGateHarness{}, nil)
			if err := store.PutSession(sqlitelog.SessionMeta{UID: "s", Harness: "gate"}); err != nil {
				t.Fatal(err)
			}
			seedPublicGate(t, store.Session("s"), "gate", api.EventApprovalRequest)
			q := publicDecision("s")
			q.Identity = &pb.IdentityRef{Principal: "human", Issuer: "claimed", Subject: "claimed-subject"}
			decision, err := session.NewService(store, r).Approve(tc.ctx(t), q)
			if err != nil {
				t.Fatal(err)
			}
			event := routingEvent(t, routingRecords(t, store.Session("s")), api.EventApprovalResult)
			if event.ApprovalResult.VerifiedApprover != tc.want {
				t.Fatalf("verified approver = %+v, want %+v", event.ApprovalResult.VerifiedApprover, tc.want)
			}
			if event.Actor != (api.IdentityRef{Principal: "human", Issuer: "claimed", Subject: "claimed-subject"}) {
				t.Fatalf("request provenance = %+v", event.Actor)
			}
			if tc.want == (api.CallerIdentity{}) && decision.GetDecision().GetEvent().GetApprovalResult().GetVerifiedApprover() != nil {
				t.Fatal("unauthenticated decision carries an approver")
			}
		})
	}
}
