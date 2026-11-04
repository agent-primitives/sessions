package session_test

import (
	"context"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/agent-primitives/sessions/api"
	v1 "github.com/agent-primitives/sessions/api/genpb"
	"github.com/agent-primitives/sessions/controller"
	"github.com/agent-primitives/sessions/session"
	"github.com/agent-primitives/sessions/sqlitelog"
	"github.com/agent-primitives/sessions/wire"
)

func publicClose(uid string) *v1.CloseApprovalRequest {
	return &v1.CloseApprovalRequest{Session: uid, ExecutionId: "execution", ToolCallId: "call", RequestSeq: 3}
}

func TestCloseApprovalPublicValidation(t *testing.T) {
	store := openStore(t, ":memory:")
	r, b := publicGateRegistry(t, &publicGateHarness{}, nil)
	c := serveRegistry(t, store, r)
	for _, tc := range []struct {
		name   string
		change func(*v1.CloseApprovalRequest)
		want   codes.Code
	}{
		{"session", func(q *v1.CloseApprovalRequest) { q.Session = "" }, codes.InvalidArgument},
		{"execution", func(q *v1.CloseApprovalRequest) { q.ExecutionId = "" }, codes.InvalidArgument},
		{"call", func(q *v1.CloseApprovalRequest) { q.ToolCallId = "" }, codes.InvalidArgument},
		{"seq", func(q *v1.CloseApprovalRequest) { q.RequestSeq = 0 }, codes.InvalidArgument},
		{"negative", func(q *v1.CloseApprovalRequest) { q.RequestSeq = -1 }, codes.InvalidArgument},
		{"unknown", func(q *v1.CloseApprovalRequest) {}, codes.NotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q := publicClose("unknown")
			tc.change(q)
			if _, err := c.CloseApproval(t.Context(), q); status.Code(err) != tc.want {
				t.Fatalf("CloseApproval=%v, want %v", err, tc.want)
			}
		})
	}
	if b.ioCounts() != ([4]int32{}) {
		t.Fatal("validation touched runtime")
	}
	list, err := c.ListSessions(t.Context(), &v1.ListSessionsRequest{})
	if err != nil || len(list.Sessions) != 0 {
		t.Fatalf("unknown close created journal: %v %v", list, err)
	}
}

// Over the public API an undecided gate blocks a new turn until CloseApproval writes the host
// denial and NOT_EXECUTED receipt; a retry returns the same records, a wrong tuple and a deleted
// session are FAILED_PRECONDITION, and the closed session takes a new turn without the runtime
// being touched by the close itself.
func TestCloseApprovalPublicUnblocksExec(t *testing.T) {
	store := openStore(t, ":memory:")
	r, b := publicGateRegistry(t, &publicGateHarness{}, nil)
	c := serveRegistry(t, store, r)
	if err := store.PutSession(sqlitelog.SessionMeta{UID: "s", Harness: "gate"}); err != nil {
		t.Fatal(err)
	}
	log := store.Session("s")
	seedPublicGate(t, log, "gate", api.EventApprovalRequest)
	turn := &v1.ExecRequest{Session: "s", Inputs: []*v1.Message{wire.MessageToProto(api.TextMessage("user", "next"))}}
	if _, err := collectPublicExec(c, turn); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("Exec on a blocked session = %v, want FAILED_PRECONDITION", err)
	}

	for name, change := range map[string]func(*v1.CloseApprovalRequest){
		"execution": func(q *v1.CloseApprovalRequest) { q.ExecutionId = "other" },
		"call":      func(q *v1.CloseApprovalRequest) { q.ToolCallId = "other" },
		"seq":       func(q *v1.CloseApprovalRequest) { q.RequestSeq = 2 },
	} {
		q := publicClose("s")
		change(q)
		if _, err := c.CloseApproval(t.Context(), q); status.Code(err) != codes.FailedPrecondition {
			t.Fatalf("wrong %s = %v, want FAILED_PRECONDITION", name, err)
		}
	}
	if head, err := log.Head(); err != nil || head != 3 {
		t.Fatalf("refused closes appended: head %d, %v", head, err)
	}

	closed, err := c.CloseApproval(t.Context(), publicClose("s"))
	if err != nil {
		t.Fatal(err)
	}
	decision := closed.GetDecision().GetEvent().GetApprovalResult()
	if closed.GetDecision().GetSeq() != 4 || decision.GetApproved() || decision.GetReason() != controller.ApprovalCloseReason || decision.GetVerifiedApprover() != nil {
		t.Fatalf("decision = %v", closed.GetDecision())
	}
	receipt := closed.GetReceipt()
	if receipt.GetSeq() != 5 || receipt.GetEvent().GetResult().GetCode() != v1.ToolResult_NOT_EXECUTED || !receipt.GetEvent().GetResult().GetIsError() {
		t.Fatalf("receipt = %v", receipt)
	}
	if closed.GetSession().GetLastSeq() != 5 || closed.GetSession().GetPendingApproval() != nil {
		t.Fatalf("session = %v", closed.GetSession())
	}
	retry, err := c.CloseApproval(t.Context(), publicClose("s"))
	if err != nil || !proto.Equal(retry.GetDecision(), closed.GetDecision()) || !proto.Equal(retry.GetReceipt(), closed.GetReceipt()) {
		t.Fatalf("retry = %v, %v", retry, err)
	}
	if head, err := log.Head(); err != nil || head != 5 {
		t.Fatalf("retry appended: head %d, %v", head, err)
	}
	if b.ioCounts() != ([4]int32{}) {
		t.Fatalf("close touched runtime: %v", b.ioCounts())
	}
	if err := controller.VerifyJournal(log); err != nil {
		t.Fatalf("VerifyJournal: %v", err)
	}

	if _, err := collectPublicExec(c, turn); err != nil {
		t.Fatalf("Exec after close = %v", err)
	}
	starts := 0
	for _, rec := range routingRecords(t, log) {
		if rec.Event.Kind == api.EventExecutionStart {
			starts++
		}
	}
	if starts != 2 {
		t.Fatalf("EXECUTION_STARTs after close = %d, want a new turn", starts)
	}

	if _, err := c.DeleteSession(t.Context(), &v1.DeleteSessionRequest{Uid: "s"}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.CloseApproval(t.Context(), publicClose("s")); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("close on a deleted session = %v, want FAILED_PRECONDITION", err)
	}
}

// CloseApproval needs read and approval.close, checked in that order before validation or any
// journal read: a caller allowed read but not approval.close is PERMISSION_DENIED with nothing
// appended. The owner policy lets the owner close and no one else; the allowlist policy delegates
// the action to it. The authenticated closer is the denial's verified approver.
func TestCloseApprovalAuthorization(t *testing.T) {
	other := api.CallerIdentity{Issuer: authOwner.Issuer, Subject: "other"}
	setup := func(t *testing.T, policy api.Authorizer) (*session.Service, *sqlitelog.Store) {
		t.Helper()
		store := openStore(t, ":memory:")
		r, _ := publicGateRegistry(t, &publicGateHarness{}, nil)
		if err := store.CreateOwnedSession(sqlitelog.SessionMeta{UID: "owned", Harness: "gate"}, authOwner, false); err != nil {
			t.Fatal(err)
		}
		seedPublicGate(t, store.Session("owned"), "gate", api.EventApprovalRequest)
		return session.NewService(store, r, session.WithAuthorization(policy)), store
	}
	unchanged := func(t *testing.T, store *sqlitelog.Store) {
		t.Helper()
		if head, err := store.Session("owned").Head(); err != nil || head != 3 {
			t.Fatalf("denied close appended: head %d, %v", head, err)
		}
	}

	t.Run("read without approval.close", func(t *testing.T) {
		var actions []api.Action
		svc, store := setup(t, policyFunc(func(_ context.Context, r api.AuthorizationRequest) error {
			actions = append(actions, r.Action)
			if r.Action == api.ActionApprovalClose {
				return api.ErrPermissionDenied
			}
			return nil
		}))
		if _, err := svc.CloseApproval(callerContext(t, authOwner), publicClose("owned")); status.Code(err) != codes.PermissionDenied {
			t.Fatalf("CloseApproval = %v, want PERMISSION_DENIED", err)
		}
		if !slices.Equal(actions, []api.Action{api.ActionRead, api.ActionApprovalClose}) {
			t.Fatalf("actions = %v", actions)
		}
		unchanged(t, store)
	})
	t.Run("approve does not imply approval.close", func(t *testing.T) {
		svc, store := setup(t, policyFunc(func(_ context.Context, r api.AuthorizationRequest) error {
			if r.Action == api.ActionRead || r.Action == api.ActionApprove {
				return nil
			}
			return api.ErrPermissionDenied
		}))
		if _, err := svc.CloseApproval(callerContext(t, authOwner), publicClose("owned")); status.Code(err) != codes.PermissionDenied {
			t.Fatalf("CloseApproval = %v, want PERMISSION_DENIED", err)
		}
		unchanged(t, store)
	})
	for name, policy := range map[string]api.Authorizer{
		"owner":     api.OwnerAuthorizer{},
		"allowlist": api.OwnerAllowlistAuthorizer{},
	} {
		t.Run(name, func(t *testing.T) {
			svc, store := setup(t, policy)
			if _, err := svc.CloseApproval(callerContext(t, other), publicClose("owned")); status.Code(err) != codes.PermissionDenied {
				t.Fatalf("non-owner CloseApproval = %v, want PERMISSION_DENIED", err)
			}
			unchanged(t, store)
			closed, err := svc.CloseApproval(callerContext(t, authOwner), publicClose("owned"))
			if err != nil {
				t.Fatal(err)
			}
			got := closed.GetDecision().GetEvent().GetApprovalResult().GetVerifiedApprover()
			if got.GetIssuer() != authOwner.Issuer || got.GetSubject() != authOwner.Subject {
				t.Fatalf("verified approver = %v, want the closer %+v", got, authOwner)
			}
			if closed.GetReceipt().GetEvent().GetResult().GetCode() != v1.ToolResult_NOT_EXECUTED {
				t.Fatalf("receipt = %v", closed.GetReceipt())
			}
		})
	}
}

// A gate whose TOOL_CALL changed in storage is DATA_LOSS and the close writes nothing.
func TestCloseApprovalTamperedJournalIsDataLoss(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.db")
	store := openStore(t, path)
	db := deletionSQL(t, path)
	r, _ := publicGateRegistry(t, &publicGateHarness{}, nil)
	c := serveRegistry(t, store, r)
	if err := store.PutSession(sqlitelog.SessionMeta{UID: "s", Harness: "gate"}); err != nil {
		t.Fatal(err)
	}
	seedPublicGate(t, store.Session("s"), "gate", api.EventApprovalRequest)
	tamperEvent(t, db, "s", 2, func(ev *api.Event) {
		if ev.Kind != api.EventToolCall {
			t.Fatalf("seq 2 is %v, want TOOL_CALL", ev.Kind)
		}
		ev.ToolCall.Args = map[string]any{"path": "elsewhere"}
	})
	before := journalRows(t, db, "s")
	if _, err := c.CloseApproval(t.Context(), publicClose("s")); status.Code(err) != codes.DataLoss {
		t.Fatalf("CloseApproval = %v, want DATA_LOSS", err)
	}
	if after := journalRows(t, db, "s"); !reflect.DeepEqual(after, before) {
		t.Fatalf("journal changed: %d records -> %d", len(before), len(after))
	}
}
