package session_test

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/agent-primitives/sessions/api"
	v1 "github.com/agent-primitives/sessions/api/genpb"
	"github.com/agent-primitives/sessions/harness/echoagent"
	"github.com/agent-primitives/sessions/harnesswire"
	"github.com/agent-primitives/sessions/placement"
	"github.com/agent-primitives/sessions/runtime/local"
	"github.com/agent-primitives/sessions/session"
	"github.com/agent-primitives/sessions/sqlitelog"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type retryConnectClient struct {
	v1.HarnessClient
	connects atomic.Int32
	starts   []*v1.Start
	firstErr error
}

func (c *retryConnectClient) Describe(ctx context.Context, _ *v1.DescribeRequest, _ ...grpc.CallOption) (*v1.HarnessDescriptor, error) {
	desc, err := (echoagent.Harness{}).Describe(ctx)
	if err != nil {
		return nil, err
	}
	return harnesswire.DescriptorToProto(desc), nil
}

func (c *retryConnectClient) Connect(context.Context, ...grpc.CallOption) (grpc.BidiStreamingClient[v1.ControllerFrame, v1.Event], error) {
	if c.connects.Add(1) == 1 {
		return nil, c.firstErr
	}
	return &retryConnectStream{client: c}, nil
}

type retryConnectStream struct {
	grpc.ClientStream
	client      *retryConnectClient
	executionID string
	ended       bool
}

func (s *retryConnectStream) Send(frame *v1.ControllerFrame) error {
	start := frame.GetStart()
	if start == nil {
		return errors.New("expected Start")
	}
	s.executionID = frame.GetExecutionId()
	s.client.starts = append(s.client.starts, start)
	return nil
}

func (s *retryConnectStream) Recv() (*v1.Event, error) {
	if s.ended {
		return nil, io.EOF
	}
	s.ended = true
	return &v1.Event{ExecutionId: s.executionID, Kind: v1.EventKind_EVENT_END, Body: &v1.Event_End{End: &v1.HarnessEnd{State: "COMPLETED"}}}, nil
}

func (*retryConnectStream) CloseSend() error { return nil }

type postEndClient struct{ v1.HarnessClient }

func (postEndClient) Describe(ctx context.Context, _ *v1.DescribeRequest, _ ...grpc.CallOption) (*v1.HarnessDescriptor, error) {
	desc, err := (echoagent.Harness{}).Describe(ctx)
	if err != nil {
		return nil, err
	}
	return harnesswire.DescriptorToProto(desc), nil
}

func (postEndClient) Connect(context.Context, ...grpc.CallOption) (grpc.BidiStreamingClient[v1.ControllerFrame, v1.Event], error) {
	return &postEndStream{}, nil
}

type postEndStream struct {
	grpc.ClientStream
	executionID string
	step        int
}

func (s *postEndStream) Send(frame *v1.ControllerFrame) error {
	s.executionID = frame.GetExecutionId()
	return nil
}

func (s *postEndStream) Recv() (*v1.Event, error) {
	s.step++
	switch s.step {
	case 1:
		return &v1.Event{ExecutionId: s.executionID, Kind: v1.EventKind_EVENT_END, Body: &v1.Event_End{End: &v1.HarnessEnd{State: "COMPLETED"}}}, nil
	case 2:
		return &v1.Event{ExecutionId: s.executionID, Kind: v1.EventKind_EVENT_ERROR, Body: &v1.Event_Error{Error: &v1.Error{Description: "contradictory"}}}, nil
	default:
		return nil, io.EOF
	}
}

func (*postEndStream) CloseSend() error { return nil }

func TestExecConnectPreparationFailureCanRetryFirstTurn(t *testing.T) {
	store, err := sqlitelog.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	backend := local.New(echoagent.Harness{})
	t.Cleanup(func() { _ = backend.Close() })
	const private = "https://private.invalid/?token=connect-secret"
	cause := status.Error(codes.Unavailable, private)
	client := &retryConnectClient{firstErr: cause}
	p := placement.New(backend, echoagent.Model, placement.WithDialer(func(api.Incarnation) (api.Harness, func() error, error) {
		return harnesswire.NewClientHarness(client), func() error { return nil }, nil
	}))
	registry, err := placement.NewRegistry("echo", map[string]*placement.Placer{"echo": p})
	if err != nil {
		t.Fatal(err)
	}
	svc := session.NewService(store, registry)
	created, err := svc.CreateSession(t.Context(), &v1.CreateSessionRequest{})
	if err != nil {
		t.Fatal(err)
	}
	uid := created.GetMetadata().GetUid()
	req := &v1.ExecRequest{Session: uid, Inputs: []*v1.Message{{Role: "user", Parts: []*v1.Part{{Part: &v1.Part_Text{Text: &v1.TextPart{Text: "hi"}}}}}}}

	err = svc.Exec(req, &rpcStream{ctx: t.Context()})
	if status.Code(err) != codes.Unavailable || !errors.Is(err, placement.ErrHarnessUnavailable) || !errors.Is(err, cause) {
		t.Fatalf("first Exec=%v, want UNAVAILABLE/HARNESS_UNAVAILABLE", err)
	}
	if strings.Contains(err.Error(), private) {
		t.Fatalf("Connect diagnostic leaked into public status: %v", err)
	}
	details := status.Convert(err).Details()
	if len(details) != 1 {
		t.Fatalf("details=%v, want HARNESS_UNAVAILABLE", details)
	}
	info, ok := details[0].(*errdetails.ErrorInfo)
	if !ok || info.Domain != "agentsessions" || info.Reason != "HARNESS_UNAVAILABLE" || len(info.Metadata) != 0 {
		t.Fatalf("details=%v, want agentsessions/HARNESS_UNAVAILABLE", details)
	}
	if head, headErr := store.Session(uid).Head(); headErr != nil || head != 0 {
		t.Fatalf("head=%d err=%v, want no failed execution", head, headErr)
	}

	if err := svc.Exec(req, &rpcStream{ctx: t.Context()}); err != nil {
		t.Fatalf("retry Exec=%v", err)
	}
	if client.connects.Load() != 2 || len(client.starts) != 1 || len(client.starts[0].GetHistory()) != 0 {
		t.Fatalf("connects=%d starts=%v, want one failed Connect and one first-turn Start", client.connects.Load(), client.starts)
	}
}

type unavailableRunHarness struct{ echoagent.Harness }

func (h unavailableRunHarness) PrepareRun(context.Context) (api.PreparedHarnessRun, error) {
	return unavailablePreparedRun{}, nil
}

type unavailablePreparedRun struct{}

func (unavailablePreparedRun) Run(context.Context, *api.Start, api.EventSink) error {
	return status.Error(codes.Unavailable, "delivery outcome unknown")
}

func (unavailablePreparedRun) Close() {}

func TestExecPreparedRunFailureRemainsDurableAndUnclassified(t *testing.T) {
	store, err := sqlitelog.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	backend := local.New(echoagent.Harness{})
	t.Cleanup(func() { _ = backend.Close() })
	p := placement.New(backend, echoagent.Model, placement.WithDialer(func(api.Incarnation) (api.Harness, func() error, error) {
		return unavailableRunHarness{}, func() error { return nil }, nil
	}))
	registry, err := placement.NewRegistry("echo", map[string]*placement.Placer{"echo": p})
	if err != nil {
		t.Fatal(err)
	}
	svc := session.NewService(store, registry)
	created, err := svc.CreateSession(t.Context(), &v1.CreateSessionRequest{})
	if err != nil {
		t.Fatal(err)
	}
	uid := created.GetMetadata().GetUid()
	err = svc.Exec(&v1.ExecRequest{Session: uid}, &rpcStream{ctx: t.Context()})
	if status.Code(err) != codes.Internal || errors.Is(err, placement.ErrHarnessUnavailable) {
		t.Fatalf("Exec=%v, want INTERNAL without HARNESS_UNAVAILABLE", err)
	}
	if len(status.Convert(err).Details()) != 0 {
		t.Fatalf("uncertain Run failure acquired ErrorInfo: %v", status.Convert(err).Details())
	}
	recs, readErr := store.Session(uid).Read(1)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(recs) != 2 || recs[0].Event.Kind != api.EventExecutionStart || recs[1].Event.Kind != api.EventError {
		t.Fatalf("records=%+v, want durable start and error", recs)
	}
}

func TestExecFrameAfterEndIsDurableProtocolFailure(t *testing.T) {
	store, err := sqlitelog.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	backend := local.New(echoagent.Harness{})
	t.Cleanup(func() { _ = backend.Close() })
	p := placement.New(backend, echoagent.Model, placement.WithDialer(func(api.Incarnation) (api.Harness, func() error, error) {
		return harnesswire.NewClientHarness(postEndClient{}), func() error { return nil }, nil
	}))
	registry, err := placement.NewRegistry("echo", map[string]*placement.Placer{"echo": p})
	if err != nil {
		t.Fatal(err)
	}
	svc := session.NewService(store, registry)
	created, err := svc.CreateSession(t.Context(), &v1.CreateSessionRequest{})
	if err != nil {
		t.Fatal(err)
	}
	uid := created.GetMetadata().GetUid()
	err = svc.Exec(&v1.ExecRequest{Session: uid}, &rpcStream{ctx: t.Context()})
	if status.Code(err) != codes.Internal {
		t.Fatalf("Exec=%v, want INTERNAL protocol failure", err)
	}
	recs, readErr := store.Session(uid).Read(1)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(recs) != 2 || recs[0].Event.Kind != api.EventExecutionStart || recs[1].Event.Kind != api.EventError {
		t.Fatalf("records=%+v, want durable start and error without END", recs)
	}
}
