package session

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/aramase/agentsessions/api"
	v1 "github.com/aramase/agentsessions/api/genpb"
	"github.com/aramase/agentsessions/sqlitelog"
	"google.golang.org/protobuf/proto"
)

// Persisted reporting must survive restart and each keyset page must retain its own
// correlation; a query-only host needs no runtime/harness to render any of these rows.
func TestApprovalProjectionGetListRestartAndPagination(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.db")
	store, err := sqlitelog.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	for i, uid := range []string{"a", "b", "c"} {
		if err := store.PutSession(sqlitelog.SessionMeta{UID: uid, Project: "approvals", CreatedAt: time.Unix(1700000000+int64(i), 0)}); err != nil {
			t.Fatal(err)
		}
		for n, ev := range []api.Event{
			{ExecutionID: uid, Kind: api.EventExecutionStart, ExecutionStart: &api.ExecutionStart{InputCount: proto.Int64(0)}},
			{ExecutionID: uid, Kind: api.EventToolCall, ToolCall: &api.ToolCall{ID: "call", IdempotencyKey: "key", Mediation: api.MediationRequiresApproval}},
			{ExecutionID: uid, Kind: api.EventApprovalRequest, Approval: &api.ApprovalRequest{ToolCallID: "call"}},
		} {
			if _, err := store.Session(uid).Append(int64(n), 0, ev); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = sqlitelog.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	svc := NewService(store, nil)
	assertPending := func(got *v1.Session, uid string) {
		t.Helper()
		if got.GetMetadata().GetUid() != uid || got.LastSeq != 3 || got.ExecState != v1.ExecState_EXEC_AWAITING || !proto.Equal(got.PendingApproval, &v1.ApprovalRef{ExecutionId: uid, ToolCallId: "call", RequestSeq: 3}) {
			t.Fatalf("reopened projection=%v want uid=%s", got, uid)
		}
	}
	for _, uid := range []string{"a", "b", "c"} {
		got, err := svc.GetSession(t.Context(), &v1.GetSessionRequest{Uid: uid})
		if err != nil {
			t.Fatal(err)
		}
		assertPending(got, uid)
	}
	first, err := svc.ListSessions(t.Context(), &v1.ListSessionsRequest{Project: "approvals", PageSize: 2})
	if err != nil || len(first.GetSessions()) != 2 || first.GetNextPageToken() == "" {
		t.Fatalf("first page=%v %v", first, err)
	}
	assertPending(first.Sessions[0], "c")
	assertPending(first.Sessions[1], "b")
	second, err := svc.ListSessions(t.Context(), &v1.ListSessionsRequest{Project: "approvals", PageSize: 2, PageToken: first.NextPageToken})
	if err != nil || len(second.GetSessions()) != 1 || second.GetNextPageToken() != "" {
		t.Fatalf("second page=%v %v", second, err)
	}
	assertPending(second.Sessions[0], "a")
}
