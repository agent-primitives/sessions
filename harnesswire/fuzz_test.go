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

// fuzzDeadline bounds one input. Both ends must return once the peer's frames run out, which takes
// microseconds; a run that returns only because this deadline expired is a hang the fuzzer reports.
// It is short so a hang does not eat the PR fuzz budget.
const fuzzDeadline = 2 * time.Second

// fuzzServerStream feeds a fixed list of host frames to Server.Connect. A frame that leaves
// execution_id empty gets the Start's, and a ModelResult that leaves model_call_id empty gets the
// ID of the harness's latest model call, so the fuzzer reaches the decoding behind the correlation
// checks as well as the checks themselves.
//
// Connect reads frames ahead of the harness, so filling the ID when the frame is read would take
// whatever call was current at that moment, which depends on goroutine scheduling. Recv instead
// holds such a frame until the harness is waiting for a reply it has not been sent, then fills the
// ID: when the harness is waiting on a model call, the reply always correlates. Waiting on that
// condition, rather than on the next model call, cannot hang: a harness that is waiting for a
// reply always satisfies it, and one that is not has returned and closed stop.
type fuzzServerStream struct {
	v1.Harness_ConnectServer
	ctx context.Context
	// wake wakes a held Recv after the harness sends a request; stop closes once Connect has
	// returned.
	wake chan struct{}
	stop chan struct{}
	// Send and Recv run on different goroutines, as gRPC allows.
	mu        sync.Mutex
	frames    []*v1.ControllerFrame
	execution string
	modelCall string
	// awaited counts the replies the harness's requests wait for (a model call or tool call waits
	// for one, a call that requires approval for a decision and then a result); delivered counts
	// the frames after Start that Recv has returned. awaited > delivered means the harness is, or
	// is about to be, waiting for a frame it has not been sent.
	awaited   int
	delivered int
	started   bool
	sent      []*v1.Event
}

func newFuzzServerStream(ctx context.Context, frames []*v1.ControllerFrame) *fuzzServerStream {
	return &fuzzServerStream{ctx: ctx, frames: frames, wake: make(chan struct{}, 1), stop: make(chan struct{})}
}

func (s *fuzzServerStream) Context() context.Context { return s.ctx }

func (s *fuzzServerStream) Send(ev *v1.Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sent = append(s.sent, ev)
	switch ev.GetKind() {
	case v1.EventKind_EVENT_MODEL_CALL:
		s.modelCall = ev.GetModel().GetId()
		s.awaited++
	case v1.EventKind_EVENT_TOOL_CALL:
		s.awaited++
		if ev.GetTool().GetMediation() == v1.Mediation_MEDIATION_REQUIRES_APPROVAL {
			s.awaited++
		}
	default:
		return nil
	}
	select {
	case s.wake <- struct{}{}:
	default:
	}
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
	if !s.started {
		s.started = true
		if f.GetStart() != nil {
			s.execution = f.GetExecutionId()
		}
	} else {
		s.delivered++
	}
	if f.GetExecutionId() == "" {
		f.ExecutionId = s.execution
	}
	if m := f.GetModel(); m != nil && m.GetModelCallId() == "" {
		for s.awaited < s.delivered {
			s.mu.Unlock()
			select {
			case <-s.wake:
			case <-s.stop:
				s.mu.Lock()
				return nil, io.EOF
			case <-s.ctx.Done():
				s.mu.Lock()
				return nil, s.ctx.Err()
			}
			s.mu.Lock()
		}
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

// decodeFrames decodes the fuzz arguments into frames. An empty argument is no frame rather than an
// empty one, so an input can end the stream early and reach the peer-closed paths.
func decodeFrames(raw ...[]byte) ([]*v1.ControllerFrame, bool) {
	var frames []*v1.ControllerFrame
	for _, b := range raw {
		if len(b) == 0 {
			continue
		}
		f := &v1.ControllerFrame{}
		if err := proto.Unmarshal(b, f); err != nil {
			return nil, false
		}
		frames = append(frames, f)
	}
	return frames, true
}

func mustMarshal(tb testing.TB, m proto.Message) []byte {
	tb.Helper()
	b, err := proto.Marshal(m)
	if err != nil {
		tb.Fatal(err)
	}
	return b
}

// serverSeed is a host frame sequence for FuzzServerConnect and the last event Connect sends for it.
type serverSeed struct {
	name   string
	frames [5][]byte
	last   v1.EventKind
	state  string // the END state; empty for PARKED
}

// serverSeeds covers each path fuzzHarness can take: a full turn through a mediated tool and an
// approved one, a park on the approval, the host going away before and after the model reply, and
// a reply of the wrong kind.
// TestFuzzServerConnectSeeds pins that each seed reaches its path, so the corpus does not decay into
// inputs that all stop at the first correlation check.
func serverSeeds(tb testing.TB) []serverSeed {
	start := mustMarshal(tb, &v1.ControllerFrame{Session: "s", ExecutionId: "e", Frame: &v1.ControllerFrame_Start{Start: &v1.Start{
		Inputs: []*v1.Message{wire.MessageToProto(api.TextMessage("user", "hi"))}, Config: []byte("cfg"), ResumeFromSeq: 3,
	}}})
	model := mustMarshal(tb, &v1.ControllerFrame{Frame: &v1.ControllerFrame_Model{Model: &v1.ModelResult{
		Message: wire.MessageToProto(api.TextMessage("assistant", "ok")), Usage: &v1.Usage{Model: "m", InputTokens: 1},
	}}})
	// Tool replies are checked against the Start's session, so each carries it.
	tool := mustMarshal(tb, &v1.ControllerFrame{Session: "s", Frame: &v1.ControllerFrame_Tool{Tool: &v1.ToolResult{Id: "t1"}}})
	decision := mustMarshal(tb, &v1.ControllerFrame{Session: "s", Frame: &v1.ControllerFrame_Approval{Approval: &v1.ApprovalResult{ToolCallId: "t2", Approved: true, RequestSeq: 4}}})
	receipt := mustMarshal(tb, &v1.ControllerFrame{Session: "s", Frame: &v1.ControllerFrame_Tool{Tool: &v1.ToolResult{Id: "t2", ApprovalRequestSeq: 4, ApprovalDecisionSeq: 5}}})
	park := mustMarshal(tb, &v1.ControllerFrame{Session: "s", Frame: &v1.ControllerFrame_Park{Park: &v1.ApprovalRef{ExecutionId: "e", ToolCallId: "t2", RequestSeq: 4}}})
	return []serverSeed{
		{"approved", [5][]byte{start, model, tool, decision, receipt}, v1.EventKind_EVENT_END, "COMPLETED"},
		{"parked", [5][]byte{start, model, tool, park, {}}, v1.EventKind_EVENT_PARKED, ""},
		{"host gone mid-turn", [5][]byte{start, {}, {}, {}, {}}, v1.EventKind_EVENT_END, "FAILED"},
		{"host gone after model", [5][]byte{start, model, {}, {}, {}}, v1.EventKind_EVENT_END, "FAILED"},
		// The second model reply arrives while the harness waits on the tool call: it must fail the
		// turn, not hold the frame for a model call that never comes.
		{"model reply for a tool call", [5][]byte{start, model, model, {}, {}}, v1.EventKind_EVENT_END, "FAILED"},
	}
}

// runServerConnect decodes the frames and runs Connect over them, failing t on a hang and on the
// allocation bound, which covers decoding the frames too. It reports false for an input that does
// not decode.
func runServerConnect(t *testing.T, raw ...[]byte) (*fuzzServerStream, bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), fuzzDeadline)
	defer cancel()
	n := 0
	for _, b := range raw {
		n += len(b)
	}
	var stream *fuzzServerStream
	fuzzcheck.Allocs(t, n, func() {
		frames, ok := decodeFrames(raw...)
		if !ok {
			return
		}
		stream = newFuzzServerStream(ctx, frames)
		done := make(chan struct{})
		go func() {
			defer close(done)
			defer close(stream.stop)
			_ = NewServer(fuzzHarness{}).Connect(stream)
		}()
		select {
		case <-done:
		case <-time.After(fuzzDeadline + time.Second):
			t.Fatal("Connect did not return after the frames ran out")
		}
	})
	if stream == nil {
		return nil, false
	}
	if ctx.Err() != nil {
		t.Fatal("Connect returned only because its stream context expired, not when the frames ran out")
	}
	return stream, true
}

// FuzzServerConnect sends arbitrary host frames to the harness side of the wire. Connect must
// return (no panic, no hang, bounded allocation) and every event it sends must carry the Start's
// execution_id.
func FuzzServerConnect(f *testing.F) {
	for _, seed := range serverSeeds(f) {
		f.Add(seed.frames[0], seed.frames[1], seed.frames[2], seed.frames[3], seed.frames[4])
	}
	f.Fuzz(func(t *testing.T, a, b, c, d, e []byte) {
		stream, ok := runServerConnect(t, a, b, c, d, e)
		if !ok {
			return
		}
		stream.mu.Lock()
		defer stream.mu.Unlock()
		for _, ev := range stream.sent {
			if ev.GetExecutionId() != stream.execution {
				t.Fatalf("sent %s with execution_id %q, Start had %q", ev.GetKind(), ev.GetExecutionId(), stream.execution)
			}
		}
	})
}

func TestFuzzServerConnectSeeds(t *testing.T) {
	for _, seed := range serverSeeds(t) {
		t.Run(seed.name, func(t *testing.T) {
			// Repeat, since a correlation that depends on goroutine timing passes some runs.
			for range 50 {
				stream, ok := runServerConnect(t, seed.frames[:]...)
				if !ok {
					t.Fatal("seed does not decode")
				}
				stream.mu.Lock()
				last := stream.sent[len(stream.sent)-1]
				stream.mu.Unlock()
				if last.GetKind() != seed.last || last.GetEnd().GetState() != seed.state {
					t.Fatalf("last event %s %q (%s), want %s %q",
						last.GetKind(), last.GetEnd().GetState(), last.GetEnd().GetError().GetDescription(), seed.last, seed.state)
				}
			}
		})
	}
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
	f.Add(seeds[0], []byte{}, []byte{}) // the harness goes away mid-turn
	f.Fuzz(func(t *testing.T, a, b, c []byte) {
		ctx, cancel := context.WithTimeout(context.Background(), fuzzDeadline)
		defer cancel()
		var events []*v1.Event
		decoded := true
		var err error
		// The bound covers decoding the events as well as Run.
		fuzzcheck.Allocs(t, len(a)+len(b)+len(c), func() {
			for _, raw := range [][]byte{a, b, c} {
				if len(raw) == 0 {
					continue // no event, so the stream can end early
				}
				ev := &v1.Event{}
				if proto.Unmarshal(raw, ev) != nil {
					decoded = false
					return
				}
				events = append(events, ev)
			}
			stream := &fuzzClientStream{execution: "e", events: events}
			err = NewClientHarness(fuzzClient{stream: stream}).Run(ctx, &api.Start{SessionUID: "s", ExecutionID: "e"}, &hostSink{})
		})
		if !decoded {
			return
		}
		if errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
			t.Fatal("Run returned only because its context expired, not when the events ran out")
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
