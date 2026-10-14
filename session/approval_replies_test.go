package session

import (
	"testing"

	"github.com/aramase/agentsessions/api"
	v1 "github.com/aramase/agentsessions/api/genpb"
	"github.com/aramase/agentsessions/harness/echoagent"
	"github.com/aramase/agentsessions/placement"
	"github.com/aramase/agentsessions/runtime/local"
	"github.com/aramase/agentsessions/sqlitelog"
	"google.golang.org/protobuf/proto"
)

// A missed RPC renderer must fail against the same stored snapshot at the same cursor.
func TestApprovalProjectionEverySessionReply(t *testing.T) {
	store, err := sqlitelog.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	backend := local.New(echoagent.Harness{})
	defer backend.Close()
	registry, err := placement.NewRegistry("gate", map[string]*placement.Placer{"gate": placement.New(backend, nil)})
	if err != nil {
		t.Fatal(err)
	}
	svc := NewService(store, registry)
	assertCurrent := func(name string, got *v1.Session) {
		t.Helper()
		info, err := store.SessionInfo(got.GetMetadata().GetUid())
		if err != nil {
			t.Fatal(err)
		}
		want, err := sessionProto(info)
		if err != nil || !proto.Equal(got, want) {
			t.Fatalf("%s=%v want=%v err=%v", name, got, want, err)
		}
	}
	created, err := svc.CreateSession(t.Context(), &v1.CreateSessionRequest{Session: &v1.Session{PendingApproval: &v1.ApprovalRef{ExecutionId: "forged", RequestSeq: 99}, ExecState: v1.ExecState_EXEC_AWAITING}})
	if err != nil {
		t.Fatal(err)
	}
	if created.LastSeq != 0 || created.PendingApproval != nil || created.ExecState != v1.ExecState_EXEC_PENDING {
		t.Fatalf("Create accepted output-only evidence: %v", created)
	}
	assertCurrent("Create", created)
	uid := created.Metadata.Uid
	log := store.Session(uid)
	for i, ev := range []api.Event{
		{ExecutionID: "execution", Kind: api.EventExecutionStart, ExecutionStart: &api.ExecutionStart{Harness: "gate", InputCount: proto.Int64(0)}},
		{ExecutionID: "execution", Kind: api.EventToolCall, ToolCall: &api.ToolCall{ID: "call", IdempotencyKey: "key", Mediation: api.MediationRequiresApproval}},
		{ExecutionID: "execution", Kind: api.EventApprovalRequest, Approval: &api.ApprovalRequest{ToolCallID: "call"}},
	} {
		if _, err := log.Append(int64(i), 0, ev); err != nil {
			t.Fatal(err)
		}
	}
	get, err := svc.GetSession(t.Context(), &v1.GetSessionRequest{Uid: uid})
	if err != nil {
		t.Fatal(err)
	}
	if get.PendingApproval.GetRequestSeq() != 3 || get.ExecState != v1.ExecState_EXEC_AWAITING {
		t.Fatalf("pending=%v", get)
	}
	assertCurrent("Get", get)
	page, err := svc.ListSessions(t.Context(), &v1.ListSessionsRequest{})
	if err != nil || len(page.GetSessions()) != 1 {
		t.Fatalf("List=%v %v", page, err)
	}
	assertCurrent("List", page.Sessions[0])
	resumed, err := svc.Resume(t.Context(), &v1.ResumeRequest{Session: uid})
	if err != nil {
		t.Fatal(err)
	}
	assertCurrent("Resume", resumed)
	stream := &failingApprovalStream{ctx: t.Context()}
	if err := svc.Exec(&v1.ExecRequest{Session: uid}, stream); err != nil {
		t.Fatal(err)
	}
	var count int
	for _, frame := range stream.frames {
		if sess := frame.GetSession(); sess != nil {
			assertCurrent("Exec", sess)
			count++
		}
	}
	if count != 2 {
		t.Fatalf("Exec initial/final session frames=%d", count)
	}
	children, err := svc.Fork(t.Context(), &v1.ForkRequest{Session: uid, AtSeq: 3, Count: 1})
	if err != nil || len(children.GetChildren()) != 1 {
		t.Fatalf("Fork=%v %v", children, err)
	}
	child := children.Children[0]
	if child.PendingApproval != nil || child.ExecState != v1.ExecState_EXEC_COMPLETED {
		t.Fatalf("inherited fork child=%v", child)
	}
	assertCurrent("Fork", child)
	suspended, err := svc.Suspend(t.Context(), &v1.SuspendRequest{Session: uid})
	if err != nil {
		t.Fatal(err)
	}
	assertCurrent("Suspend", suspended)
	approved, err := svc.Approve(t.Context(), &v1.ApproveRequest{Session: uid, ExecutionId: "execution", ToolCallId: "call", RequestSeq: 3, Approved: proto.Bool(false)})
	if err != nil {
		t.Fatal(err)
	}
	if approved.Session.PendingApproval != nil || approved.Session.ExecState != v1.ExecState_EXEC_COMPLETED {
		t.Fatalf("decision projection=%v", approved)
	}
	assertCurrent("Approve", approved.Session)
}
