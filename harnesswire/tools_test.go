package harnesswire_test

import (
	"context"
	"errors"
	"net"
	"reflect"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	"github.com/agent-primitives/sessions/api"
	v1 "github.com/agent-primitives/sessions/api/genpb"
	"github.com/agent-primitives/sessions/harnesswire"
)

// dial serves srv over bufconn and returns a ClientHarness that drives it out of process.
func dial(t *testing.T, srv v1.HarnessServer) *harnesswire.ClientHarness {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	gs := grpc.NewServer()
	v1.RegisterHarnessServer(gs, srv)
	go gs.Serve(lis)
	t.Cleanup(gs.Stop)
	conn, err := grpc.NewClient(
		"passthrough:///harness",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return harnesswire.NewClientHarness(v1.NewHarnessClient(conn))
}

// hostSink is the host side of the stream: it records each model request and answers with resp.
type hostSink struct {
	resp api.ModelResponse
	reqs []api.ModelRequest
}

func (s *hostSink) Model(_ context.Context, req api.ModelRequest) (api.ModelResponse, error) {
	s.reqs = append(s.reqs, req)
	return s.resp, nil
}
func (*hostSink) Output(context.Context, string) error { return nil }
func (*hostSink) ToolCall(context.Context, api.ToolCall) (api.ToolResult, error) {
	return api.ToolResult{}, errors.New("unexpected tool call")
}
func (*hostSink) Report(context.Context, api.ToolResult) error { return nil }
func (*hostSink) Usage(context.Context, api.Usage) error       { return nil }

// modelHarness makes one model call with req and keeps the response it was served.
type modelHarness struct {
	req  api.ModelRequest
	got  *api.ModelResponse
	errs chan error
}

func (modelHarness) Describe(context.Context) (api.Descriptor, error) {
	return api.Descriptor{ID: "m"}, nil
}

func (h modelHarness) Run(ctx context.Context, _ *api.Start, sink api.EventSink) error {
	resp, err := sink.Model(ctx, h.req)
	if h.got != nil {
		*h.got = resp
	}
	if h.errs != nil {
		h.errs <- err
	}
	return err
}

// Tools, tool choice and tool parts cross Harness.Connect in both directions: the host sees the
// request the harness built (so its input hash matches an in-process run) and the harness gets the
// tool_call parts the model returned.
func TestModelCallCarriesToolsAcrossWire(t *testing.T) {
	req := api.ModelRequest{
		Model:  "m",
		Params: map[string]string{"temperature": "0"},
		Messages: []api.Message{
			{Role: "user", Parts: []api.Part{{Text: &api.TextPart{Text: "weather in Paris?"}}}},
			{Role: "assistant", Parts: []api.Part{{ToolCall: &api.ToolCall{ID: "c0", Tool: "get_weather", Args: map[string]any{"city": "Paris"}}}}},
			{Role: "tool", Parts: []api.Part{{ToolResult: &api.ToolResult{ID: "c0", Content: []api.Part{{Text: &api.TextPart{Text: "sunny"}}}}}}},
		},
		Tools: []api.ToolDefinition{
			{Name: "get_weather", Description: "Current weather", InputSchema: map[string]any{
				"type": "object", "properties": map[string]any{"city": map[string]any{"type": "string"}},
			}},
			{Name: "now"},
		},
		ToolChoice: &api.ToolChoice{Mode: api.ToolChoiceRequired, Name: "get_weather"},
	}
	served := api.ModelResponse{Message: api.Message{Role: "assistant", Parts: []api.Part{
		{ToolCall: &api.ToolCall{ID: "c1", Tool: "get_weather", Args: map[string]any{"city": "Rome", "days": float64(2)}}},
	}}}

	var got api.ModelResponse
	host := &hostSink{resp: served}
	h := dial(t, harnesswire.NewServer(modelHarness{req: req, got: &got}))
	if err := h.Run(t.Context(), &api.Start{ExecutionID: "e1"}, host); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(host.reqs) != 1 || !reflect.DeepEqual(host.reqs[0], req) {
		t.Fatalf("host saw %#v\nwant %#v", host.reqs, req)
	}
	if !reflect.DeepEqual(got.Message, served.Message) {
		t.Fatalf("harness was served %#v\nwant %#v", got.Message, served.Message)
	}
}

// The provider's usage crosses Harness.Connect with the completion, so a remote harness can report
// it; a completion with no usage reaches the harness as the zero Usage.
func TestModelResultCarriesUsageAcrossWire(t *testing.T) {
	for _, usage := range []api.Usage{
		{Model: "m", InputTokens: 12, OutputTokens: 5, ReasoningTokens: 2},
		{},
	} {
		served := api.ModelResponse{Message: *api.TextMessage("assistant", "hi"), Usage: usage}
		var got api.ModelResponse
		h := dial(t, harnesswire.NewServer(modelHarness{req: api.ModelRequest{Model: "m"}, got: &got}))
		if err := h.Run(t.Context(), &api.Start{ExecutionID: "e1"}, &hostSink{resp: served}); err != nil {
			t.Fatalf("Run: %v", err)
		}
		if got.Usage != usage {
			t.Fatalf("harness was served usage %+v, want %+v", got.Usage, usage)
		}
	}
}

// A harness-side tool choice this build cannot encode fails the call rather than going out as the
// provider default.
func TestModelCallRejectsUnknownToolChoiceFromHarness(t *testing.T) {
	errs := make(chan error, 1)
	host := &hostSink{}
	h := dial(t, harnesswire.NewServer(modelHarness{
		req:  api.ModelRequest{Model: "m", ToolChoice: &api.ToolChoice{Mode: "ANY"}},
		errs: errs,
	}))
	if err := h.Run(t.Context(), &api.Start{ExecutionID: "e1"}, host); err == nil {
		t.Fatal("Run succeeded with an unknown tool choice mode")
	}
	if err := <-errs; err == nil || !strings.Contains(err.Error(), "tool choice mode") {
		t.Fatalf("harness Model err = %v, want an unknown tool choice mode error", err)
	}
	if len(host.reqs) != 0 {
		t.Fatalf("host was asked for %d model calls, want 0", len(host.reqs))
	}
}

// rawModelCallServer sends one MODEL_CALL as given, for peers this build's streamSink cannot emit.
type rawModelCallServer struct {
	v1.UnimplementedHarnessServer
	call *v1.ModelCall
}

func (s rawModelCallServer) Connect(stream v1.Harness_ConnectServer) error {
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	if err := stream.Send(&v1.Event{
		ExecutionId: first.GetExecutionId(),
		Kind:        v1.EventKind_EVENT_MODEL_CALL,
		Body:        &v1.Event_Model{Model: s.call},
	}); err != nil {
		return err
	}
	_, err = stream.Recv() // hold the stream open until the host hangs up
	return err
}

// Proto3 enums are open, so a newer harness can send a tool choice mode this host does not know. The
// host refuses the call instead of serving it with the provider default.
func TestModelCallRejectsUnknownToolChoiceFromPeer(t *testing.T) {
	host := &hostSink{}
	h := dial(t, rawModelCallServer{call: &v1.ModelCall{
		Model: "m", Id: "mc-1", ToolChoice: &v1.ToolChoice{Mode: v1.ToolChoice_Mode(99)},
	}})
	err := h.Run(t.Context(), &api.Start{ExecutionID: "e1"}, host)
	if err == nil || !strings.Contains(err.Error(), "unknown tool choice mode 99") {
		t.Fatalf("Run err = %v, want an unknown tool choice mode error", err)
	}
	if len(host.reqs) != 0 {
		t.Fatalf("host served %d model calls, want 0", len(host.reqs))
	}
}

var errHostSink = errors.New("host sink failed")

// failingSink fails the one sink call named by kind, as a provider outage or store fault would.
type failingSink struct{ kind string }

func (s failingSink) fail(kind string) error {
	if s.kind == kind {
		return errHostSink
	}
	return nil
}
func (s failingSink) Model(context.Context, api.ModelRequest) (api.ModelResponse, error) {
	return api.ModelResponse{}, s.fail("model")
}
func (s failingSink) Output(context.Context, string) error { return s.fail("output") }
func (s failingSink) ToolCall(context.Context, api.ToolCall) (api.ToolResult, error) {
	return api.ToolResult{}, s.fail("tool_call")
}
func (s failingSink) Report(context.Context, api.ToolResult) error { return s.fail("report") }
func (s failingSink) Usage(context.Context, api.Usage) error       { return s.fail("usage") }

// lingeringHarness makes one sink call of its kind and then keeps the RPC open until released,
// ignoring cancellation, as a harness still flushing its telemetry would.
type lingeringHarness struct {
	kind    string
	release chan struct{}
}

func (lingeringHarness) Describe(context.Context) (api.Descriptor, error) {
	return api.Descriptor{ID: "lingering"}, nil
}
func (h lingeringHarness) Run(ctx context.Context, _ *api.Start, sink api.EventSink) error {
	switch h.kind {
	case "model":
		_, _ = sink.Model(ctx, api.ModelRequest{Model: "m"})
	case "output":
		_ = sink.Output(ctx, "out")
	case "tool_call":
		_, _ = sink.ToolCall(ctx, api.ToolCall{ID: "c", Tool: "t", IdempotencyKey: "k"})
	case "report":
		_ = sink.Report(ctx, api.ToolResult{ID: "c"})
	case "usage":
		_ = sink.Usage(ctx, api.Usage{Model: "m", InputTokens: 1})
	}
	<-h.release
	return nil
}

// TestRunWaitsForHarnessAfterHostSinkFailure checks that a turn failed by any host sink call
// lets the harness end the RPC, so it can flush its spans, and still returns the host's error.
func TestRunWaitsForHarnessAfterHostSinkFailure(t *testing.T) {
	for _, kind := range []string{"model", "output", "tool_call", "report", "usage"} {
		t.Run(kind, func(t *testing.T) {
			release := make(chan struct{})
			client := dial(t, harnesswire.NewServer(lingeringHarness{kind: kind, release: release}))
			done := make(chan error, 1)
			go func() {
				done <- client.Run(context.Background(), &api.Start{ExecutionID: "e-1", SessionUID: "s"}, failingSink{kind: kind})
			}()
			select {
			case err := <-done:
				close(release)
				t.Fatalf("Run returned (%v) while the harness was still running", err)
			case <-time.After(300 * time.Millisecond):
			}
			close(release)
			select {
			case err := <-done:
				if !errors.Is(err, errHostSink) {
					t.Fatalf("Run: %v, want the host sink error", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("Run did not return after the harness ended the RPC")
			}
		})
	}
}
