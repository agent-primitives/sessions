package session_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agent-primitives/sessions/api"
	v1 "github.com/agent-primitives/sessions/api/genpb"
	"github.com/agent-primitives/sessions/harness/echoagent"
	"github.com/agent-primitives/sessions/runtime/local"
	"github.com/agent-primitives/sessions/session"
	"github.com/agent-primitives/sessions/sqlitelog"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type failedStart struct {
	*local.Backend
	cause   error
	cancel  context.CancelFunc
	wait    bool
	peer    bool
	success bool
	creates atomic.Int32
	started atomic.Bool
}

func (b *failedStart) Create(ctx context.Context, spec *api.SessionSpec) (api.Incarnation, error) {
	b.creates.Add(1)
	if b.cancel != nil {
		b.cancel()
	}
	if b.wait {
		<-ctx.Done()
	}
	if b.success {
		in, err := b.Backend.Create(context.WithoutCancel(ctx), spec)
		b.started.Store(err == nil)
		return in, err
	}
	if b.peer {
		return api.Incarnation{}, fmt.Errorf("start peer: %w", b.cause)
	}
	if ctx.Err() != nil {
		return api.Incarnation{}, fmt.Errorf("start: %w", ctx.Err())
	}
	return api.Incarnation{}, fmt.Errorf("start: %w", b.cause)
}

func (b *failedStart) Restore(ctx context.Context, ref api.SnapshotRef) (api.Incarnation, error) {
	in, err := b.Backend.Restore(ctx, ref)
	b.started.Store(err == nil)
	if b.cancel != nil {
		b.cancel()
	}
	return in, err
}

func TestResumeStartupSuccessAfterCallerCancellation(t *testing.T) {
	store := openStore(t, ":memory:")
	if err := store.PutSession(sqlitelog.SessionMeta{UID: "uid", Harness: "echo"}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	backend := &failedStart{Backend: local.New(echoagent.Harness{}), cancel: cancel}
	t.Cleanup(func() { _ = backend.Close() })
	_, err := session.NewService(store, echoRegistry(t, backend)).Resume(ctx, &v1.ResumeRequest{Session: "uid"})
	if !backend.started.Load() {
		t.Fatal("Restore did not succeed")
	}
	if status.Code(err) != codes.Canceled || !errors.Is(err, context.Canceled) {
		t.Fatalf("resume=%v, want caller cancellation retaining cause", err)
	}
	if len(status.Convert(err).Details()) != 0 {
		t.Fatalf("invented cancellation reason: %v", err)
	}
	if head, err := store.Session("uid").Head(); err != nil || head != 0 {
		t.Fatalf("canceled pre-redrive Resume appended records: head=%d err=%v", head, err)
	}
}

// deadline_unix bounds compute start while the RPC itself stays live, so a client can
// receive the host's context status rather than a locally canceled transport error.
func TestExecStartDeadlineDoesNotExposeBackendDiagnostic(t *testing.T) {
	const diagnostic = "backend-private-diagnostic"
	backend := &failedStart{Backend: local.New(echoagent.Harness{}), cause: errors.New(diagnostic), wait: true, peer: true}
	t.Cleanup(func() { _ = backend.Close() })
	c := newClientWith(t, backend)
	stream, err := c.Exec(t.Context(), &v1.ExecRequest{Session: mustCreate(t, c), DeadlineUnix: time.Now().Add(2 * time.Second).Unix()})
	if err == nil {
		err = drainExec(stream)
	}
	if backend.creates.Load() != 1 {
		t.Fatalf("test did not reach compute start: creates=%d err=%v", backend.creates.Load(), err)
	}
	if status.Code(err) != codes.DeadlineExceeded || strings.Contains(err.Error(), diagnostic) {
		t.Fatalf("deadline status exposed backend diagnostic: %v", err)
	}
	if len(status.Convert(err).Details()) != 0 {
		t.Fatalf("caller deadline acquired a reason: %v", err)
	}
}

// A start failure precedes EXECUTION_START. Returning INTERNAL or appending a turn would make
// a caller choose the wrong recovery path; model/Run failures are tested separately.
func TestExecComputeStartReasonsAndCallerContexts(t *testing.T) {
	for _, tc := range []struct {
		name   string
		cause  error
		code   codes.Code
		reason string
	}{
		{"unavailable", api.ErrComputeUnavailable, codes.Unavailable, "COMPUTE_UNAVAILABLE"},
		{"capacity", api.ErrComputeResourceExhausted, codes.ResourceExhausted, "COMPUTE_RESOURCE_EXHAUSTED"},
		{"configuration", api.ErrComputeConfigInvalid, codes.FailedPrecondition, "COMPUTE_CONFIG_INVALID"},
		{"canceled", context.Canceled, codes.Canceled, ""},
		{"canceled with peer deadline", context.DeadlineExceeded, codes.Canceled, ""},
		{"deadline", context.DeadlineExceeded, codes.DeadlineExceeded, ""},
		{"successful start after cancellation", context.Canceled, codes.Canceled, ""},
		{"peer deadline", context.DeadlineExceeded, codes.Internal, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := openStore(t, ":memory:")
			inner := local.New(echoagent.Harness{})
			t.Cleanup(func() { _ = inner.Close() })
			backend := &failedStart{Backend: inner, cause: tc.cause}
			ctx := t.Context()
			if tc.name == "canceled" || tc.name == "canceled with peer deadline" || tc.name == "successful start after cancellation" {
				backend.success = tc.name == "successful start after cancellation"
				backend.peer = tc.name == "canceled with peer deadline"
				ctx, backend.cancel = context.WithCancel(ctx)
				defer backend.cancel()
			}
			if tc.name == "deadline" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 20*time.Millisecond)
				defer cancel()
				backend.wait = true
			}
			if err := store.PutSession(sqlitelog.SessionMeta{UID: "start", Harness: "echo"}); err != nil {
				t.Fatal(err)
			}
			svc := session.NewService(store, echoRegistry(t, backend))
			err := svc.Exec(&v1.ExecRequest{Session: "start"}, &rpcStream{ctx: ctx})
			if backend.success && !backend.started.Load() {
				t.Fatalf("Create did not succeed: calls=%d err=%v", backend.creates.Load(), err)
			}
			if status.Code(err) != tc.code || !errors.Is(err, tc.cause) || tc.code == codes.Canceled && !errors.Is(err, context.Canceled) {
				t.Fatalf("start=%v, want %s retaining %v", err, tc.code, tc.cause)
			}
			details := status.Convert(err).Details()
			if tc.reason != "" {
				if len(details) != 1 {
					t.Fatalf("missing reason: %v", details)
				}
				info, ok := details[0].(*errdetails.ErrorInfo)
				if !ok || info.Domain != "agentsessions" || info.Reason != tc.reason {
					t.Fatalf("unexpected details: %v", details)
				}
			} else if len(details) != 0 {
				t.Fatalf("unknown/context start acquired a reason: %v", details)
			}
			if head, err := store.Session("start").Head(); err != nil || head != 0 {
				t.Fatalf("failed start journaled a turn: head=%d err=%v", head, err)
			}
		})
	}
}
