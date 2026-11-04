package conformance_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/agent-primitives/sessions/api"
	v1 "github.com/agent-primitives/sessions/api/genpb"
	"github.com/agent-primitives/sessions/controller"
	"github.com/agent-primitives/sessions/eventlog"
)

// APR-18/APR-19: after CloseApproval, Resume over the harness wire serves the closed gate to a
// harness that re-issues the call as an approval frame and a TOOL_RESULT frame carrying the
// close receipt code, without calling the executor. An undecided gate is served as a host
// denial with the fixed reason; a denied gate keeps its decision; an approved gate keeps its
// approval with an OUTCOME_UNKNOWN receipt.
func TestApprovalCloseWire(t *testing.T) {
	for _, tc := range []struct {
		state    string
		approved bool
		code     v1.ToolResult_Code
	}{
		{"undecided", false, v1.ToolResult_NOT_EXECUTED},
		{"denied", false, v1.ToolResult_NOT_EXECUTED},
		{"approved", true, v1.ToolResult_OUTCOME_UNKNOWN},
	} {
		t.Run(tc.state, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			log := eventlog.AsStore(eventlog.New())
			peer := &approvalWirePeer{frames: make(chan []*v1.ControllerFrame, 1)}
			harness := rawApprovalHarness(t, peer)
			executor := controller.WithToolExecutor(func(context.Context, controller.ToolCallContext, api.ToolCall) (api.ToolResult, error) {
				t.Error("executor called for a closed gate")
				return api.ToolResult{}, errors.New("unexpected executor call")
			})
			c, err := controller.New(log, nil, controller.WithSessionUID("session"), executor)
			if err != nil {
				t.Fatal(err)
			}
			if err := c.Exec(ctx, harness, []api.Message{*api.TextMessage("user", "charge")}, 0); !errors.Is(err, api.ErrApprovalParked) {
				t.Fatalf("Exec = %v, want a parked gate", err)
			}
			<-peer.frames
			state, err := controller.InspectApproval(log, 0)
			if err != nil || state == nil || state.Request == nil {
				t.Fatalf("InspectApproval = %+v, %v", state, err)
			}
			ref := api.ApprovalRef{ExecutionID: state.ExecutionID, ToolCallID: state.Call.ID, RequestSeq: state.Request.Seq}
			if tc.state != "undecided" {
				if _, err := controller.Approve(log, api.ApprovalDecision{ExecutionID: ref.ExecutionID, ToolCallID: ref.ToolCallID, RequestSeq: ref.RequestSeq, Approved: tc.approved}); err != nil {
					t.Fatal(err)
				}
			}
			closer := api.CallerIdentity{Issuer: "https://issuer.example", Subject: "owner"}
			decision, receipt, err := controller.CloseApproval(log, ref, closer)
			if err != nil {
				t.Fatal(err)
			}

			r, err := controller.New(log, nil, controller.WithSessionUID("session"), executor)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := r.Resume(ctx, harness); err != nil {
				t.Fatalf("Resume = %v", err)
			}
			frames := <-peer.frames
			approval, tool := frames[0].GetApproval(), frames[1].GetTool()
			if approval.GetApproved() != tc.approved || approval.GetRequestSeq() != ref.RequestSeq {
				t.Fatalf("approval frame = %v", approval)
			}
			// The closer is host audit state: journaled on the denial, never sent to a harness.
			if approval.GetVerifiedApprover() != nil {
				t.Fatalf("approval frame carries the verified approver: %v", approval)
			}
			if tc.state == "undecided" && (approval.GetReason() != controller.ApprovalCloseReason ||
				decision.Event.ApprovalResult.VerifiedApprover != closer) {
				t.Fatalf("undecided close frame = %v, journaled %+v; want the closer's denial", approval, decision.Event.ApprovalResult)
			}
			if tool.GetCode() != tc.code || !tool.GetIsError() || tool.GetApprovalRequestSeq() != ref.RequestSeq ||
				tool.GetApprovalDecisionSeq() != decision.Seq || receipt.Event.Result.Code != api.ToolResultCode(tc.code) {
				t.Fatalf("receipt frame = %v, journaled %+v", tool, receipt.Event.Result)
			}
			if err := controller.VerifyJournal(log); err != nil {
				t.Fatalf("VerifyJournal: %v", err)
			}
		})
	}
}
