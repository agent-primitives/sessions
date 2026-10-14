package session

import (
	"testing"

	"github.com/aramase/agentsessions/api"
	v1 "github.com/aramase/agentsessions/api/genpb"
	"github.com/aramase/agentsessions/controller"
	"github.com/aramase/agentsessions/sqlitelog"
	"google.golang.org/protobuf/proto"
)

// Rendering must use first-seen execution ownership, not the gated call's sequence.
// Late older records cannot select a different execution. Invalid gated histories withhold
// references just like strict inspection; malformed nongated histories do not.
func TestApprovalReportingOwnershipAndInterleaving(t *testing.T) {
	start := func(id string) api.Event {
		return api.Event{ExecutionID: id, Kind: api.EventExecutionStart, ExecutionStart: &api.ExecutionStart{InputCount: proto.Int64(0)}}
	}
	call := func(id string) api.Event {
		return api.Event{ExecutionID: id, Kind: api.EventToolCall, ToolCall: &api.ToolCall{ID: "call", IdempotencyKey: "key", Mediation: api.MediationRequiresApproval}}
	}
	request := func(id string) api.Event {
		return api.Event{ExecutionID: id, Kind: api.EventApprovalRequest, Approval: &api.ApprovalRequest{ToolCallID: "call"}}
	}
	fork := api.Event{Kind: api.EventLifecycle, Lifecycle: &api.Lifecycle{Kind: api.LifecycleFork}}
	for _, tc := range []struct {
		name   string
		events []api.Event
		seq    int64
		id     string
	}{
		{"late inherited call", []api.Event{start("inherited"), fork, call("inherited"), request("inherited")}, 0, ""},
		{"new child execution", []api.Event{start("inherited"), fork, start("own"), call("own"), request("own")}, 5, "own"},
		{"nested fork excludes child execution", []api.Event{start("parent"), fork, start("child"), fork, call("child"), request("child")}, 0, ""},
		{"new nested execution", []api.Event{start("parent"), fork, start("child"), fork, start("grandchild"), call("grandchild"), request("grandchild")}, 7, "grandchild"},
		{"late old malformed gated evidence", []api.Event{start("old"), start("latest"), call("latest"), request("latest"), request("old")}, 0, ""},
		{"malformed nongated history", []api.Event{{ExecutionID: "old", Kind: api.EventExecutionStart}, start("latest"), call("latest"), request("latest")}, 4, "latest"},
		{"late old valid gate", []api.Event{start("old"), start("latest"), call("latest"), request("latest"), call("old"), request("old")}, 4, "latest"},
		{"late old matched decision", []api.Event{start("old"), call("old"), request("old"), start("latest"), call("latest"), request("latest"), {ExecutionID: "old", Kind: api.EventApprovalResult, ApprovalResult: &api.ApprovalResult{ToolCallID: "call", RequestSeq: 3, Approved: true}}}, 6, "latest"},
		{"late old mismatched decision", []api.Event{start("old"), call("old"), request("old"), start("latest"), call("latest"), request("latest"), {ExecutionID: "old", Kind: api.EventApprovalResult, ApprovalResult: &api.ApprovalResult{ToolCallID: "call", RequestSeq: 6, Approved: true}}}, 0, ""},
		{"new ordinary execution clears", []api.Event{start("old"), call("old"), request("old"), start("latest")}, 0, ""},
		{"error keeps awaiting", []api.Event{start("own"), call("own"), request("own"), {ExecutionID: "own", Kind: api.EventError, Err: &api.Error{Description: "audit"}}}, 3, "own"},
		{"suspend keeps awaiting", []api.Event{start("own"), call("own"), request("own"), {Kind: api.EventLifecycle, Lifecycle: &api.Lifecycle{Kind: api.LifecycleSuspend}}}, 3, "own"},
		{"end suppresses reference", []api.Event{start("own"), call("own"), request("own"), {ExecutionID: "own", Kind: api.EventEnd, End: &api.HarnessEnd{State: "COMPLETED"}}}, 0, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, err := sqlitelog.Open(":memory:")
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			log := store.Session("s")
			for i, ev := range tc.events {
				if _, err := log.Append(int64(i), 0, ev); err != nil {
					t.Fatalf("permissive append %d: %v", i, err)
				}
			}
			svc := NewService(store, nil)
			got, err := svc.GetSession(t.Context(), &v1.GetSessionRequest{Uid: "s"})
			if err != nil {
				t.Fatal(err)
			}
			wantState := v1.ExecState_EXEC_COMPLETED
			var wantRef *v1.ApprovalRef
			if tc.seq > 0 {
				wantState = v1.ExecState_EXEC_AWAITING
				wantRef = &v1.ApprovalRef{ExecutionId: tc.id, ToolCallId: "call", RequestSeq: tc.seq}
			}
			strict, strictErr := controller.InspectApproval(log, 0)
			if wantRef != nil && (strictErr != nil || strict == nil || strict.Request == nil || strict.Request.Seq != wantRef.RequestSeq) {
				t.Fatalf("reference unsupported by strict evidence: state=%+v err=%v", strict, strictErr)
			}
			if tc.name == "late old malformed gated evidence" && strictErr == nil {
				t.Fatal("fixture must fail strict inspection")
			}
			if got.LastSeq != int64(len(tc.events)) || got.ExecState != wantState || !proto.Equal(got.PendingApproval, wantRef) {
				t.Fatalf("Get=%v want state=%v ref=%v", got, wantState, wantRef)
			}
			info, err := store.SessionInfo("s")
			if err != nil {
				t.Fatal(err)
			}
			rendered, err := sessionProto(info)
			if err != nil || !proto.Equal(rendered, got) {
				t.Fatalf("common renderer=%v err=%v want=%v", rendered, err, got)
			}
			page, err := svc.ListSessions(t.Context(), &v1.ListSessionsRequest{})
			if err != nil || len(page.GetSessions()) != 1 || !proto.Equal(page.Sessions[0], got) {
				t.Fatalf("List=%v err=%v", page, err)
			}
		})
	}
}
