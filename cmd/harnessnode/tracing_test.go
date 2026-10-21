package main

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	"github.com/agent-primitives/sessions/api"
	v1 "github.com/agent-primitives/sessions/api/genpb"
	"github.com/agent-primitives/sessions/harnesswire"
)

// blockingExporter reports each batch and then blocks until released, standing in for a slow
// collector.
type blockingExporter struct {
	batches chan []string
	release chan struct{}
}

func (e *blockingExporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	names := make([]string, 0, len(spans))
	for _, s := range spans {
		names = append(names, s.Name())
	}
	e.batches <- names
	select {
	case <-e.release:
	case <-ctx.Done():
	}
	return nil
}

func (*blockingExporter) Shutdown(context.Context) error { return nil }

type quietHarness struct{}

func (quietHarness) Describe(context.Context) (api.Descriptor, error) {
	return api.Descriptor{ID: "quiet"}, nil
}
func (quietHarness) Run(context.Context, *api.Start, api.EventSink) error { return nil }

// parkingHarness makes one approval-gated call, which the host parks.
type parkingHarness struct{}

func (parkingHarness) Describe(context.Context) (api.Descriptor, error) {
	return api.Descriptor{ID: "parking"}, nil
}
func (parkingHarness) Run(ctx context.Context, _ *api.Start, sink api.EventSink) error {
	_, err := sink.ToolCall(ctx, api.ToolCall{ID: "c", Tool: "t", IdempotencyKey: "k", Mediation: api.MediationRequiresApproval})
	return err
}

var parkRef = api.ApprovalRef{ExecutionID: "e-1", ToolCallID: "c", RequestSeq: 1}

type discardSink struct{}

func (discardSink) Model(context.Context, api.ModelRequest) (api.ModelResponse, error) {
	return api.ModelResponse{}, nil
}
func (discardSink) Output(context.Context, string) error { return nil }
func (discardSink) ToolCall(_ context.Context, tc api.ToolCall) (api.ToolResult, error) {
	if tc.Mediation == api.MediationRequiresApproval {
		return api.ToolResult{}, &api.ApprovalParkedError{Ref: parkRef}
	}
	return api.ToolResult{}, nil
}
func (discardSink) Report(context.Context, api.ToolResult) error { return nil }
func (discardSink) Usage(context.Context, api.Usage) error       { return nil }

// TestConnectSpanExportedBeforeTurnEnds checks the ordering the host relies on to suspend a harness
// right after a turn: by the time Run returns, the harness's Connect span, the root of its side of
// the turn, has been handed to the exporter. The exporter blocks, and Run must still be waiting.
// A parked turn ends the same way: the host waits for the harness to close the stream.
func TestConnectSpanExportedBeforeTurnEnds(t *testing.T) {
	for _, tc := range []struct {
		name    string
		harness api.Harness
		wantErr func(error) bool
	}{
		{name: "end", harness: quietHarness{}, wantErr: func(err error) bool { return err == nil }},
		{name: "parked", harness: parkingHarness{}, wantErr: func(err error) bool {
			var parked *api.ApprovalParkedError
			return errors.As(err, &parked) && parked.Ref == parkRef
		}},
	} {
		t.Run(tc.name, func(t *testing.T) { testConnectSpanExported(t, tc.harness, tc.wantErr) })
	}
}

func testConnectSpanExported(t *testing.T, h api.Harness, wantErr func(error) bool) {
	exp := &blockingExporter{batches: make(chan []string, 16), release: make(chan struct{})}
	// A long batch timeout, so only an explicit flush exports anything.
	tp := sdktrace.NewTracerProvider(sdktrace.WithBatcher(exp, sdktrace.WithBatchTimeout(time.Hour)))
	prevTP, prevProp := otel.GetTracerProvider(), otel.GetTextMapPropagator()
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() {
		close(exp.release)
		_ = tp.Shutdown(context.Background())
		otel.SetTracerProvider(prevTP)
		otel.SetTextMapPropagator(prevProp)
	})

	lis := bufconn.Listen(1 << 20)
	srv := newServer(h)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	conn, err := grpc.NewClient("passthrough:///harness",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	client := harnesswire.NewClientHarness(v1.NewHarnessClient(conn))

	done := make(chan error, 1)
	go func() {
		done <- client.Run(context.Background(), &api.Start{ExecutionID: "e-1", SessionUID: "s"}, discardSink{})
	}()

	var batch []string
	select {
	case batch = <-exp.batches:
	case err := <-done:
		t.Fatalf("Run returned (%v) before the harness exported any spans", err)
	case <-time.After(5 * time.Second):
		t.Fatal("the harness exported nothing")
	}
	select {
	case err := <-done:
		t.Fatalf("Run returned (%v) while the harness's export was still blocked", err)
	case <-time.After(100 * time.Millisecond):
	}
	exp.release <- struct{}{}
	if err := <-done; !wantErr(err) {
		t.Fatalf("Run: %v", err)
	}

	var connect bool
	for _, name := range batch {
		connect = connect || name == "agentsessions.v1.Harness/Connect"
	}
	if !connect {
		t.Fatalf("flushed batch %v is missing the Connect span", batch)
	}
}
