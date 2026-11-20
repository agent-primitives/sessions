package conformance_test

import (
	"context"
	"errors"
	"net"
	"reflect"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	"github.com/agent-primitives/sessions/api"
	v1 "github.com/agent-primitives/sessions/api/genpb"
	"github.com/agent-primitives/sessions/controller"
	"github.com/agent-primitives/sessions/harness/echoagent"
	"github.com/agent-primitives/sessions/harnesswire"
)

// wireHarness runs the echo harness in a gRPC Harness server (bufconn) and returns an api.Harness
// that drives it OUT OF PROCESS over Harness.Connect. Each Run opens a fresh Connect stream, so the
// same value serves both the live turn and the replay.
func wireHarness(t *testing.T) api.Harness { return wireHarnessFrom(t, echoagent.Harness{}) }

// wireHarnessFrom serves h over a bufconn Harness server and returns an api.Harness that drives it
// out of process, so a test can swap in a deliberately-failing harness.
func wireHarnessFrom(t *testing.T, h api.Harness) api.Harness {
	return harnesswire.NewClientHarness(wireHarnessClientFrom(t, h))
}

func wireHarnessClientFrom(t *testing.T, h api.Harness) v1.HarnessClient {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	v1.RegisterHarnessServer(srv, harnesswire.NewServer(h))
	go srv.Serve(lis)
	t.Cleanup(srv.Stop)

	conn, err := grpc.NewClient(
		"passthrough:///harness",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return v1.NewHarnessClient(conn)
}

type descriptorHarness struct {
	descriptor api.Descriptor
}

func (h descriptorHarness) Describe(context.Context) (api.Descriptor, error) {
	return h.descriptor, nil
}

func (descriptorHarness) Run(context.Context, *api.Start, api.EventSink) error {
	return errors.New("descriptor harness only supports Describe")
}

func TestWireDescribeRoundTrip(t *testing.T) {
	for _, test := range []struct {
		name  string
		tools []api.ToolSpec
	}{
		{
			name: "multiple tools",
			tools: []api.ToolSpec{
				{Name: "report", Description: "Report local work", Mediation: api.MediationInHarnessReported},
				{Name: "lookup", Description: "Look up a record", Mediation: api.MediationControllerMediated},
				{Name: "charge", Description: "Request a charge", Mediation: api.MediationRequiresApproval},
				{Name: "default", Description: "Use unspecified mediation"},
			},
		},
		{name: "no tools"},
	} {
		t.Run(test.name, func(t *testing.T) {
			want := api.Descriptor{
				ID:     "declared-tools",
				Models: []string{"model-b", "model-a"},
				Tools:  test.tools,
				Capabilities: api.Capabilities{
					Resumability:    api.ResumabilityRequiresMemorySnapshot,
					ForkSafe:        true,
					RequiresGPU:     true,
					Streaming:       true,
					ReasoningReplay: true,
				},
			}
			har := wireHarnessFrom(t, descriptorHarness{descriptor: want})
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			got, err := har.Describe(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("Describe round-trip = %#v, want %#v", got, want)
			}
		})
	}
}

// failHarness fails its run without emitting anything; the Server terminates the stream with
// END{FAILED,error} and returns the status.
type failHarness struct{}

func (failHarness) Describe(context.Context) (api.Descriptor, error) {
	return api.Descriptor{ID: "fail"}, nil
}
func (failHarness) Run(context.Context, *api.Start, api.EventSink) error {
	return errors.New("boom")
}

type executionCaptureHarness struct {
	executionIDs chan<- string
}

func (executionCaptureHarness) Describe(context.Context) (api.Descriptor, error) {
	return api.Descriptor{ID: "execution-capture"}, nil
}

func (h executionCaptureHarness) Run(_ context.Context, start *api.Start, _ api.EventSink) error {
	h.executionIDs <- start.ExecutionID
	return nil
}

func TestWirePropagatesExecutionID(t *testing.T) {
	s, _ := openFile(t)
	defer s.Close()
	log := s.Session("s")
	executionIDs := make(chan string, 1)
	har := wireHarnessFrom(t, executionCaptureHarness{executionIDs: executionIDs})

	c, err := controller.New(log, (&countModel{}).call)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Exec(context.Background(), har, []api.Message{*api.TextMessage("user", "hi")}, 0); err != nil {
		t.Fatal(err)
	}
	records, err := log.Read(1)
	if err != nil {
		t.Fatal(err)
	}
	got := <-executionIDs
	want := records[0].Event.ExecutionID
	if got == "" || got != want {
		t.Fatalf("remote harness execution ID = %q, want journal ID %q", got, want)
	}
}

// TestWireFailedHarnessSurfacesError is the END-state correctness proof across the wire: a remote
// harness that FAILs is journaled as EVENT_ERROR, not EVENT_END{COMPLETED}. Before the fix,
// ClientHarness.Run returned nil on any END event, so the controller recorded the failed turn as a
// successful one and the failure was silently lost across the process boundary.
func TestWireFailedHarnessSurfacesError(t *testing.T) {
	s, _ := openFile(t)
	defer s.Close()
	log := s.Session("s")
	har := wireHarnessFrom(t, failHarness{})

	c, err := controller.New(log, (&countModel{}).call)
	if err != nil {
		t.Fatal(err)
	}
	err = c.Exec(context.Background(), har, []api.Message{*api.TextMessage("user", "hi")}, 0)
	if err == nil {
		t.Fatal("exec of a failing remote harness returned nil; the failure was swallowed")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Fatalf("surfaced error did not carry the harness failure %q: %v", "boom", err)
	}

	recs, rerr := log.Read(1)
	if rerr != nil {
		t.Fatal(rerr)
	}
	last := recs[len(recs)-1].Event
	if last.Kind == api.EventEnd {
		t.Fatalf("failed turn was journaled as END{%s}; want EVENT_ERROR", last.End.State)
	}
	if last.Kind != api.EventError {
		t.Fatalf("terminal event = %s; want ERROR", last.Kind)
	}
	if err := log.Verify(); err != nil {
		t.Fatalf("verify after a failed turn: %v", err)
	}
}

// TestWireNondeterministicReplay is the load-bearing proof across a PROCESS BOUNDARY: with the
// harness driven out-of-process over Harness.Connect and a genuinely nondeterministic model, replay
// still reconstructs the recorded output byte-identically and invokes the model zero times (I1).
// The host mediates the model over the wire — the harness never invokes it — so the load-bearing
// rule holds remotely, exactly as in-process.
func TestWireNondeterministicReplay(t *testing.T) {
	s, _ := openFile(t)
	defer s.Close()
	log := s.Session("s")
	har := wireHarness(t)
	m := &countModel{}

	c, err := controller.New(log, m.call)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Exec(context.Background(), har, []api.Message{*api.TextMessage("user", "hi")}, 0); err != nil {
		t.Fatalf("wire exec: %v", err)
	}
	live, _ := c.Outputs()
	again, _ := m.call(t.Context(), api.ModelRequest{Model: "echo", Messages: []api.Message{*api.TextMessage("user", "hi")}})
	if len(live) != 1 || again.Message.Text() == live[0] {
		t.Fatalf("model not nondeterministic (live=%v again=%q)", live, again.Message.Text())
	}

	c2, err := controller.New(log, m.call)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := c2.Replay(context.Background(), har)
	if err != nil {
		t.Fatalf("wire replay: %v", err)
	}
	if !reflect.DeepEqual(live, replay) {
		t.Fatalf("wire replay served a fresh answer instead of the recorded one: %v vs %v", replay, live)
	}
	if c2.ModelInvocations() != 0 {
		t.Fatalf("wire replay invoked the model %d times (I1)", c2.ModelInvocations())
	}
	if err := log.Verify(); err != nil {
		t.Fatalf("verify: %v", err)
	}
}

type unmediatedReplayHarness struct {
	describes int
	runs      int
}

type probeDescribeFailureHarness struct {
	describes int
	runs      int
	err       error
}

func (h *probeDescribeFailureHarness) Describe(context.Context) (api.Descriptor, error) {
	h.describes++
	if h.describes > 1 {
		return api.Descriptor{}, h.err
	}
	return api.Descriptor{ID: "probe-failure", Version: "1", HarnessClass: api.HarnessClassMediated}, nil
}

func (h *probeDescribeFailureHarness) Run(context.Context, *api.Start, api.EventSink) error {
	h.runs++
	return nil
}

func (h *unmediatedReplayHarness) Describe(context.Context) (api.Descriptor, error) {
	h.describes++
	return api.Descriptor{ID: "unmediated", Version: "1", HarnessClass: api.HarnessClassUnmediated,
		Capabilities: api.Capabilities{Resumability: api.ResumabilityRequiresMemorySnapshot}}, nil
}

func (h *unmediatedReplayHarness) Run(ctx context.Context, _ *api.Start, sink api.EventSink) error {
	h.runs++
	return sink.Output(ctx, "reported output")
}

func TestWireReplayRefusesUnmediatedHarnessBeforeConnect(t *testing.T) {
	s, _ := openFile(t)
	defer s.Close()
	log := s.Session("s")
	fence, err := log.NewFence()
	if err != nil {
		t.Fatal(err)
	}
	for i, event := range []api.Event{
		{ExecutionID: "turn", Kind: api.EventExecutionStart, ExecutionStart: &api.ExecutionStart{Harness: "unmediated", HarnessVersion: "1", InputCount: new(int64)}},
		{ExecutionID: "turn", Kind: api.EventOutput, Message: api.TextMessage("assistant", "reported output")},
		{ExecutionID: "turn", Kind: api.EventEnd, End: &api.HarnessEnd{State: "COMPLETED"}},
	} {
		if _, err := log.Append(int64(i), fence, event); err != nil {
			t.Fatal(err)
		}
	}
	h := &unmediatedReplayHarness{}
	remote := wireHarnessFrom(t, h)
	c, err := controller.New(log, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Replay(t.Context(), remote); !errors.Is(err, controller.ErrReconstructionUnsupported) {
		t.Fatalf("wire Replay = %v; want ErrReconstructionUnsupported", err)
	}
	if h.runs != 0 {
		t.Fatalf("wire Replay opened Connect and ran the unmediated harness %d times", h.runs)
	}
	if h.describes != 1 {
		t.Fatalf("wire Replay Describe calls = %d, want preflight refusal before Connect", h.describes)
	}
}

func TestWireReplayProbeDescribeFailureIsTransient(t *testing.T) {
	const private = "backend temporarily down: token=private"
	for _, tc := range []struct {
		name, want, notWant string
		cause               error
		code                codes.Code
	}{
		{name: "plain error is redacted", cause: errors.New(private), code: codes.Unavailable, want: "describe harness for reconstruction failed", notWant: private},
		{name: "explicit status is preserved", cause: status.Error(codes.ResourceExhausted, "worker pool drained"), code: codes.ResourceExhausted, want: "worker pool drained"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := openFile(t)
			defer s.Close()
			log := s.Session("s")
			fence, err := log.NewFence()
			if err != nil {
				t.Fatal(err)
			}
			for i, event := range []api.Event{
				{ExecutionID: "turn", Kind: api.EventExecutionStart, ExecutionStart: &api.ExecutionStart{Harness: "probe-failure", HarnessVersion: "1", InputCount: new(int64)}},
				{ExecutionID: "turn", Kind: api.EventEnd, End: &api.HarnessEnd{State: "COMPLETED"}},
			} {
				if _, err := log.Append(int64(i), fence, event); err != nil {
					t.Fatal(err)
				}
			}
			h := &probeDescribeFailureHarness{err: tc.cause}
			c, err := controller.New(log, nil)
			if err != nil {
				t.Fatal(err)
			}
			_, err = c.Replay(t.Context(), wireHarnessFrom(t, h))
			if !errors.Is(err, api.ErrReconstructionHandshake) || errors.Is(err, api.ErrReconstructionUnsupported) || status.Code(err) != tc.code || !strings.Contains(err.Error(), tc.want) || tc.notWant != "" && strings.Contains(err.Error(), tc.notWant) {
				t.Fatalf("Replay probe Describe failure = %v; want code=%s text=%q without %q", err, tc.code, tc.want, tc.notWant)
			}
			if h.describes != 2 || h.runs != 0 {
				t.Fatalf("probe Describe failure had effects: describes=%d runs=%d", h.describes, h.runs)
			}
		})
	}
}

type routedClassClient struct {
	describe *v1.HarnessDescriptor
	connect  v1.HarnessClient
}

type harnessDecorator struct {
	api.Harness
}

func (h harnessDecorator) Describe(ctx context.Context) (api.Descriptor, error) {
	return h.Harness.Describe(ctx)
}

func (h harnessDecorator) Run(ctx context.Context, start *api.Start, sink api.EventSink) error {
	return h.Harness.Run(ctx, start, sink)
}

func (c routedClassClient) Describe(context.Context, *v1.DescribeRequest, ...grpc.CallOption) (*v1.HarnessDescriptor, error) {
	return c.describe, nil
}

func (c routedClassClient) Connect(ctx context.Context, opts ...grpc.CallOption) (v1.Harness_ConnectClient, error) {
	return c.connect.Connect(ctx, opts...)
}

func TestWireReplayBindsClassToConnectPeer(t *testing.T) {
	s, _ := openFile(t)
	defer s.Close()
	log := s.Session("s")
	fence, err := log.NewFence()
	if err != nil {
		t.Fatal(err)
	}
	for i, event := range []api.Event{
		{ExecutionID: "turn", Kind: api.EventExecutionStart, ExecutionStart: &api.ExecutionStart{Harness: "routed", HarnessVersion: "1", InputCount: new(int64)}},
		{ExecutionID: "turn", Kind: api.EventOutput, Message: api.TextMessage("assistant", "reported output")},
		{ExecutionID: "turn", Kind: api.EventEnd, End: &api.HarnessEnd{State: "COMPLETED"}},
	} {
		if _, err := log.Append(int64(i), fence, event); err != nil {
			t.Fatal(err)
		}
	}

	unmediated := &unmediatedReplayHarness{}
	mediated := harnesswire.DescriptorToProto(api.Descriptor{ID: "routed", Version: "1", HarnessClass: api.HarnessClassMediated})
	remote := harnessDecorator{Harness: harnesswire.NewClientHarness(routedClassClient{describe: mediated, connect: wireHarnessClientFrom(t, unmediated)})}
	c, err := controller.New(log, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Replay(t.Context(), remote); !errors.Is(err, controller.ErrReconstructionUnsupported) {
		t.Fatalf("routed Replay = %v; want stream-bound refusal", err)
	}
	if unmediated.runs != 0 {
		t.Fatalf("routed Replay ran unmediated Connect peer %d times", unmediated.runs)
	}
}

func TestWireReplayPropagatesClassCheckThroughDelegatingServer(t *testing.T) {
	s, _ := openFile(t)
	defer s.Close()
	log := s.Session("s")
	fence, err := log.NewFence()
	if err != nil {
		t.Fatal(err)
	}
	for i, event := range []api.Event{
		{ExecutionID: "turn", Kind: api.EventExecutionStart, ExecutionStart: &api.ExecutionStart{Harness: "routed", HarnessVersion: "1", InputCount: new(int64)}},
		{ExecutionID: "turn", Kind: api.EventOutput, Message: api.TextMessage("assistant", "reported output")},
		{ExecutionID: "turn", Kind: api.EventEnd, End: &api.HarnessEnd{State: "COMPLETED"}},
	} {
		if _, err := log.Append(int64(i), fence, event); err != nil {
			t.Fatal(err)
		}
	}

	unmediated := &unmediatedReplayHarness{}
	mediated := harnesswire.DescriptorToProto(api.Descriptor{ID: "routed", Version: "1", HarnessClass: api.HarnessClassMediated})
	inner := harnesswire.NewClientHarness(routedClassClient{describe: mediated, connect: wireHarnessClientFrom(t, unmediated)})
	outer := wireHarnessFrom(t, inner)
	c, err := controller.New(log, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Replay(t.Context(), outer); err == nil || !strings.Contains(err.Error(), api.ErrReconstructionUnsupported.Error()) || !strings.Contains(err.Error(), "UNMEDIATED") {
		t.Fatalf("two-hop Replay = %v; want the inner unmediated-class refusal", err)
	}
	if unmediated.runs != 0 {
		t.Fatalf("two-hop Replay ran unmediated inner peer %d times", unmediated.runs)
	}
	if unmediated.describes != 1 {
		t.Fatalf("two-hop Replay inner Describe calls = %d, want exact-stream class check", unmediated.describes)
	}
}

// TestWireControllerMediatedTool proves a CONTROLLER_MEDIATED tool is host-mediated across the
// process boundary: the out-of-process harness emits the tool call over Harness.Connect, the HOST
// executes it (its executor, not the harness) and records intent+result, and on replay the recorded
// result is served without re-executing — determinism holds for tools exactly as for model calls.
func TestWireControllerMediatedTool(t *testing.T) {
	s, _ := openFile(t)
	defer s.Close()
	log := s.Session("s")
	har := wireHarnessFrom(t, toolHarness{key: "k1"})
	tool := newIdempotentTool()

	c, err := controller.New(log, (&countModel{}).call, controller.WithSessionUID("s"), controller.WithToolExecutor(tool.exec))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Exec(context.Background(), har, []api.Message{*api.TextMessage("user", "go")}, 0); err != nil {
		t.Fatalf("wire exec: %v", err)
	}
	if tool.effects[toolKey{"s", "k1"}] != 1 {
		t.Fatalf("host executed the tool %d times, want 1", tool.effects[toolKey{"s", "k1"}])
	}
	recs, _ := log.Read(1)
	var calls, results int
	for _, r := range recs {
		switch r.Event.Kind {
		case api.EventToolCall:
			calls++
		case api.EventToolResult:
			results++
		}
	}
	if calls != 1 || results != 1 {
		t.Fatalf("journal has %d TOOL_CALL / %d TOOL_RESULT, want 1/1", calls, results)
	}

	c2, err := controller.New(log, (&countModel{}).call, controller.WithSessionUID("s"), controller.WithToolExecutor(tool.exec))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c2.Replay(context.Background(), har); err != nil {
		t.Fatalf("wire replay: %v", err)
	}
	if tool.effects[toolKey{"s", "k1"}] != 1 {
		t.Fatalf("replay re-executed the tool: %d times, want 1 (I1)", tool.effects[toolKey{"s", "k1"}])
	}
	if err := log.Verify(); err != nil {
		t.Fatalf("verify: %v", err)
	}
}

// handlesModelErrorHarness handles a failed model call and completes the turn with its own output.
type handlesModelErrorHarness struct{}

func (handlesModelErrorHarness) Describe(context.Context) (api.Descriptor, error) {
	return api.Descriptor{ID: "handles-model-error"}, nil
}

func (handlesModelErrorHarness) Run(ctx context.Context, start *api.Start, sink api.EventSink) error {
	if _, err := sink.Model(ctx, api.ModelRequest{Model: "m", Messages: start.Inputs}); err == nil {
		return errors.New("model call succeeded; want the provider error")
	}
	return sink.Output(ctx, "handled")
}

// TestWireSinkErrorEndsInvocation pins EXE-33: an in-process harness can handle a sink error and
// complete the turn, but over Harness.Connect the host has no error reply frame, so the same
// harness's turn fails with the sink error and is journaled as EVENT_ERROR. An approval park is
// the exception (APR-10); TestApprovalRawProtocol covers it.
func TestWireSinkErrorEndsInvocation(t *testing.T) {
	errProvider := errors.New("provider unavailable")
	failing := func(context.Context, api.ModelRequest) (api.ModelResponse, error) {
		return api.ModelResponse{}, errProvider
	}
	for _, test := range []struct {
		name    string
		harness func(t *testing.T) api.Harness
		wantErr error
		want    []api.EventKind
	}{
		{
			name:    "in-process",
			harness: func(*testing.T) api.Harness { return handlesModelErrorHarness{} },
			want:    []api.EventKind{api.EventExecutionStart, api.EventInput, api.EventModelCall, api.EventOutput, api.EventEnd},
		},
		{
			name:    "wire",
			harness: func(t *testing.T) api.Harness { return wireHarnessFrom(t, handlesModelErrorHarness{}) },
			wantErr: errProvider,
			want:    []api.EventKind{api.EventExecutionStart, api.EventInput, api.EventModelCall, api.EventError},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			s, _ := openFile(t)
			defer s.Close()
			log := s.Session("s")
			c, err := controller.New(log, failing)
			if err != nil {
				t.Fatal(err)
			}
			err = c.Exec(t.Context(), test.harness(t), []api.Message{*api.TextMessage("user", "hi")}, 0)
			if test.wantErr == nil && err != nil {
				t.Fatalf("exec: %v", err)
			}
			if test.wantErr != nil && !errors.Is(err, test.wantErr) {
				t.Fatalf("exec error = %v; want %v", err, test.wantErr)
			}
			recs, err := log.Read(1)
			if err != nil {
				t.Fatal(err)
			}
			var got []api.EventKind
			for _, r := range recs {
				got = append(got, r.Event.Kind)
			}
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("journal kinds = %v; want %v", got, test.want)
			}
		})
	}
}
