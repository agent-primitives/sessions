package harnesswire

import (
	"context"
	"errors"
	"io"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/agent-primitives/sessions/api"
	v1 "github.com/agent-primitives/sessions/api/genpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

type prepareClient struct {
	v1.HarnessClient
	stream   *prepareClientStream
	err      error
	connects int
	ctx      context.Context
}

func (c *prepareClient) Connect(ctx context.Context, _ ...grpc.CallOption) (grpc.BidiStreamingClient[v1.ControllerFrame, v1.Event], error) {
	c.connects++
	c.ctx = ctx
	if c.err != nil {
		return nil, c.err
	}
	return c.stream, nil
}

type prepareClientStream struct {
	grpc.ClientStream
	sent      []*v1.ControllerFrame
	events    []*v1.Event
	sendErr   error
	recvErr   error
	closeSend int
}

type blockingPreparedStream struct {
	grpc.ClientStream
	ctx  context.Context
	sent chan struct{}
}

type blockingReconstructionStream struct {
	grpc.ClientStream
	ctx context.Context
}

type bufferedBlockingReconstructionStream struct {
	grpc.ClientStream
	ctx      context.Context
	metadata bool
}

func (*bufferedBlockingReconstructionStream) Send(*v1.ControllerFrame) error { return io.EOF }
func (s *bufferedBlockingReconstructionStream) Recv() (*v1.Event, error) {
	if !s.metadata {
		s.metadata = true
		return reconstructionInfo(v1.HarnessClass_HARNESS_CLASS_MEDIATED), nil
	}
	<-s.ctx.Done()
	return nil, s.ctx.Err()
}
func (*bufferedBlockingReconstructionStream) CloseSend() error { return nil }

func (*blockingReconstructionStream) Send(*v1.ControllerFrame) error { return nil }
func (s *blockingReconstructionStream) Recv() (*v1.Event, error) {
	<-s.ctx.Done()
	return nil, s.ctx.Err()
}
func (*blockingReconstructionStream) CloseSend() error { return nil }

func (s *blockingPreparedStream) Send(*v1.ControllerFrame) error {
	close(s.sent)
	return nil
}

func (s *blockingPreparedStream) Recv() (*v1.Event, error) {
	<-s.ctx.Done()
	return nil, s.ctx.Err()
}

func (*blockingPreparedStream) CloseSend() error { return nil }

func (s *prepareClientStream) Send(frame *v1.ControllerFrame) error {
	s.sent = append(s.sent, frame)
	return s.sendErr
}

func (s *prepareClientStream) Recv() (*v1.Event, error) {
	if len(s.events) > 0 {
		ev := s.events[0]
		s.events = s.events[1:]
		return ev, nil
	}
	if s.recvErr != nil {
		return nil, s.recvErr
	}
	return nil, io.EOF
}

func (s *prepareClientStream) CloseSend() error {
	s.closeSend++
	return nil
}

type noOpSink struct{}

func (noOpSink) Model(context.Context, api.ModelRequest) (api.ModelResponse, error) {
	return api.ModelResponse{}, nil
}
func (noOpSink) Output(context.Context, string) error { return nil }
func (noOpSink) ToolCall(context.Context, api.ToolCall) (api.ToolResult, error) {
	return api.ToolResult{}, nil
}
func (noOpSink) Report(context.Context, api.ToolResult) error { return nil }
func (noOpSink) Usage(context.Context, api.Usage) error       { return nil }

type blockingModelSink struct {
	noOpSink
	started chan struct{}
}

func (s blockingModelSink) Model(ctx context.Context, _ api.ModelRequest) (api.ModelResponse, error) {
	close(s.started)
	<-ctx.Done()
	return api.ModelResponse{}, ctx.Err()
}

func TestPrepareRunOpensAndReusesOneConnectStream(t *testing.T) {
	stream := &prepareClientStream{events: []*v1.Event{{ExecutionId: "e", Kind: v1.EventKind_EVENT_END, Body: &v1.Event_End{End: &v1.HarnessEnd{State: "COMPLETED"}}}}}
	client := &prepareClient{stream: stream}
	h := NewClientHarness(client)
	prepared, err := h.PrepareRun(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.Close()
	if client.connects != 1 || len(stream.sent) != 0 {
		t.Fatalf("after PrepareRun: connects=%d sent=%d, want 1/0", client.connects, len(stream.sent))
	}
	if err := prepared.Run(t.Context(), &api.Start{SessionUID: "s", ExecutionID: "e"}, noOpSink{}); err != nil {
		t.Fatal(err)
	}
	if client.connects != 1 || len(stream.sent) != 1 || stream.sent[0].GetStart() == nil {
		t.Fatalf("after Run: connects=%d sent=%v, want one Start on prepared stream", client.connects, stream.sent)
	}
	if err := prepared.Run(t.Context(), &api.Start{SessionUID: "s", ExecutionID: "e"}, noOpSink{}); err == nil {
		t.Fatal("second prepared Run succeeded")
	}
	if client.connects != 1 || len(stream.sent) != 1 {
		t.Fatalf("second Run opened or sent again: connects=%d sent=%d", client.connects, len(stream.sent))
	}
}

func TestPrepareRunFailureSendsNothing(t *testing.T) {
	cause := errors.New("connect unavailable")
	client := &prepareClient{err: cause}
	prepared, err := NewClientHarness(client).PrepareRun(t.Context())
	if !errors.Is(err, cause) || prepared != nil || client.connects != 1 {
		t.Fatalf("PrepareRun=(%v, %v), connects=%d; want nil, %v, 1", prepared, err, client.connects, cause)
	}
}

func reconstructionInfo(class v1.HarnessClass) *v1.Event {
	return &v1.Event{Kind: v1.EventKind_EVENT_RECONSTRUCTION_INFO,
		Body: &v1.Event_ReconstructionInfo{ReconstructionInfo: &v1.ReconstructionInfo{HarnessClass: class}}}
}

func TestPrepareReconstructionClassHandshake(t *testing.T) {
	for _, tc := range []struct {
		name  string
		event *v1.Event
		err   error
		ok    bool
	}{
		{name: "mediated", event: reconstructionInfo(v1.HarnessClass_HARNESS_CLASS_MEDIATED), ok: true},
		{name: "legacy unspecified", event: reconstructionInfo(v1.HarnessClass_HARNESS_CLASS_UNSPECIFIED), ok: true},
		{name: "unmediated", event: reconstructionInfo(v1.HarnessClass_HARNESS_CLASS_UNMEDIATED), err: api.ErrReconstructionUnsupported},
		{name: "unknown", event: reconstructionInfo(v1.HarnessClass(99)), err: api.ErrReconstructionUnsupported},
		{name: "missing payload", event: &v1.Event{Kind: v1.EventKind_EVENT_RECONSTRUCTION_INFO}, err: api.ErrReconstructionUnsupported},
		{name: "old server rejection", err: api.ErrReconstructionUnsupported},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stream := &prepareClientStream{}
			if tc.event != nil {
				stream.events = []*v1.Event{tc.event}
			}
			prepared, err := NewClientHarness(&prepareClient{stream: stream}).prepareReconstruction(t.Context())
			if !errors.Is(err, tc.err) || (prepared != nil) != tc.ok {
				t.Fatalf("PrepareReconstruction = (%v, %v), want prepared=%v err=%v", prepared, err, tc.ok, tc.err)
			}
			if len(stream.sent) != 1 || stream.sent[0].GetReconstructionProbe() == nil || stream.sent[0].GetSession() != "" || stream.sent[0].GetExecutionId() != "" {
				t.Fatalf("probe = %+v, want one explicit reconstruction probe", stream.sent)
			}
			if prepared != nil {
				prepared.Close()
			}
		})
	}
}

func TestPrepareReconstructionPreservesFailureClassification(t *testing.T) {
	t.Run("caller cancellation", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		client := &prepareClient{stream: &prepareClientStream{recvErr: context.Canceled}}
		cancel()
		if _, err := NewClientHarness(client).prepareReconstruction(ctx); !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled handshake = %v", err)
		}
	})

	t.Run("unavailable", func(t *testing.T) {
		cause := status.Error(codes.Unavailable, "peer down")
		stream := &prepareClientStream{recvErr: cause}
		if _, err := NewClientHarness(&prepareClient{stream: stream}).prepareReconstruction(t.Context()); !errors.Is(err, cause) || errors.Is(err, api.ErrReconstructionUnsupported) {
			t.Fatalf("unavailable handshake = %v", err)
		}
	})

	t.Run("send EOF uses final status", func(t *testing.T) {
		cause := status.Error(codes.Unavailable, "peer down")
		stream := &prepareClientStream{sendErr: io.EOF, recvErr: cause}
		_, err := NewClientHarness(&prepareClient{stream: stream}).prepareReconstruction(t.Context())
		if !errors.Is(err, api.ErrReconstructionHandshake) || status.Code(err) != codes.Unavailable || errors.Is(err, api.ErrReconstructionUnsupported) {
			t.Fatalf("send EOF handshake = %v", err)
		}
	})

	t.Run("send EOF drains buffered metadata", func(t *testing.T) {
		cause := status.Error(codes.Unavailable, "peer down")
		stream := &prepareClientStream{sendErr: io.EOF, events: []*v1.Event{reconstructionInfo(v1.HarnessClass_HARNESS_CLASS_MEDIATED)}, recvErr: cause}
		_, err := NewClientHarness(&prepareClient{stream: stream}).prepareReconstruction(t.Context())
		if !errors.Is(err, api.ErrReconstructionHandshake) || status.Code(err) != codes.Unavailable || errors.Is(err, api.ErrReconstructionUnsupported) {
			t.Fatalf("buffered send EOF handshake = %v", err)
		}
	})

	t.Run("send EOF buffered drain is bounded", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		stream := &bufferedBlockingReconstructionStream{ctx: ctx}
		client := blockingConnectClient{stream: stream, setContext: func(got context.Context) { stream.ctx = got }}
		h := NewClientHarness(client)
		h.reconstructionTimeout = 10 * time.Millisecond
		start := time.Now()
		_, err := h.prepareReconstruction(ctx)
		cancel()
		if !errors.Is(err, api.ErrReconstructionHandshake) || !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > time.Second {
			t.Fatalf("bounded buffered drain = %v after %v", err, time.Since(start))
		}
	})

	t.Run("bounded", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		stream := &blockingReconstructionStream{ctx: ctx}
		client := blockingConnectClient{stream: stream, setContext: func(got context.Context) { stream.ctx = got }}
		h := NewClientHarness(client)
		h.reconstructionTimeout = 10 * time.Millisecond
		start := time.Now()
		_, err := h.prepareReconstruction(ctx)
		cancel()
		if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > time.Second {
			t.Fatalf("bounded handshake = %v after %v", err, time.Since(start))
		}
	})
}

type blockingConnectClient struct {
	v1.HarnessClient
	stream     grpc.BidiStreamingClient[v1.ControllerFrame, v1.Event]
	setContext func(context.Context)
}

func (c blockingConnectClient) Connect(ctx context.Context, _ ...grpc.CallOption) (grpc.BidiStreamingClient[v1.ControllerFrame, v1.Event], error) {
	c.setContext(ctx)
	return c.stream, nil
}

func TestPrepareRunMissingUnixSocketFailsBeforeStart(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing.sock")
	conn, err := grpc.NewClient(
		"passthrough:///missing-harness",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", missing)
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	prepared, err := NewClientHarness(v1.NewHarnessClient(conn)).PrepareRun(ctx)
	if err == nil || prepared != nil {
		t.Fatalf("PrepareRun=(%v, %v), want pre-Start Connect failure", prepared, err)
	}
}

func TestPreparedRunCloseCancelsUnusedConnect(t *testing.T) {
	client := &prepareClient{stream: &prepareClientStream{}}
	prepared, err := NewClientHarness(client).PrepareRun(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	prepared.Close()
	prepared.Close()
	select {
	case <-client.ctx.Done():
	default:
		t.Fatal("Close did not cancel prepared Connect")
	}
	if len(client.stream.sent) != 0 {
		t.Fatal("Close sent a frame")
	}
	if err := prepared.Run(t.Context(), &api.Start{ExecutionID: "e"}, noOpSink{}); err == nil {
		t.Fatal("Run after Close succeeded")
	}
}

func TestPreparedRunCloseCancelsActiveRun(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	stream := &blockingPreparedStream{ctx: ctx, sent: make(chan struct{})}
	prepared := &preparedClientRun{stream: stream, ctx: ctx, cancel: cancel}
	done := make(chan error, 1)
	go func() {
		done <- prepared.Run(ctx, &api.Start{ExecutionID: "e"}, noOpSink{})
	}()
	<-stream.sent
	prepared.Close()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run=%v, want Close cancellation", err)
	}
}

func TestPreparedRunHonorsIndependentRunCancellation(t *testing.T) {
	prepareCtx, prepareCancel := context.WithCancel(context.Background())
	stream := &blockingPreparedStream{ctx: prepareCtx, sent: make(chan struct{})}
	prepared := &preparedClientRun{stream: stream, ctx: prepareCtx, cancel: prepareCancel}
	runCtx, runCancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- prepared.Run(runCtx, &api.Start{ExecutionID: "e"}, noOpSink{})
	}()
	<-stream.sent
	runCancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run=%v, want Run context cancellation", err)
	}
}

func TestPreparedRunCloseCancelsBlockedSinkWithIndependentContext(t *testing.T) {
	prepareCtx, prepareCancel := context.WithCancel(context.Background())
	stream := &prepareClientStream{events: []*v1.Event{{
		ExecutionId: "e",
		Kind:        v1.EventKind_EVENT_MODEL_CALL,
		Body:        &v1.Event_Model{Model: &v1.ModelCall{Id: "m", Model: "model"}},
	}}}
	prepared := &preparedClientRun{stream: stream, ctx: prepareCtx, cancel: prepareCancel}
	sink := blockingModelSink{started: make(chan struct{})}
	done := make(chan error, 1)
	go func() {
		done <- prepared.Run(context.Background(), &api.Start{ExecutionID: "e"}, sink)
	}()
	<-sink.started
	prepared.Close()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run=%v, want Close cancellation", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not cancel blocked sink call")
	}
}

func TestClientRunInvalidStartDoesNotConnect(t *testing.T) {
	client := &prepareClient{stream: &prepareClientStream{}}
	err := NewClientHarness(client).Run(t.Context(), &api.Start{}, noOpSink{})
	if err == nil || client.connects != 0 {
		t.Fatalf("Run=%v connects=%d, want local validation before Connect", err, client.connects)
	}
}

func TestPreparedSendAndRecvFailuresAreRunFailures(t *testing.T) {
	for _, tc := range []struct {
		name   string
		stream *prepareClientStream
		cause  error
	}{
		{name: "send", cause: errors.New("send uncertain")},
		{name: "recv", cause: errors.New("recv uncertain")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stream := &prepareClientStream{}
			if tc.name == "send" {
				stream.sendErr = tc.cause
			} else {
				stream.recvErr = tc.cause
			}
			prepared, err := NewClientHarness(&prepareClient{stream: stream}).PrepareRun(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer prepared.Close()
			err = prepared.Run(t.Context(), &api.Start{ExecutionID: "e"}, noOpSink{})
			if !errors.Is(err, tc.cause) {
				t.Fatalf("Run=%v, want %v", err, tc.cause)
			}
		})
	}
}

func TestPreparedRunRequiresEndAndRejectsUnsupportedEvents(t *testing.T) {
	for _, tc := range []struct {
		name   string
		events []*v1.Event
	}{
		{name: "EOF before END"},
		{name: "host-owned error", events: []*v1.Event{{ExecutionId: "e", Kind: v1.EventKind_EVENT_ERROR, Body: &v1.Event_Error{Error: &v1.Error{Description: "peer error"}}}}},
		{name: "unspecified", events: []*v1.Event{{ExecutionId: "e", Kind: v1.EventKind_EVENT_KIND_UNSPECIFIED}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stream := &prepareClientStream{events: tc.events}
			prepared, err := NewClientHarness(&prepareClient{stream: stream}).PrepareRun(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer prepared.Close()
			if err := prepared.Run(t.Context(), &api.Start{ExecutionID: "e"}, noOpSink{}); err == nil {
				t.Fatal("Run accepted stream without EVENT_END")
			}
		})
	}
}

func TestPreparedRunRejectsFramesAfterEnd(t *testing.T) {
	for _, ev := range []*v1.Event{
		{ExecutionId: "e", Kind: v1.EventKind_EVENT_ERROR, Body: &v1.Event_Error{Error: &v1.Error{Description: "peer error"}}},
		{ExecutionId: "e", Kind: v1.EventKind_EVENT_KIND_UNSPECIFIED},
		{ExecutionId: "e", Kind: v1.EventKind_EVENT_END, Body: &v1.Event_End{End: &v1.HarnessEnd{State: "COMPLETED"}}},
	} {
		stream := &prepareClientStream{events: []*v1.Event{
			{ExecutionId: "e", Kind: v1.EventKind_EVENT_END, Body: &v1.Event_End{End: &v1.HarnessEnd{State: "COMPLETED"}}},
			ev,
		}}
		prepared, err := NewClientHarness(&prepareClient{stream: stream}).PrepareRun(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if err := prepared.Run(t.Context(), &api.Start{ExecutionID: "e"}, noOpSink{}); err == nil {
			t.Fatalf("Run accepted %s after EVENT_END", ev.GetKind())
		}
		prepared.Close()
	}
}
