package session_test

import (
	"context"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/agent-primitives/sessions/api"
	v1 "github.com/agent-primitives/sessions/api/genpb"
	"github.com/agent-primitives/sessions/harness/echoagent"
	"github.com/agent-primitives/sessions/placement"
	"github.com/agent-primitives/sessions/runtime/local"
	"github.com/agent-primitives/sessions/wire"
)

// unreachableBackend is a backend without api.CleanupRuntime whose compute's harness never answers:
// Create returns an address nothing listens on, which the lazy gRPC dial reports at the first call.
type unreachableBackend struct {
	placement.Backend
	address string
	creates atomic.Int32
}

func (b *unreachableBackend) Create(ctx context.Context, spec *api.SessionSpec) (api.Incarnation, error) {
	b.creates.Add(1)
	in, err := b.Backend.Create(ctx, spec)
	in.Address = b.address
	return in, err
}

func newUnreachableClient(t *testing.T) (v1.SessionsClient, *unreachableBackend) {
	t.Helper()
	inner := local.New(echoagent.Harness{})
	t.Cleanup(func() { _ = inner.Close() })
	b := &unreachableBackend{Backend: inner, address: "unix://" + filepath.Join(t.TempDir(), "missing.sock")}
	if _, ok := placement.Backend(b).(api.CleanupRuntime); ok {
		t.Fatal("test backend must not implement CleanupRuntime")
	}
	return newClientWith(t, b), b
}

// Compute a failed Exec may have left running is reported in the message; the public code stays
// the one the failure always had.
func TestExecReportsComputeLeftRunningWithUnchangedCode(t *testing.T) {
	client, b := newUnreachableClient(t)
	uid := mustCreate(t, client)
	stream, err := client.Exec(t.Context(), &v1.ExecRequest{
		Session: uid,
		Inputs:  []*v1.Message{wire.MessageToProto(api.TextMessage("user", "hi"))},
	})
	if err == nil {
		err = drainExec(stream)
	}
	if status.Code(err) != codes.Unavailable || !strings.Contains(err.Error(), "may still be running") {
		t.Fatalf("Exec = %v, want UNAVAILABLE reporting the compute", err)
	}
	if b.creates.Load() != 1 {
		t.Fatalf("creates = %d, want 1", b.creates.Load())
	}
}

// A stale expected_last_seq is refused before compute is created, with the code it always had.
func TestExecStaleCursorRefusedBeforeCreate(t *testing.T) {
	client, b := newUnreachableClient(t)
	uid := mustCreate(t, client)
	stream, err := client.Exec(t.Context(), &v1.ExecRequest{
		Session:         uid,
		Inputs:          []*v1.Message{wire.MessageToProto(api.TextMessage("user", "hi"))},
		ExpectedLastSeq: proto.Int64(3),
	})
	if err == nil {
		err = drainExec(stream)
	}
	if status.Code(err) != codes.Aborted || !strings.Contains(err.Error(), "expected_last_seq 3, head 0") {
		t.Fatalf("Exec = %v, want ABORTED naming the cursor and head", err)
	}
	if b.creates.Load() != 0 {
		t.Fatalf("creates = %d, want none", b.creates.Load())
	}
}
