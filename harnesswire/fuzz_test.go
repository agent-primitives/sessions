package harnesswire

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"

	"github.com/agent-primitives/sessions/api"
	v1 "github.com/agent-primitives/sessions/api/genpb"
	"github.com/agent-primitives/sessions/internal/fuzzcheck"
	"github.com/agent-primitives/sessions/wire"
)

// fuzzDeadline bounds one input. Both ends must return once the peer's frames run out; a run that
// does not is a hang the fuzzer reports.
const fuzzDeadline = 10 * time.Second

// fuzzServerStream feeds a fixed list of host frames to Server.Connect. A frame that leaves
// execution_id or model_call_id empty gets the current one, so the fuzzer reaches the decoding
// behind the correlation checks as well as the checks themselves.
type fuzzServerStream struct {
	v1.Harness_ConnectServer
	ctx context.Context
	// Send and Recv run on different goroutines, as gRPC allows.
	mu        sync.Mutex
	frames    []*v1.ControllerFrame
	execution string
	modelCall string
	sent      []*v1.Event
}

func (s *fuzzServerStream) Context() context.Context { return s.ctx }

func (s *fuzzServerStream) Send(ev *v1.Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if id := ev.GetModel().GetId(); id != "" {
		s.modelCall = id
	}
	s.sent = append(s.sent, ev)
	return nil
}

func (s *fuzzServerStream) Recv() (*v1.ControllerFrame, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.frames) == 0 {
		return nil, io.EOF
	}
	f := s.frames[0]
	s.frames = s.frames[1:]
	if f.GetStart() != nil && s.execution == "" {
		s.execution = f.GetExecutionId()
	}
	if f.GetExecutionId() == "" {
		f.ExecutionId = s.execution
	}
	if m := f.GetModel(); m != nil && m.GetModelCallId() == "" {
		m.ModelCallId = s.modelCall
	}
	return f, nil
}

// fuzzHarness drives every sink operation that waits for a host frame.
type fuzzHarness struct{}

func (fuzzHarness) Describe(context.Context) (api.Descriptor, error) {
	return api.Descriptor{ID: "fuzz"}, nil
}

func (fuzzHarness) Run(ctx context.Context, start *api.Start, sink api.EventSink) error {
	if _, err := sink.Model(ctx, api.ModelRequest{Model: "m", Messages: start.Inputs}); err != nil {
		return err
	}
	if _, err := sink.ToolCall(ctx, api.ToolCall{ID: "t1", Tool: "read", Mediation: api.MediationControllerMediated}); err != nil {
		return err
	}
	if _, err := sink.ToolCall(ctx, api.ToolCall{ID: "t2", Tool: "write", Mediation: api.MediationRequiresApproval}); err != nil {
		return err
	}
	return sink.Output(ctx, "done")
}

func decodeFrames(raw ...[]byte) ([]*v1.ControllerFrame, int, bool) {
	var frames []*v1.ControllerFrame
	n := 0
	for _, b := range raw {
		f := &v1.ControllerFrame{}
		if err := proto.Unmarshal(b, f); err != nil {
			return nil, 0, false
		}
		frames = append(frames, f)
		n += len(b)
	}
	return frames, n, true
}

func mustMarshal(tb testing.TB, m proto.Message) []byte {
	tb.Helper()
	b, err := proto.Marshal(m)
	if err != nil {
		tb.Fatal(err)
	}
	return b
}

// FuzzServerConnect sends arbitrary host frames to the harness side of the wire. Connect must
// return (no panic, no hang, bounded allocation) and every event it sends must carry the Start's
// execution_id.
func FuzzServerConnect(f *testing.F) {
	start := mustMarshal(f, &v1.ControllerFrame{Session: "s", ExecutionId: "e", Frame: &v1.ControllerFrame_Start{Start: &v1.Start{
		Inputs: []*v1.Message{wire.MessageToProto(api.TextMessage("user", "hi"))}, Config: []byte("cfg"), ResumeFromSeq: 3,
	}}})
	model := mustMarshal(f, &v1.ControllerFrame{Frame: &v1.ControllerFrame_Model{Model: &v1.ModelResult{
		Message: wire.MessageToProto(api.TextMessage("assistant", "ok")), Usage: &v1.Usage{Model: "m", InputTokens: 1},
	}}})
	tool := mustMarshal(f, &v1.ControllerFrame{Frame: &v1.ControllerFrame_Tool{Tool: &v1.ToolResult{Id: "t1"}}})
	decision := mustMarshal(f, &v1.ControllerFrame{Frame: &v1.ControllerFrame_Approval{Approval: &v1.ApprovalResult{ToolCallId: "t2", Approved: true, RequestSeq: 4}}})
	receipt := mustMarshal(f, &v1.ControllerFrame{Frame: &v1.ControllerFrame_Tool{Tool: &v1.ToolResult{Id: "t2", ApprovalRequestSeq: 4, ApprovalDecisionSeq: 5}}})
	park := mustMarshal(f, &v1.ControllerFrame{Frame: &v1.ControllerFrame_Park{Park: &v1.ApprovalRef{ExecutionId: "e", ToolCallId: "t2", RequestSeq: 4}}})
	f.Add(start, model, tool, decision, receipt)
	f.Add(start, model, tool, park, []byte{})
	f.Add(start, []byte{}, []byte{}, []byte{}, []byte{})
	f.Fuzz(func(t *testing.T, a, b, c, d, e []byte) {
		frames, n, ok := decodeFrames(a, b, c, d, e)
		if !ok {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), fuzzDeadline)
		defer cancel()
		stream := &fuzzServerStream{ctx: ctx, frames: frames}
		fuzzcheck.Allocs(t, n, func() {
			done := make(chan struct{})
			go func() {
				defer close(done)
				_ = NewServer(fuzzHarness{}).Connect(stream)
			}()
			select {
			case <-done:
			case <-time.After(fuzzDeadline + time.Second):
				t.Fatal("Connect did not return after the frames ran out")
			}
		})
		stream.mu.Lock()
		defer stream.mu.Unlock()
		for _, ev := range stream.sent {
			if ev.GetExecutionId() != stream.execution {
				t.Fatalf("sent %s with execution_id %q, Start had %q", ev.GetKind(), ev.GetExecutionId(), stream.execution)
			}
		}
	})
}

// fuzzClientStream feeds a fixed list of harness events to ClientHarness.Run. An event with an
// empty execution_id gets the Start's.
type fuzzClientStream struct {
	grpc.ClientStream
	execution string
	events    []*v1.Event
}

func (s *fuzzClientStream) Send(*v1.ControllerFrame) error { return nil }
func (s *fuzzClientStream) CloseSend() error               { return nil }
func (s *fuzzClientStream) Recv() (*v1.Event, error) {
	if len(s.events) == 0 {
		return nil, io.EOF
	}
	ev := s.events[0]
	s.events = s.events[1:]
	if ev.GetExecutionId() == "" {
		ev.ExecutionId = s.execution
	}
	return ev, nil
}

type fuzzClient struct {
	v1.HarnessClient
	stream *fuzzClientStream
}

func (c fuzzClient) Connect(context.Context, ...grpc.CallOption) (grpc.BidiStreamingClient[v1.ControllerFrame, v1.Event], error) {
	return c.stream, nil
}

// FuzzClientRun sends arbitrary harness events to the host side of the wire. Run must return (no
// panic, no hang, bounded allocation), and a host-owned event from the harness is always refused.
func FuzzClientRun(f *testing.F) {
	// Journal events carry their own execution IDs; clear them so the stream gives each the
	// Start's and the seeds reach dispatch instead of the execution_id check.
	var seeds [][]byte
	for _, blob := range fuzzcheck.JournalEvents(f) {
		ev := &v1.Event{}
		if err := proto.Unmarshal(blob, ev); err != nil {
			f.Fatal(err)
		}
		ev.ExecutionId = ""
		seeds = append(seeds, mustMarshal(f, ev))
	}
	for i := range seeds {
		f.Add(seeds[i], seeds[(i+1)%len(seeds)], seeds[(i+2)%len(seeds)])
	}
	f.Fuzz(func(t *testing.T, a, b, c []byte) {
		var events []*v1.Event
		for _, raw := range [][]byte{a, b, c} {
			ev := &v1.Event{}
			if err := proto.Unmarshal(raw, ev); err != nil {
				return
			}
			events = append(events, ev)
		}
		ctx, cancel := context.WithTimeout(context.Background(), fuzzDeadline)
		defer cancel()
		stream := &fuzzClientStream{execution: "e", events: events}
		var err error
		fuzzcheck.Allocs(t, len(a)+len(b)+len(c), func() {
			err = NewClientHarness(fuzzClient{stream: stream}).Run(ctx, &api.Start{SessionUID: "s", ExecutionID: "e"}, &hostSink{})
		})
		if errors.Is(err, context.DeadlineExceeded) {
			t.Fatal("Run did not return after the events ran out")
		}
		// Run handles events in order and stops at END, so only a host-owned event before any END
		// is certain to be reached.
	scan:
		for _, ev := range events {
			switch ev.GetKind() {
			case v1.EventKind_EVENT_END:
				break scan
			case v1.EventKind_EVENT_APPROVAL_REQUEST, v1.EventKind_EVENT_APPROVAL_RESULT,
				v1.EventKind_EVENT_EXECUTION_START, v1.EventKind_EVENT_PARKED:
				if err == nil {
					t.Fatalf("Run accepted host-owned %s from the harness", ev.GetKind())
				}
				break scan
			}
		}
	})
}
