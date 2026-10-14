package sqlitelog

import (
	"testing"

	"github.com/aramase/agentsessions/api"
	"github.com/aramase/agentsessions/controller"
	"google.golang.org/protobuf/proto"
)

// Pin reporting safety against the existing strict scanner at every prefix, including
// histories that become invalid only when later evidence turns an ordinary execution gated.
func TestApprovalProjectionMatchesStrictEvidence(t *testing.T) {
	base := projectionEvents("execution")
	decision := api.Event{ExecutionID: "execution", Kind: api.EventApprovalResult, ApprovalResult: &api.ApprovalResult{ToolCallID: "call", RequestSeq: 3, Approved: true}}
	receipt := api.Event{ExecutionID: "execution", Kind: api.EventToolResult, Result: &api.ToolResult{ID: "call", ApprovalRequestSeq: 3, ApprovalDecisionSeq: 4}}
	for _, tc := range []struct {
		name   string
		events []api.Event
	}{
		{"success then second call", append(append(append([]api.Event{}, base...), decision, receipt), api.Event{ExecutionID: "execution", Kind: api.EventToolCall, ToolCall: &api.ToolCall{ID: "second", IdempotencyKey: "second-key", Mediation: api.MediationRequiresApproval}}, api.Event{ExecutionID: "execution", Kind: api.EventApprovalRequest, Approval: &api.ApprovalRequest{ToolCallID: "second"}})},
		{"duplicate request", append(append([]api.Event{}, base...), base[2])},
		{"duplicate decision", append(append([]api.Event{}, base...), decision, decision)},
		{"duplicate gated call ID", append(append([]api.Event{}, base...), decision, receipt, base[1])},
		{"unbound receipt before gate", append([]api.Event{receipt}, base...)},
		{"contradictory denied receipt", append(append([]api.Event{}, base...), api.Event{ExecutionID: "execution", Kind: api.EventApprovalResult, ApprovalResult: &api.ApprovalResult{ToolCallID: "call", RequestSeq: 3, Approved: false}}, receipt)},
		{"bad receipt request sequence", append(append([]api.Event{}, base...), decision, api.Event{ExecutionID: "execution", Kind: api.EventToolResult, Result: &api.ToolResult{ID: "call", ApprovalRequestSeq: 2, ApprovalDecisionSeq: 4}})},
		{"invalid nongated becomes gated", append([]api.Event{{ExecutionID: "old", Kind: api.EventExecutionStart}}, append(base, api.Event{ExecutionID: "old", Kind: api.EventApprovalRequest})...)},
		{"missing input payload", append([]api.Event{{ExecutionID: "execution", Kind: api.EventExecutionStart, ExecutionStart: &api.ExecutionStart{InputCount: proto.Int64(1)}}, {ExecutionID: "execution", Kind: api.EventInput}}, base[1:]...)},
		{"missing input count", append([]api.Event{{ExecutionID: "execution", Kind: api.EventExecutionStart, ExecutionStart: &api.ExecutionStart{}}}, base[1:]...)},
		{"negative input count", append([]api.Event{{ExecutionID: "execution", Kind: api.EventExecutionStart, ExecutionStart: &api.ExecutionStart{InputCount: proto.Int64(-1)}}}, base[1:]...)},
		{"duplicate start before gate", append([]api.Event{base[0]}, base...)},
		{"missing tool call payload", append([]api.Event{base[0], {ExecutionID: "execution", Kind: api.EventToolCall}}, base[1:]...)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, err := Open(":memory:")
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			log := s.Session("s")
			for i, ev := range tc.events {
				if _, err := log.Append(int64(i), 0, ev); err != nil {
					t.Fatalf("malformed append %d refused: %v", i, err)
				}
				strict, strictErr := controller.InspectApproval(log, 0)
				want := api.ApprovalRef{}
				if strictErr == nil && strict != nil && !strict.Inherited && !strict.Completed && strict.Request != nil && strict.Decision == nil {
					want = api.ApprovalRef{ExecutionID: strict.ExecutionID, ToolCallID: strict.Call.ID, RequestSeq: strict.Request.Seq}
				}
				info, err := s.SessionInfo("s")
				if err != nil || info.PendingApproval != want {
					t.Fatalf("prefix %d projection=%+v want=%+v strict=%v query=%v", i+1, info.PendingApproval, want, strictErr, err)
				}
			}
		})
	}
}

func TestApprovalProjectionLateInputCompletenessRecovery(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	log := s.Session("s")
	old := projectionEvents("old")
	old[0].ExecutionStart.InputCount = proto.Int64(1)
	events := append(old, api.Event{ExecutionID: "old", Kind: api.EventApprovalResult, ApprovalResult: &api.ApprovalResult{ToolCallID: "call", RequestSeq: 3, Approved: false}}, api.Event{ExecutionID: "old", Kind: api.EventToolResult, Result: &api.ToolResult{ID: "call", ApprovalRequestSeq: 3, ApprovalDecisionSeq: 4, IsError: true, Code: api.ToolResultCodeApprovalDenied}})
	events = append(events, projectionEvents("latest")...)
	for i, ev := range events {
		if _, err := log.Append(int64(i), 0, ev); err != nil {
			t.Fatal(err)
		}
	}
	info, err := s.SessionInfo("s")
	if err != nil || info.PendingApproval.RequestSeq != 0 {
		t.Fatalf("incomplete old gated evidence=%+v %v", info, err)
	}
	if _, err := controller.InspectApproval(log, 0); err == nil {
		t.Fatal("fixture must be incomplete")
	}
	if _, err := log.Append(8, 0, api.Event{ExecutionID: "old", Kind: api.EventInput, Message: api.TextMessage("user", "late input")}); err != nil {
		t.Fatal(err)
	}
	strict, err := controller.InspectApproval(log, 0)
	if err != nil || strict.Request.Seq != 8 {
		t.Fatalf("repaired strict evidence=%+v %v", strict, err)
	}
	info, err = s.SessionInfo("s")
	if err != nil || info.PendingApproval.RequestSeq != 8 {
		t.Fatalf("completeness recovery failed=%+v %v", info, err)
	}
}
