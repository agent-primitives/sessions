package harnesswire

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/aramase/agentsessions/api"
	v1 "github.com/aramase/agentsessions/api/genpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

// Transport tables traverse real gRPC Connect; lifecycle tables isolate pump and sink barriers.
type connectPeer struct {
	v1.UnimplementedHarnessServer
	run func(v1.Harness_ConnectServer) error
}

func (p connectPeer) Connect(s v1.Harness_ConnectServer) error { return p.run(s) }

func approvalClient(t *testing.T, server v1.HarnessServer) v1.HarnessClient {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	gs := grpc.NewServer()
	v1.RegisterHarnessServer(gs, server)
	go gs.Serve(lis)
	t.Cleanup(gs.Stop)
	conn, err := grpc.NewClient("passthrough:///approval", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close(); lis.Close() })
	return v1.NewHarnessClient(conn)
}
func approvalContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	t.Cleanup(cancel)
	return ctx
}

type gateHarness struct {
	run func(context.Context, *api.Start, api.EventSink) error
}

func (gateHarness) Describe(context.Context) (api.Descriptor, error) {
	return api.Descriptor{ID: "gate"}, nil
}
func (h gateHarness) Run(ctx context.Context, start *api.Start, sink api.EventSink) error {
	return h.run(ctx, start, sink)
}

var gateCall = api.ToolCall{ID: "call", Tool: "charge", Mediation: api.MediationRequiresApproval}
var gateRef = api.ApprovalRef{ExecutionID: "exec", ToolCallID: "call", RequestSeq: 17}

type gateHost struct {
	hostSink
	result  api.ToolResult
	err     error
	outputs []string
	reports int
	calls   int
}

func (h *gateHost) ToolCall(context.Context, api.ToolCall) (api.ToolResult, error) {
	h.calls++
	return h.result, h.err
}
func (h *gateHost) Output(_ context.Context, s string) error {
	h.outputs = append(h.outputs, s)
	return nil
}
func (h *gateHost) Report(context.Context, api.ToolResult) error { h.reports++; return nil }

func gateToolEvent() *v1.Event {
	return &v1.Event{ExecutionId: "exec", Kind: v1.EventKind_EVENT_TOOL_CALL, Body: &v1.Event_Tool{Tool: &v1.ToolCall{Id: "call", Tool: "charge", Mediation: v1.Mediation_MEDIATION_REQUIRES_APPROVAL}}}
}
func parkFrame() *v1.ControllerFrame {
	return &v1.ControllerFrame{Session: "session", ExecutionId: "exec", Frame: &v1.ControllerFrame_Park{Park: &v1.ApprovalRef{ExecutionId: "exec", ToolCallId: "call", RequestSeq: 17}}}
}
func ackEvent() *v1.Event {
	return &v1.Event{ExecutionId: "exec", Kind: v1.EventKind_EVENT_PARKED, Body: &v1.Event_Parked{Parked: &v1.ApprovalRef{ExecutionId: "exec", ToolCallId: "call", RequestSeq: 17}}}
}
func decisionFrame(approved bool) *v1.ControllerFrame {
	return &v1.ControllerFrame{Session: "session", ExecutionId: "exec", Frame: &v1.ControllerFrame_Approval{Approval: &v1.ApprovalResult{ToolCallId: "call", RequestSeq: 17, Approved: approved, Reason: "reviewed"}}}
}
func receiptFrame() *v1.ControllerFrame {
	return &v1.ControllerFrame{Session: "session", ExecutionId: "exec", Frame: &v1.ControllerFrame_Tool{Tool: &v1.ToolResult{Id: "call", ApprovalRequestSeq: 17, ApprovalDecisionSeq: 19}}}
}
func openGate(t *testing.T, client v1.HarnessClient, session string) v1.Harness_ConnectClient {
	t.Helper()
	s, err := client.Connect(approvalContext(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Send(&v1.ControllerFrame{Session: session, ExecutionId: "exec", Frame: &v1.ControllerFrame_Start{Start: &v1.Start{}}}); err != nil {
		t.Fatal(err)
	}
	ev, err := s.Recv()
	if err != nil || ev.GetKind() != v1.EventKind_EVENT_TOOL_CALL || ev.GetTool().GetId() != "call" {
		t.Fatalf("tool event = %v, %v", ev, err)
	}
	return s
}

func TestApprovalParkServerAckAndEOF(t *testing.T) {
	for _, swallow := range []bool{false, true} {
		t.Run(fmt.Sprint("swallow=", swallow), func(t *testing.T) {
			late := make(chan []error, 1)
			server := NewServer(gateHarness{run: func(ctx context.Context, _ *api.Start, sink api.EventSink) error {
				_, err := sink.ToolCall(ctx, gateCall)
				if !swallow {
					return err
				}
				_, modelErr := sink.Model(ctx, api.ModelRequest{Model: "m"})
				_, toolErr := sink.ToolCall(ctx, gateCall)
				late <- []error{sink.Output(ctx, "late"), modelErr, toolErr, sink.Report(ctx, api.ToolResult{ID: "ordinary"}), sink.Usage(ctx, api.Usage{Model: "m"})}
				return nil
			}})
			stream := openGate(t, approvalClient(t, server), "session")
			if err := stream.Send(parkFrame()); err != nil {
				t.Fatal(err)
			}
			ev, err := stream.Recv()
			if err != nil || ev.GetKind() != v1.EventKind_EVENT_PARKED || ev.GetExecutionId() != "exec" || ev.GetParked().GetExecutionId() != "exec" || ev.GetParked().GetToolCallId() != "call" || ev.GetParked().GetRequestSeq() != 17 {
				t.Fatalf("park ack = %v, %v", ev, err)
			}
			if ev, err := stream.Recv(); err != io.EOF {
				t.Fatalf("after parked = %v, %v; want actual EOF, no END/effect", ev, err)
			}
			if swallow {
				for _, err := range <-late {
					if !errors.Is(err, api.ErrApprovalParked) {
						t.Fatalf("late operation = %v, want sealed park", err)
					}
				}
			}
		})
	}
}

func TestApprovalParkDistinctRunFailure(t *testing.T) {
	for _, joined := range []bool{false, true} {
		t.Run(fmt.Sprint("joined=", joined), func(t *testing.T) {
			server := NewServer(gateHarness{run: func(ctx context.Context, _ *api.Start, sink api.EventSink) error {
				_, parked := sink.ToolCall(ctx, gateCall)
				failure := status.Error(codes.Unavailable, "harness unwind failed")
				if joined {
					return errors.Join(parked, failure)
				}
				return failure
			}})
			stream := openGate(t, approvalClient(t, server), "session")
			if err := stream.Send(parkFrame()); err != nil {
				t.Fatal(err)
			}
			if ev, err := stream.Recv(); status.Code(err) != codes.Unavailable {
				t.Fatalf("failed handoff = %v, %v; want Unavailable, no parked success", ev, err)
			}
		})
	}
}

func TestApprovalServerRejectsMalformedFrames(t *testing.T) {
	// Each row owns fresh frames; receipt rows retain the approval-first barrier.
	parkWith := func(change func(*v1.ControllerFrame)) []*v1.ControllerFrame {
		frame := parkFrame()
		change(frame)
		return []*v1.ControllerFrame{frame}
	}
	approvalWith := func(change func(*v1.ControllerFrame)) []*v1.ControllerFrame {
		frame := decisionFrame(true)
		change(frame)
		return []*v1.ControllerFrame{frame}
	}
	receiptWith := func(change func(*v1.ControllerFrame)) []*v1.ControllerFrame {
		frame := receiptFrame()
		change(frame)
		return []*v1.ControllerFrame{decisionFrame(true), frame}
	}
	tests := []struct {
		name    string
		session string
		frames  []*v1.ControllerFrame
		want    string
	}{
		{"wrong park session", "session", parkWith(func(p *v1.ControllerFrame) { p.Session = "foreign" }), "session"},
		{"empty park session", "session", parkWith(func(p *v1.ControllerFrame) { p.Session = "" }), "session"},
		{"wrong park envelope execution", "session", parkWith(func(p *v1.ControllerFrame) { p.ExecutionId = "foreign" }), "execution_id"},
		{"wrong park body execution", "session", parkWith(func(p *v1.ControllerFrame) { p.GetPark().ExecutionId = "foreign" }), "park"},
		{"wrong park call", "session", parkWith(func(p *v1.ControllerFrame) { p.GetPark().ToolCallId = "foreign" }), "park"},
		{"zero park request", "session", parkWith(func(p *v1.ControllerFrame) { p.GetPark().RequestSeq = 0 }), "park"},
		{"bare park", "session", parkWith(func(p *v1.ControllerFrame) { p.Frame = &v1.ControllerFrame_Park{} }), "park"},
		{"tool before approval", "session", []*v1.ControllerFrame{receiptFrame()}, "approval"},
		{"wrong approval session", "session", approvalWith(func(p *v1.ControllerFrame) { p.Session = "foreign" }), "session"},
		{"wrong approval call", "session", approvalWith(func(p *v1.ControllerFrame) { p.GetApproval().ToolCallId = "foreign" }), "approval"},
		{"zero approval request", "session", approvalWith(func(p *v1.ControllerFrame) { p.GetApproval().RequestSeq = 0 }), "approval"},
		{"bare approval", "session", approvalWith(func(p *v1.ControllerFrame) { p.Frame = &v1.ControllerFrame_Approval{} }), "approval"},
		{"duplicate approval", "session", []*v1.ControllerFrame{decisionFrame(true), decisionFrame(true)}, "ToolResult"},
		{"conflicting approval", "session", []*v1.ControllerFrame{decisionFrame(true), decisionFrame(false)}, "ToolResult"},
		{"park after approval", "session", []*v1.ControllerFrame{decisionFrame(true), parkFrame()}, "ToolResult"},
		{"wrong receipt session", "session", receiptWith(func(p *v1.ControllerFrame) { p.Session = "foreign" }), "session"},
		{"wrong receipt execution", "session", receiptWith(func(p *v1.ControllerFrame) { p.ExecutionId = "foreign" }), "execution_id"},
		{"wrong receipt call", "session", receiptWith(func(p *v1.ControllerFrame) { p.GetTool().Id = "foreign" }), "correlation"},
		{"wrong receipt request", "session", receiptWith(func(p *v1.ControllerFrame) { p.GetTool().ApprovalRequestSeq = 18 }), "receipt"},
		{"zero receipt decision", "session", receiptWith(func(p *v1.ControllerFrame) { p.GetTool().ApprovalDecisionSeq = 0 }), "receipt"},
		{"denied success", "session", []*v1.ControllerFrame{decisionFrame(false), receiptFrame()}, "status"},
		{"approved denial", "session", receiptWith(func(p *v1.ControllerFrame) {
			p.GetTool().Code = v1.ToolResult_APPROVAL_DENIED
			p.GetTool().IsError = true
		}), "status"},
		{"approved unclassified error", "session", receiptWith(func(p *v1.ControllerFrame) { p.GetTool().IsError = true }), "status"},
		{"executor code without error", "session", receiptWith(func(p *v1.ControllerFrame) { p.GetTool().Code = v1.ToolResult_EXECUTOR_ERROR }), "status"},
		{"unknown receipt code", "session", receiptWith(func(p *v1.ControllerFrame) { p.GetTool().Code = v1.ToolResult_Code(99) }), "status"},
		{"closure between frames", "session", []*v1.ControllerFrame{decisionFrame(true)}, "EOF"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			observed := make(chan error, 1)
			server := NewServer(gateHarness{run: func(ctx context.Context, _ *api.Start, sink api.EventSink) error {
				_, err := sink.ToolCall(ctx, gateCall)
				observed <- err
				return err
			}})
			stream := openGate(t, approvalClient(t, server), test.session)
			for _, frame := range test.frames {
				if err := stream.Send(frame); err != nil {
					t.Fatal(err)
				}
			}
			stream.CloseSend()
			// A failure END may carry diagnostics, but no parked or completed reply is legal.
			for {
				ev, err := stream.Recv()
				if err != nil {
					break
				}
				if ev.GetKind() == v1.EventKind_EVENT_PARKED || ev.GetKind() == v1.EventKind_EVENT_END && ev.GetEnd().GetState() == "COMPLETED" {
					t.Fatalf("invalid control accepted: %v", ev)
				}
			}
			err := <-observed
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("ToolCall = %v, want %s refusal", err, test.want)
			}
		})
	}
}

func TestApprovalReportAcceptsExecutorError(t *testing.T) {
	server := NewServer(gateHarness{run: func(ctx context.Context, _ *api.Start, sink api.EventSink) error {
		return sink.Report(ctx, api.ToolResult{ID: "ordinary", Code: api.ToolResultCodeExecutorError, IsError: true})
	}})
	host := &gateHost{}
	if err := NewClientHarness(approvalClient(t, server)).Run(approvalContext(t), &api.Start{SessionUID: "session", ExecutionID: "exec"}, host); err != nil {
		t.Fatal(err)
	}
	if host.reports != 1 {
		t.Fatalf("reports = %d, want 1", host.reports)
	}
}

func TestApprovalClientRejectsHostAuthorityForgery(t *testing.T) {
	tests := map[string]*v1.Event{
		"request":          {Kind: v1.EventKind_EVENT_APPROVAL_REQUEST, Body: &v1.Event_Approval{Approval: &v1.ApprovalRequest{ToolCallId: "call"}}},
		"decision":         {Kind: v1.EventKind_EVENT_APPROVAL_RESULT, Body: &v1.Event_ApprovalResult{ApprovalResult: &v1.ApprovalResult{ToolCallId: "call", RequestSeq: 17, Approved: true}}},
		"execution start":  {Kind: v1.EventKind_EVENT_EXECUTION_START},
		"unsolicited park": {Kind: v1.EventKind_EVENT_PARKED},
		"request receipt":  {Kind: v1.EventKind_EVENT_TOOL_RESULT, Body: &v1.Event_Result{Result: &v1.ToolResult{Id: "call", ApprovalRequestSeq: 17}}},
		"decision receipt": {Kind: v1.EventKind_EVENT_TOOL_RESULT, Body: &v1.Event_Result{Result: &v1.ToolResult{Id: "call", ApprovalDecisionSeq: 19}}},
		"denial receipt":   {Kind: v1.EventKind_EVENT_TOOL_RESULT, Body: &v1.Event_Result{Result: &v1.ToolResult{Id: "call", Code: v1.ToolResult_APPROVAL_DENIED, IsError: true}}},
		"unknown code":     {Kind: v1.EventKind_EVENT_TOOL_RESULT, Body: &v1.Event_Result{Result: &v1.ToolResult{Id: "call", Code: v1.ToolResult_Code(7)}}},
	}
	for name, event := range tests {
		t.Run(name, func(t *testing.T) {
			event.ExecutionId = "exec"
			peer := connectPeer{run: func(s v1.Harness_ConnectServer) error {
				if _, err := s.Recv(); err != nil {
					return err
				}
				if err := s.Send(event); err != nil {
					return err
				}
				return s.Send(&v1.Event{ExecutionId: "exec", Kind: v1.EventKind_EVENT_END, Body: &v1.Event_End{End: &v1.HarnessEnd{State: "COMPLETED"}}})
			}}
			host := &gateHost{}
			err := NewClientHarness(approvalClient(t, peer)).Run(approvalContext(t), &api.Start{SessionUID: "session", ExecutionID: "exec"}, host)
			if err == nil || !strings.Contains(err.Error(), "host-owned") {
				t.Fatalf("forgery = %v, want host-owned refusal", err)
			}
			if host.reports != 0 || host.calls != 0 {
				t.Fatalf("forgery reached host sink: %#v", host)
			}
		})
	}
}

type hostSink struct{}

func (*hostSink) Model(context.Context, api.ModelRequest) (api.ModelResponse, error) {
	return api.ModelResponse{}, nil
}
func (*hostSink) Output(context.Context, string) error { return nil }
func (*hostSink) ToolCall(context.Context, api.ToolCall) (api.ToolResult, error) {
	return api.ToolResult{}, nil
}
func (*hostSink) Report(context.Context, api.ToolResult) error { return nil }
func (*hostSink) Usage(context.Context, api.Usage) error       { return nil }

// Controlled Recv avoids goroutine-count heuristics: the second frame is known
// to be forwarded against a full result queue, and pump return is observable.
type pumpStream struct {
	v1.Harness_ConnectServer
	ctx      context.Context
	frames   chan *v1.ControllerFrame
	received chan struct{}
}

func (s *pumpStream) Context() context.Context { return s.ctx }
func (s *pumpStream) Recv() (*v1.ControllerFrame, error) {
	select {
	case frame, ok := <-s.frames:
		if !ok {
			return nil, io.EOF
		}
		s.received <- struct{}{}
		return frame, nil
	case <-s.ctx.Done():
		return nil, s.ctx.Err()
	}
}

func TestReceivePumpBackpressureShutdown(t *testing.T) {
	for _, mode := range []string{"handler return", "context canceled", "blocked Recv canceled"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			stream := &pumpStream{ctx: ctx, frames: make(chan *v1.ControllerFrame, 2), received: make(chan struct{}, 2)}
			results := make(chan *v1.ControllerFrame, 1)
			done, stopped := make(chan struct{}), make(chan struct{})
			if mode != "blocked Recv canceled" {
				stream.frames <- &v1.ControllerFrame{ExecutionId: "one"}
				stream.frames <- &v1.ControllerFrame{ExecutionId: "two"}
			}
			go func() { receiveFrames(stream, results, done); close(stopped) }()
			if mode != "blocked Recv canceled" {
				for range 2 {
					select {
					case <-stream.received:
					case <-time.After(time.Second):
						t.Fatal("pump did not receive the backpressured frame")
					}
				}
			}
			if mode == "handler return" {
				close(done)
			} else {
				cancel() // gRPC owns this cancellation when Connect returns.
			}
			select {
			case <-stopped:
			case <-time.After(time.Second):
				t.Fatal("receive pump leaked")
			}
		})
	}
}

// Ready frames must not beat cancellation or closure; cancellation retains priority.
func TestAwaitResultCanceledWithReadyFrame(t *testing.T) {
	for _, test := range []struct {
		name             string
		canceled, closed bool
		want             error
	}{
		{"canceled with ready frame", true, false, context.Canceled},
		{"closed with ready frame", false, true, errSinkClosed},
		{"canceled and closed", true, true, context.Canceled},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if test.canceled {
				cancel()
			}
			done := make(chan struct{})
			if test.closed {
				close(done)
			}
			for range 100 {
				results := make(chan *v1.ControllerFrame, 1)
				results <- receiptFrame()
				s := &streamSink{results: results, executionID: "exec", done: done}
				if _, err := s.awaitResult(ctx); !errors.Is(err, test.want) {
					t.Fatalf("await = %v, want %v", err, test.want)
				}
			}
		})
	}
}

func TestStreamSinkOperativeFailuresStaySticky(t *testing.T) {
	for _, cause := range []error{context.Canceled, context.DeadlineExceeded, io.EOF, errors.New("invalid control frame"), errors.Join(errSinkClosed, context.Canceled)} {
		s := &streamSink{}
		if err := s.fail(cause); !errors.Is(err, cause) || !errors.Is(s.failure, cause) {
			t.Fatalf("operative failure = %v, sticky cause = %v, want %v", err, s.failure, cause)
		}
		if err := s.check(t.Context()); !errors.Is(err, cause) {
			t.Fatalf("later effect refusal = %v, want original %v", err, cause)
		}
	}
}
func TestStreamSinkInvalidApprovalStaysSealed(t *testing.T) {
	foreign := parkFrame()
	foreign.Session = "foreign"
	for _, test := range []struct {
		name, session, callID string
		reply                 *v1.ControllerFrame
		wantSent              int
	}{
		{"foreign park session", "session", "call", foreign, 1},
		{"empty session before send", "", "call", parkFrame(), 0},
		{"empty call before send", "session", "", parkFrame(), 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			results := make(chan *v1.ControllerFrame, 1)
			results <- test.reply
			server := &fakeConnectServer{}
			sink := &streamSink{stream: server, results: results, executionID: "exec", session: test.session}
			_, err := sink.ToolCall(t.Context(), api.ToolCall{ID: test.callID, Mediation: api.MediationRequiresApproval})
			if err == nil {
				t.Fatal("invalid approval accepted")
			}
			if lateErr := sink.Output(t.Context(), "late"); lateErr != err {
				t.Fatalf("sticky refusal = %v, want %v", lateErr, err)
			}
			if len(server.sent) != test.wantSent || test.wantSent == 1 && server.sent[0].GetKind() != v1.EventKind_EVENT_TOOL_CALL {
				t.Fatalf("unexpected or late effect: %v", server.sent)
			}
		})
	}
}

type parkSendFailure struct {
	v1.Harness_ConnectClient
	cause error
}

func (s parkSendFailure) Send(*v1.ControllerFrame) error { return s.cause }
func (parkSendFailure) Recv() (*v1.Event, error)         { panic("Recv after failed park Send") }

func TestParkExchangePreservesSendFailure(t *testing.T) {
	cause := errors.New("transport write failed")
	err := exchangePark(t.Context(), parkSendFailure{cause: cause}, &api.Start{SessionUID: "session", ExecutionID: "exec"}, api.ApprovalRef{ExecutionID: "exec", ToolCallID: "call", RequestSeq: 17})
	if !errors.Is(err, cause) || errors.Is(err, api.ErrApprovalParked) {
		t.Fatalf("failed park Send = %v, want original transport cause", err)
	}
}

func TestApprovalParkClientRequiresAckAndClosure(t *testing.T) {
	tests := []struct {
		name  string
		reply func(v1.Harness_ConnectServer) error
		want  string
		code  codes.Code
	}{
		{name: "missing ack", reply: func(v1.Harness_ConnectServer) error { return nil }, want: "acknowledgement"},
		{name: "wrong ack execution", reply: func(s v1.Harness_ConnectServer) error { a := ackEvent(); a.ExecutionId = "foreign"; return s.Send(a) }, want: "acknowledgement"},
		{name: "wrong ack body execution", reply: func(s v1.Harness_ConnectServer) error {
			a := ackEvent()
			a.GetParked().ExecutionId = "foreign"
			return s.Send(a)
		}, want: "acknowledgement"},
		{name: "wrong ack call", reply: func(s v1.Harness_ConnectServer) error {
			a := ackEvent()
			a.GetParked().ToolCallId = "foreign"
			return s.Send(a)
		}, want: "acknowledgement"},
		{name: "wrong ack request", reply: func(s v1.Harness_ConnectServer) error {
			a := ackEvent()
			a.GetParked().RequestSeq = 18
			return s.Send(a)
		}, want: "acknowledgement"},
		{name: "bare ack", reply: func(s v1.Harness_ConnectServer) error {
			return s.Send(&v1.Event{ExecutionId: "exec", Kind: v1.EventKind_EVENT_PARKED})
		}, want: "acknowledgement"},
		{name: "END instead", reply: func(s v1.Harness_ConnectServer) error {
			return s.Send(&v1.Event{ExecutionId: "exec", Kind: v1.EventKind_EVENT_END})
		}, want: "acknowledgement"},
		{name: "extra END", reply: func(s v1.Harness_ConnectServer) error {
			if err := s.Send(ackEvent()); err != nil {
				return err
			}
			return s.Send(&v1.Event{ExecutionId: "exec", Kind: v1.EventKind_EVENT_END})
		}, want: "closure"},
		{name: "late effect", reply: func(s v1.Harness_ConnectServer) error {
			if err := s.Send(ackEvent()); err != nil {
				return err
			}
			return s.Send(&v1.Event{ExecutionId: "exec", Kind: v1.EventKind_EVENT_OUTPUT})
		}, want: "closure"},
		{name: "canceled EOF", reply: func(s v1.Harness_ConnectServer) error {
			if err := s.Send(ackEvent()); err != nil {
				return err
			}
			return status.Error(codes.Canceled, "unwind canceled")
		}, code: codes.Canceled},
		{name: "stalled ack", reply: func(s v1.Harness_ConnectServer) error { <-s.Context().Done(); return s.Context().Err() }, code: codes.DeadlineExceeded},
		{name: "stalled closure", reply: func(s v1.Harness_ConnectServer) error {
			if err := s.Send(ackEvent()); err != nil {
				return err
			}
			<-s.Context().Done()
			return s.Context().Err()
		}, code: codes.DeadlineExceeded},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			gotControl := make(chan *v1.ControllerFrame, 1)
			peer := connectPeer{run: func(s v1.Harness_ConnectServer) error {
				if _, err := s.Recv(); err != nil {
					return err
				}
				if err := s.Send(gateToolEvent()); err != nil {
					return err
				}
				frame, err := s.Recv()
				if err != nil {
					return err
				}
				gotControl <- frame
				return test.reply(s)
			}}
			ctx := approvalContext(t)
			if test.code == codes.DeadlineExceeded {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, time.Second)
				defer cancel()
			}
			err := NewClientHarness(approvalClient(t, peer)).Run(ctx, &api.Start{SessionUID: "session", ExecutionID: "exec"}, &gateHost{err: &api.ApprovalParkedError{Ref: gateRef}})
			if err == nil || errors.Is(err, api.ErrApprovalParked) {
				t.Fatalf("Run = %v; invalid handoff cannot be parked success", err)
			}
			if test.want != "" && !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Run = %v, want %s failure", err, test.want)
			}
			if test.code != codes.OK && status.Code(err) != test.code {
				t.Fatalf("Run = %v, want %s", err, test.code)
			}
			select {
			case f := <-gotControl:
				if f.GetSession() != "session" || f.GetExecutionId() != "exec" || f.GetPark().GetRequestSeq() != 17 {
					t.Fatalf("park control = %v", f)
				}
			default:
				t.Fatal("host never sent park control")
			}
		})
	}
}

func TestApprovalClientRejectsInvalidHostReplies(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*gateHost)
		want   string
	}{
		{"missing decision", func(h *gateHost) { h.result.Approval = nil }, "approval"},
		{"foreign decision call", func(h *gateHost) { h.result.Approval.ToolCallID = "foreign" }, "correlation"},
		{"zero decision request", func(h *gateHost) { h.result.Approval.RequestSeq = 0 }, "approval"},
		{"foreign receipt call", func(h *gateHost) { h.result.ID = "foreign" }, "correlation"},
		{"foreign receipt request", func(h *gateHost) { h.result.ApprovalRequestSeq = 18 }, "receipt"},
		{"negative receipt request", func(h *gateHost) { h.result.ApprovalRequestSeq = -1 }, "receipt"},
		{"zero receipt decision", func(h *gateHost) { h.result.ApprovalDecisionSeq = 0 }, "receipt"},
		{"negative receipt decision", func(h *gateHost) { h.result.ApprovalDecisionSeq = -1 }, "receipt"},
		{"denied success", func(h *gateHost) { h.result.Approval.Approved = false }, "status"},
		{"denied executor error", func(h *gateHost) {
			h.result.Approval.Approved = false
			h.result.Code = api.ToolResultCodeExecutorError
			h.result.IsError = true
		}, "status"},
		{"approved denial", func(h *gateHost) { h.result.Code = api.ToolResultCodeApprovalDenied; h.result.IsError = true }, "status"},
		{"approved legacy error", func(h *gateHost) { h.result.IsError = true }, "status"},
		{"executor code without error", func(h *gateHost) { h.result.Code = api.ToolResultCodeExecutorError }, "status"},
		{"unknown code", func(h *gateHost) { h.result.Code = api.ToolResultCode(99) }, "status"},
		{"bare park sentinel", func(h *gateHost) { h.err = api.ErrApprovalParked }, "park"},
		{"foreign park execution", func(h *gateHost) {
			h.err = &api.ApprovalParkedError{Ref: api.ApprovalRef{ExecutionID: "foreign", ToolCallID: "call", RequestSeq: 17}}
		}, "park"},
		{"foreign park call", func(h *gateHost) {
			h.err = &api.ApprovalParkedError{Ref: api.ApprovalRef{ExecutionID: "exec", ToolCallID: "foreign", RequestSeq: 17}}
		}, "park"},
		{"zero park request", func(h *gateHost) {
			h.err = &api.ApprovalParkedError{Ref: api.ApprovalRef{ExecutionID: "exec", ToolCallID: "call"}}
		}, "park"},
		{"negative park request", func(h *gateHost) {
			h.err = &api.ApprovalParkedError{Ref: api.ApprovalRef{ExecutionID: "exec", ToolCallID: "call", RequestSeq: -1}}
		}, "park"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			frames := make(chan *v1.ControllerFrame, 1)
			peer := connectPeer{run: func(s v1.Harness_ConnectServer) error {
				if _, err := s.Recv(); err != nil {
					return err
				}
				if err := s.Send(gateToolEvent()); err != nil {
					return err
				}
				f, err := s.Recv()
				frames <- f
				return err
			}}
			host := &gateHost{result: api.ToolResult{ID: "call", ApprovalRequestSeq: 17, ApprovalDecisionSeq: 19, Approval: &api.ApprovalResult{ToolCallID: "call", RequestSeq: 17, Approved: true}}}
			test.mutate(host)
			err := NewClientHarness(approvalClient(t, peer)).Run(approvalContext(t), &api.Start{SessionUID: "session", ExecutionID: "exec"}, host)
			if err == nil || errors.Is(err, api.ErrApprovalParked) || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("host reply = %v, want %s refusal", err, test.want)
			}
			select {
			case f := <-frames:
				if f != nil {
					t.Fatalf("invalid host result sent a control frame: %v", f)
				}
			case <-time.After(time.Second):
				t.Fatal("peer receive did not drain")
			}
		})
	}
}

func TestApprovalServerReportForgeryIsSticky(t *testing.T) {
	for _, test := range []struct {
		name   string
		result api.ToolResult
	}{
		{"transient", api.ToolResult{Approval: &api.ApprovalResult{ToolCallID: "call", RequestSeq: 17, Approved: true}}},
		{"request", api.ToolResult{ApprovalRequestSeq: 17}},
		{"decision", api.ToolResult{ApprovalDecisionSeq: 19}},
		{"denial", api.ToolResult{Code: api.ToolResultCodeApprovalDenied, IsError: true}},
		{"unknown code", api.ToolResult{Code: api.ToolResultCode(7)}},
	} {
		t.Run(test.name, func(t *testing.T) {
			observed := make(chan []error, 1)
			server := NewServer(gateHarness{run: func(ctx context.Context, _ *api.Start, sink api.EventSink) error {
				observed <- []error{sink.Report(ctx, test.result), sink.Output(ctx, "forged continuation")}
				return nil
			}})
			host := &gateHost{}
			err := NewClientHarness(approvalClient(t, server)).Run(approvalContext(t), &api.Start{SessionUID: "session", ExecutionID: "exec"}, host)
			if err == nil || !strings.Contains(err.Error(), "host-owned") {
				t.Fatalf("swallowed forged Report = %v", err)
			}
			for _, err := range <-observed {
				if err == nil || !strings.Contains(err.Error(), "host-owned") {
					t.Fatalf("sink refusal = %v", err)
				}
			}
			if host.reports != 0 || len(host.outputs) != 0 {
				t.Fatalf("forged receipt/effect reached host: %#v", host)
			}
		})
	}
}
