package conformance_test

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/aramase/agentsessions/api"
	v1 "github.com/aramase/agentsessions/api/genpb"
	"github.com/aramase/agentsessions/harnesswire"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

// This peer implements only the protobuf contract. Assertions use normative wire
// symbols and durable journal evidence, not Go park/control error sentinels.
type approvalWirePeer struct {
	v1.UnimplementedHarnessServer
	frames        chan []*v1.ControllerFrame
	wrongAck      bool
	parkRejection chan codes.Code
}

func (*approvalWirePeer) Describe(context.Context, *v1.DescribeRequest) (*v1.HarnessDescriptor, error) {
	return &v1.HarnessDescriptor{Id: "proto-gate", Version: "1", Capabilities: &v1.Capabilities{Resumability: v1.Resumability_RESUMABILITY_STATELESS_REPLAY}, Tools: []*v1.ToolSpec{{Name: "charge", Mediation: v1.Mediation_MEDIATION_REQUIRES_APPROVAL}}}, nil
}
func (p *approvalWirePeer) Connect(s v1.Harness_ConnectServer) error {
	start, err := s.Recv()
	if err != nil {
		return err
	}
	if start.GetStart() == nil || start.GetSession() != "session" || start.GetExecutionId() == "" {
		return errors.New("invalid Start envelope")
	}
	exec := start.GetExecutionId()
	if err := s.Send(&v1.Event{ExecutionId: exec, Kind: v1.EventKind_EVENT_TOOL_CALL, Body: &v1.Event_Tool{Tool: &v1.ToolCall{Id: "call", Tool: "charge", IdempotencyKey: "key", Mediation: v1.Mediation_MEDIATION_REQUIRES_APPROVAL}}}); err != nil {
		return err
	}
	control, err := s.Recv()
	if err != nil {
		return err
	}
	if control.GetSession() != start.GetSession() || control.GetExecutionId() != exec {
		return errors.New("foreign control envelope")
	}
	if park := control.GetPark(); park != nil {
		if park.GetExecutionId() != exec || park.GetToolCallId() != "call" || park.GetRequestSeq() <= 0 {
			return errors.New("invalid park tuple")
		}
		p.frames <- []*v1.ControllerFrame{control}
		ack := &v1.ApprovalRef{ExecutionId: exec, ToolCallId: "call", RequestSeq: park.GetRequestSeq()}
		if p.wrongAck {
			ack.ToolCallId = "foreign"
		}
		if err := s.Send(&v1.Event{ExecutionId: exec, Kind: v1.EventKind_EVENT_PARKED, Body: &v1.Event_Parked{Parked: ack}}); err != nil {
			return err
		}
		if p.parkRejection != nil {
			// Hold EOF so the raw peer can distinguish rejection/cancellation from
			// a successful park, whose host would still be awaiting clean closure.
			_, err := s.Recv()
			p.parkRejection <- status.Code(err)
			return err
		}
		return nil // clean closure without END
	}
	decision := control.GetApproval()
	if decision == nil || decision.GetToolCallId() != "call" || decision.GetRequestSeq() <= 0 {
		return errors.New("expected approval before tool")
	}
	receipt, err := s.Recv()
	if err != nil {
		return err
	}
	tr := receipt.GetTool()
	if receipt.GetSession() != start.GetSession() || receipt.GetExecutionId() != exec || tr == nil || tr.GetId() != "call" || tr.GetApprovalRequestSeq() != decision.GetRequestSeq() || tr.GetApprovalDecisionSeq() <= 0 {
		return errors.New("invalid receipt tuple")
	}
	if !decision.GetApproved() {
		if tr.GetCode() != v1.ToolResult_APPROVAL_DENIED || !tr.GetIsError() {
			return errors.New("invalid denial status")
		}
	} else if (tr.GetCode() != v1.ToolResult_CODE_UNSPECIFIED || tr.GetIsError()) && (tr.GetCode() != v1.ToolResult_EXECUTOR_ERROR || !tr.GetIsError()) {
		return errors.New("invalid approval status")
	}
	p.frames <- []*v1.ControllerFrame{control, receipt}
	if err := s.Send(&v1.Event{ExecutionId: exec, Kind: v1.EventKind_EVENT_OUTPUT, Body: &v1.Event_Message{Message: &v1.Message{Role: "assistant", Parts: []*v1.Part{{Part: &v1.Part_Text{Text: &v1.TextPart{Text: "continued"}}}}}}}); err != nil {
		return err
	}
	return s.Send(&v1.Event{ExecutionId: exec, Kind: v1.EventKind_EVENT_END, Body: &v1.Event_End{End: &v1.HarnessEnd{State: "COMPLETED"}}})
}
func rawApprovalHarness(t *testing.T, p *approvalWirePeer) api.Harness {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	server := grpc.NewServer()
	v1.RegisterHarnessServer(server, p)
	go server.Serve(lis)
	t.Cleanup(server.Stop)
	conn, err := grpc.NewClient("passthrough:///approval-conformance", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close(); lis.Close() })
	return harnesswire.NewClientHarness(v1.NewHarnessClient(conn))
}

type approvalHost struct {
	result  api.ToolResult
	err     error
	outputs int
}

func (*approvalHost) Model(context.Context, api.ModelRequest) (api.ModelResponse, error) {
	return api.ModelResponse{}, nil
}
func (h *approvalHost) Output(context.Context, string) error { h.outputs++; return nil }
func (h *approvalHost) ToolCall(context.Context, api.ToolCall) (api.ToolResult, error) {
	return h.result, h.err
}
func (*approvalHost) Report(context.Context, api.ToolResult) error { return nil }
func (*approvalHost) Usage(context.Context, api.Usage) error       { return nil }

func TestApprovalRawProtocol(t *testing.T) {
	for _, shape := range []struct {
		name     string
		approved bool
		code     api.ToolResultCode
		isError  bool
	}{
		{"approved", true, api.ToolResultCodeUnspecified, false}, {"denied", false, api.ToolResultCodeApprovalDenied, true}, {"executor failure", true, api.ToolResultCodeExecutorError, true},
	} {
		t.Run(shape.name, func(t *testing.T) {
			peer := &approvalWirePeer{frames: make(chan []*v1.ControllerFrame, 1)}
			host := &approvalHost{result: api.ToolResult{ID: "call", ApprovalRequestSeq: 3, ApprovalDecisionSeq: 4, Code: shape.code, IsError: shape.isError, Approval: &api.ApprovalResult{ToolCallID: "call", RequestSeq: 3, Approved: shape.approved}}}
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			if err := rawApprovalHarness(t, peer).Run(ctx, &api.Start{SessionUID: "session", ExecutionID: "e"}, host); err != nil {
				t.Fatal(err)
			}
			frames := <-peer.frames
			if len(frames) != 2 || frames[0].GetApproval().GetApproved() != shape.approved || frames[1].GetTool().GetCode() != v1.ToolResult_Code(map[api.ToolResultCode]int32{api.ToolResultCodeUnspecified: 0, api.ToolResultCodeApprovalDenied: 1, api.ToolResultCodeExecutorError: 2}[shape.code]) || host.outputs != 1 {
				t.Fatalf("frames=%v outputs=%d", frames, host.outputs)
			}
		})
	}
	for _, wrong := range []bool{false, true} {
		t.Run(map[bool]string{false: "ACK EOF", true: "wrong ACK cancels"}[wrong], func(t *testing.T) {
			peer := &approvalWirePeer{frames: make(chan []*v1.ControllerFrame, 1), wrongAck: wrong}
			if wrong {
				peer.parkRejection = make(chan codes.Code, 1)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			host := &approvalHost{err: &api.ApprovalParkedError{Ref: api.ApprovalRef{ExecutionID: "e", ToolCallID: "call", RequestSeq: 3}}}
			err := rawApprovalHarness(t, peer).Run(ctx, &api.Start{SessionUID: "session", ExecutionID: "e"}, host)
			if wrong {
				if err == nil || errors.Is(err, api.ErrApprovalParked) {
					t.Fatalf("bad ACK accepted %v", err)
				}
				select {
				case code := <-peer.parkRejection:
					if code != codes.Canceled {
						t.Fatal(code)
					}
				case <-ctx.Done():
					t.Fatal("peer not canceled")
				}
			} else if !errors.Is(err, api.ErrApprovalParked) {
				t.Fatal(err)
			}
			if host.outputs != 0 {
				t.Fatal("park produced output")
			}
		})
	}
}
