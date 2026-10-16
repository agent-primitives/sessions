package harnesswire

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/agent-primitives/sessions/api"
	v1 "github.com/agent-primitives/sessions/api/genpb"
)

type cancellationConnectServer struct {
	v1.Harness_ConnectServer
	sent chan *v1.Event
}

func (s *cancellationConnectServer) Send(ev *v1.Event) error {
	s.sent <- ev
	return nil
}

func canceledStreamMethods(ctx context.Context, s *streamSink) []error {
	_, model := s.Model(ctx, api.ModelRequest{})
	_, tool := s.ToolCall(ctx, api.ToolCall{ID: "queued"})
	return []error{model, tool, s.Output(ctx, "late"), s.Report(ctx, api.ToolResult{}), s.Usage(ctx, api.Usage{})}
}

func TestStreamSinkQueuedCancellation(t *testing.T) {
	server := &cancellationConnectServer{sent: make(chan *v1.Event, 10)}
	results := make(chan *v1.ControllerFrame, 1)
	done := make(chan struct{})
	defer close(done)
	sink := &streamSink{stream: server, results: results, done: done, executionID: "e"}
	first := make(chan error, 1)
	go func() { _, err := sink.Model(t.Context(), api.ModelRequest{}); first <- err }()
	call := <-server.sent // The request is sent; its correlated reply is still withheld.
	ctx, cancel := context.WithCancel(t.Context())
	queued := make(chan []error, 1)
	go func() { queued <- canceledStreamMethods(ctx, sink) }()
	cancel()
	var errs []error
	select {
	case errs = <-queued:
	case <-time.After(time.Second):
		t.Error("canceled waiter blocked behind the host reply")
	}
	results <- &v1.ControllerFrame{ExecutionId: "e", Frame: &v1.ControllerFrame_Model{Model: &v1.ModelResult{ModelCallId: call.GetModel().GetId()}}}
	if err := <-first; err != nil {
		t.Fatalf("first reply lost correlation: %v", err)
	}
	if errs == nil {
		errs = <-queued
	}
	for _, err := range errs {
		if !errors.Is(err, context.Canceled) {
			t.Errorf("queued effect = %v, want canceled", err)
		}
	}
	if err := sink.Output(t.Context(), "after"); err != nil {
		t.Fatalf("canceled waiter poisoned the stream: %v", err)
	}
	if ev := <-server.sent; ev.GetKind() != v1.EventKind_EVENT_OUTPUT || len(server.sent) != 0 {
		t.Fatalf("canceled waiter sent an effect: %v", ev)
	}
}

func TestStreamSinkCanceledContextPreservesTerminalError(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	parked := &api.ApprovalParkedError{Ref: api.ApprovalRef{ExecutionID: "e", ToolCallID: "c", RequestSeq: 2}}
	failure := errors.New("protocol failure")
	for _, sink := range []*streamSink{{}, {parked: parked}, {failure: failure}} {
		want := context.Canceled
		if sink.parked != nil {
			want = parked
		}
		if sink.failure != nil {
			want = failure
		}
		for _, err := range canceledStreamMethods(ctx, sink) {
			if !errors.Is(err, want) {
				t.Errorf("got %v, want %v", err, want)
			}
		}
	}
}
