package ateadapter

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net"
	"strings"
	"testing"

	"github.com/agent-primitives/sessions/api"
	v1 "github.com/agent-primitives/sessions/api/genpb"
	"github.com/agent-primitives/sessions/harness/echoagent"
	"github.com/agent-primitives/sessions/placement"
	"github.com/agent-primitives/sessions/runtime/substrate"
	"github.com/agent-primitives/sessions/session"
	"github.com/agent-primitives/sessions/sqlitelog"
	atepb "go.ate.dev/substrate/api/ateapi/v1"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

const peerCredential = "synthetic-peer-credential-DO-NOT-EXPOSE"

// Keep the successful lifecycle real; inject only the failed control-plane operation.
type faultControl struct {
	*lifecycleControl
	op              string
	cause           error
	vanish          bool
	onResumeFailure func()
}

func (f *faultControl) GetActor(ctx context.Context, in *atepb.GetActorRequest, opts ...grpc.CallOption) (*atepb.Actor, error) {
	if f.op == "get" {
		return nil, f.cause
	}
	return f.lifecycleControl.GetActor(ctx, in, opts...)
}

func (f *faultControl) CreateActor(ctx context.Context, in *atepb.CreateActorRequest, opts ...grpc.CallOption) (*atepb.Actor, error) {
	op := "create"
	if in.GetActor().GetSourceTag() != nil {
		op = "clone"
	}
	if f.op == op {
		return nil, f.cause
	}
	return f.lifecycleControl.CreateActor(ctx, in, opts...)
}

func (f *faultControl) ResumeActor(ctx context.Context, in *atepb.ResumeActorRequest, opts ...grpc.CallOption) (*atepb.ResumeActorResponse, error) {
	if f.op == "resume" {
		cause := f.cause
		if f.onResumeFailure != nil {
			f.onResumeFailure()
		}
		if f.vanish {
			f.mu.Lock()
			delete(f.actors, in.GetActor().GetName())
			delete(f.completed, in.GetActor().GetName())
			f.mu.Unlock()
		}
		return nil, cause
	}
	return f.lifecycleControl.ResumeActor(ctx, in, opts...)
}

func (f *faultControl) CreateTag(ctx context.Context, in *atepb.CreateTagRequest, opts ...grpc.CallOption) (*atepb.Tag, error) {
	if f.op == "tag" {
		return nil, f.cause
	}
	return f.lifecycleControl.CreateTag(ctx, in, opts...)
}

type errorPathHarness struct {
	echoagent.Harness
	desc api.Descriptor
}

func (h errorPathHarness) Describe(context.Context) (api.Descriptor, error) { return h.desc, nil }

type errorPathStream struct {
	v1.Sessions_ExecServer
	ctx     context.Context
	updates []*v1.ExecUpdate
}

func (s *errorPathStream) Context() context.Context { return s.ctx }
func (s *errorPathStream) Send(u *v1.ExecUpdate) error {
	s.updates = append(s.updates, u)
	return nil
}

func errorPathService(t *testing.T, memory bool) (*session.Service, *sqlitelog.Store, *faultControl, *bytes.Buffer, string) {
	t.Helper()
	store, err := sqlitelog.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctl := &faultControl{lifecycleControl: newLifecycleControl()}
	desc, _ := (echoagent.Harness{}).Describe(t.Context())
	if memory {
		desc.Capabilities.Resumability = api.ResumabilityRequiresMemorySnapshot
	}
	logs := new(bytes.Buffer)
	logger := slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	backend := substrate.New(FromClient(ctl), "space", substrate.ObjectRef{Name: "echo"}, desc, substrate.WithLogger(logger))
	placer := placement.New(backend, echoagent.Model, placement.WithLogger(logger), placement.WithDialer(func(api.Incarnation) (api.Harness, func() error, error) {
		return errorPathHarness{desc: desc}, func() error { return nil }, nil
	}))
	registry, err := placement.NewRegistry("echo", map[string]*placement.Placer{"echo": placer})
	if err != nil {
		t.Fatal(err)
	}
	svc := session.NewService(store, registry, session.WithLogger(logger))
	sess, err := svc.CreateSession(t.Context(), &v1.CreateSessionRequest{})
	if err != nil {
		t.Fatal(err)
	}
	uid := sess.GetMetadata().GetUid()
	if err := svc.Exec(&v1.ExecRequest{Session: uid}, &errorPathStream{ctx: t.Context()}); err != nil {
		t.Fatalf("healthy turn: %v", err)
	}
	return svc, store, ctl, logs, uid
}

func peerFailure(t *testing.T, code codes.Code) error {
	t.Helper()
	st, err := status.New(code, peerCredential).WithDetails(&errdetails.ErrorInfo{
		Domain: "peer.example", Reason: "COMPUTE_CONFIG_INVALID", Metadata: map[string]string{"credential": peerCredential},
	})
	if err != nil {
		t.Fatal(err)
	}
	return st.Err()
}

func assertErrorPath(t *testing.T, err error, code codes.Code, reason string, cause error, logs *bytes.Buffer) {
	t.Helper()
	if err == nil || status.Code(err) != code || cause != nil && !errors.Is(err, cause) {
		t.Fatalf("error=%v, want %s retaining original cause", err, code)
	}
	details := status.Convert(err).Details()
	if len(details) != 1 {
		t.Fatalf("details=%v, want one host ErrorInfo", details)
	}
	info, ok := details[0].(*errdetails.ErrorInfo)
	if !ok || info.Domain != "agentsessions" || info.Reason != reason || len(info.Metadata) != 0 {
		t.Fatalf("details=%v, want host reason %s without metadata", details, reason)
	}
	if strings.Contains(err.Error(), peerCredential) || strings.Contains(logs.String(), peerCredential) {
		t.Fatal("peer credential escaped through public message or logs")
	}
}

// Every path crosses Service -> placement -> Backend -> FromClient, not a sentinel-only runtime.
func TestServiceComputeControlFailures(t *testing.T) {
	for _, tc := range []struct {
		path, op string
		memory   bool
	}{
		{"cold", "get", false}, {"cold", "create", false}, {"cold", "resume", false},
		{"attach", "get", true}, {"attach", "resume", true},
		{"resume", "get", true}, {"resume", "resume", true},
		{"fork", "create", false}, {"fork", "resume", false},
		{"fork", "tag", true}, {"fork", "clone", true}, {"fork", "resume", true},
	} {
		for _, failure := range []struct {
			code   codes.Code
			reason string
		}{
			{codes.Unavailable, "COMPUTE_UNAVAILABLE"},
			{codes.ResourceExhausted, "COMPUTE_RESOURCE_EXHAUSTED"},
		} {
			name := tc.path + "/" + tc.op + "/" + failure.code.String()
			if tc.memory {
				name += "/memory"
			}
			t.Run(name, func(t *testing.T) {
				svc, store, ctl, logs, uid := errorPathService(t, tc.memory)
				var action func() error
				switch tc.path {
				case "cold":
					sess, err := svc.CreateSession(t.Context(), &v1.CreateSessionRequest{})
					if err != nil {
						t.Fatal(err)
					}
					uid = sess.GetMetadata().GetUid()
				case "attach", "resume":
					if _, err := svc.Suspend(t.Context(), &v1.SuspendRequest{Session: uid}); err != nil {
						t.Fatalf("healthy suspend: %v", err)
					}
				}
				stream := &errorPathStream{ctx: t.Context()}
				action = func() error { return svc.Exec(&v1.ExecRequest{Session: uid}, stream) }
				if tc.path == "resume" {
					action = func() error { _, err := svc.Resume(t.Context(), &v1.ResumeRequest{Session: uid}); return err }
				}
				if tc.path == "fork" {
					action = func() error { _, err := svc.Fork(t.Context(), &v1.ForkRequest{Session: uid, Count: 1}); return err }
				}
				// Prove the same operation succeeds before injecting the peer error.
				if tc.path != "cold" {
					if err := action(); err != nil {
						t.Fatalf("healthy %s: %v", tc.path, err)
					}
					if tc.path != "fork" {
						if _, err := svc.Suspend(t.Context(), &v1.SuspendRequest{Session: uid}); err != nil {
							t.Fatal(err)
						}
					}
				}
				ctl.op, ctl.cause = tc.op, peerFailure(t, failure.code)
				assertErrorPath(t, action(), failure.code, failure.reason, ctl.cause, logs)
				if tc.path == "cold" {
					records, err := store.Session(uid).Read(1)
					if err != nil {
						t.Fatal(err)
					}
					for _, r := range records {
						if r.Event.Kind == api.EventExecutionStart {
							t.Fatal("failed cold turn appended EXECUTION_START")
						}
					}
				}
			})
		}
	}
}

func TestServiceMemoryLossPrecedesComputeRetry(t *testing.T) {
	for _, code := range []codes.Code{codes.Unavailable, codes.ResourceExhausted} {
		t.Run(code.String(), func(t *testing.T) {
			svc, _, ctl, logs, uid := errorPathService(t, true)
			if _, err := svc.Suspend(t.Context(), &v1.SuspendRequest{Session: uid}); err != nil {
				t.Fatal(err)
			}
			if _, err := svc.Resume(t.Context(), &v1.ResumeRequest{Session: uid}); err != nil {
				t.Fatalf("healthy resume: %v", err)
			}
			if _, err := svc.Suspend(t.Context(), &v1.SuspendRequest{Session: uid}); err != nil {
				t.Fatal(err)
			}
			ctl.op, ctl.cause, ctl.vanish = "resume", peerFailure(t, code), true
			_, err := svc.Resume(t.Context(), &v1.ResumeRequest{Session: uid})
			assertErrorPath(t, err, codes.FailedPrecondition, "MEMORY_STATE_LOST", ctl.cause, logs)
			if !errors.Is(err, api.ErrMemoryStateLost) {
				t.Fatal("memory loss sentinel was lost")
			}
		})
	}
}

func TestForkCleanupReadFailureKeepsPrimaryError(t *testing.T) {
	for _, tc := range []struct {
		code   codes.Code
		reason string
	}{
		{codes.Unavailable, "COMPUTE_UNAVAILABLE"},
		{codes.ResourceExhausted, "COMPUTE_RESOURCE_EXHAUSTED"},
	} {
		t.Run(tc.code.String(), func(t *testing.T) {
			svc, _, ctl, logs, uid := errorPathService(t, true)
			primary, cleanup := peerFailure(t, tc.code), peerFailure(t, codes.Unknown)
			ctl.op, ctl.cause = "resume", primary
			ctl.onResumeFailure = func() { ctl.op, ctl.cause = "get", cleanup }
			_, err := svc.Fork(t.Context(), &v1.ForkRequest{Session: uid, Count: 1})
			assertErrorPath(t, err, tc.code, tc.reason, primary, logs)
			if errors.Is(err, cleanup) {
				t.Fatal("secondary cleanup error entered the primary cause chain")
			}
			if !strings.Contains(err.Error(), "could not be read for cleanup") || !strings.Contains(err.Error(), "were left in place") {
				t.Fatalf("lost leftover-clone warning: %v", err)
			}
			if len(ctl.actors) != 2 || len(ctl.tags) != 1 {
				t.Fatal("unreadable clone or its tag was removed")
			}
		})
	}
}

func TestCallerCancellationPrecedesComputeRetry(t *testing.T) {
	svc, store, ctl, logs, _ := errorPathService(t, false)
	sess, err := svc.CreateSession(t.Context(), &v1.CreateSessionRequest{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	ctl.op, ctl.cause, ctl.onResumeFailure = "resume", peerFailure(t, codes.Unavailable), cancel
	uid := sess.GetMetadata().GetUid()
	err = svc.Exec(&v1.ExecRequest{Session: uid}, &errorPathStream{ctx: ctx})
	if status.Code(err) != codes.Canceled || !errors.Is(err, ctl.cause) || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation lost precedence or causes: %v", err)
	}
	if len(status.Convert(err).Details()) != 0 || strings.Contains(err.Error()+logs.String(), peerCredential) {
		t.Fatal("cancellation leaked peer details or diagnostics")
	}
	records, err := store.Session(uid).Read(1)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range records {
		if r.Event.Kind == api.EventExecutionStart {
			t.Fatal("canceled start journaled a turn")
		}
	}
}

func TestExecComputeFailureAfterSessionFrame(t *testing.T) {
	svc, store, ctl, logs, _ := errorPathService(t, false)
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	v1.RegisterSessionsServer(srv, svc)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(func() { srv.Stop(); _ = lis.Close() })
	conn, err := grpc.NewClient("passthrough:///error-path", grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	ctl.op, ctl.cause = "create", peerFailure(t, codes.ResourceExhausted)
	stream, err := v1.NewSessionsClient(conn).Exec(t.Context(), &v1.ExecRequest{})
	if err != nil {
		t.Fatal(err)
	}
	first, err := stream.Recv()
	if err != nil || first.GetSession().GetMetadata().GetUid() == "" {
		t.Fatalf("first frame=%v, err=%v; want session before compute failure", first, err)
	}
	_, err = stream.Recv()
	assertErrorPath(t, err, codes.ResourceExhausted, "COMPUTE_RESOURCE_EXHAUSTED", nil, logs)
	records, err := store.Session(first.GetSession().GetMetadata().GetUid()).Read(1)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range records {
		if r.Event.Kind == api.EventExecutionStart {
			t.Fatal("failed streamed turn appended EXECUTION_START")
		}
	}
}
