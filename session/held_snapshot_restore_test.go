package session_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/agent-primitives/sessions/api"
	v1 "github.com/agent-primitives/sessions/api/genpb"
	"github.com/agent-primitives/sessions/harness/echoagent"
	"github.com/agent-primitives/sessions/placement"
	"github.com/agent-primitives/sessions/runtime/substrate"
	"github.com/agent-primitives/sessions/sqlitelog"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Inject only the control-plane read/readiness evidence, preserving actor lifecycle side effects.
type restoreControl struct {
	*actorControl
	mode atomic.Int32 // 1: read failure; 2: newest durable copy not ready
}

func (c *restoreControl) GetActor(ctx context.Context, ref substrate.ActorRef) (substrate.ActorInfo, error) {
	if c.mode.Load() == 1 {
		return substrate.ActorInfo{}, errors.New("control unavailable")
	}
	info, err := c.actorControl.GetActor(ctx, ref)
	info.SnapshotNotReady = c.mode.Load() == 2
	return info, err
}

func (c *restoreControl) SuspendActor(ctx context.Context, ref substrate.ActorRef) (substrate.ActorInfo, error) {
	info, err := c.actorControl.SuspendActor(ctx, ref)
	info.SnapshotNotReady = c.mode.Load() == 2
	return info, err
}

// URI/state/readiness refusals deliberately remain INTERNAL, unlike inherited fidelity/loss errors.
func TestHeldRestoreRefusalsAreInternalAndDoNotJournal(t *testing.T) {
	for _, reason := range []string{"wrong URI", "running after Exec", "read failure", "not ready"} {
		t.Run(reason, func(t *testing.T) {
			var resumes, dials atomic.Int32
			ctl := &restoreControl{actorControl: &actorControl{
				status: map[string]substrate.ActorStatus{}, snapshots: map[string]string{},
				onResume: func(string) { resumes.Add(1) },
			}}
			store, err := sqlitelog.Open(":memory:")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			desc, err := echoagent.Harness{}.Describe(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			backend := substrate.New(ctl, "space", substrate.ObjectRef{Name: "echo"}, desc)
			p := placement.New(backend, echoagent.Model, placement.WithDialer(func(api.Incarnation) (api.Harness, func() error, error) {
				dials.Add(1)
				return echoagent.Harness{}, func() error { return nil }, nil
			}))
			r, err := placement.NewRegistry("echo", map[string]*placement.Placer{"echo": p})
			if err != nil {
				t.Fatal(err)
			}
			c := serveRegistry(t, store, r)
			sess := mustCreate(t, c)
			if err := execHi(c, sess); err != nil {
				t.Fatal(err)
			}
			if _, err := c.Suspend(t.Context(), &v1.SuspendRequest{Session: sess}); err != nil {
				t.Fatal(err)
			}
			switch reason {
			case "wrong URI":
				ctl.mu.Lock()
				ctl.snapshots[sess] = "newer-durable-checkpoint"
				ctl.mu.Unlock()
			case "running after Exec":
				if _, err := c.Resume(t.Context(), &v1.ResumeRequest{Session: sess}); err != nil {
					t.Fatal(err)
				}
				if err := execHi(c, sess); err != nil {
					t.Fatal(err)
				}
			case "read failure":
				ctl.mode.Store(1)
			case "not ready":
				ctl.mode.Store(2)
			}
			head, err := store.Session(sess).Head()
			if err != nil {
				t.Fatal(err)
			}
			beforeResume, beforeDial := resumes.Load(), dials.Load()
			if _, err := c.Resume(t.Context(), &v1.ResumeRequest{Session: sess}); status.Code(err) != codes.Internal {
				t.Fatalf("resume: %v, want INTERNAL", err)
			}
			if reason == "not ready" || reason == "read failure" {
				if err := execHi(c, sess); status.Code(err) != codes.Internal {
					t.Fatalf("exec: %v, want INTERNAL", err)
				}
			}
			if reason == "not ready" {
				if _, err := c.Suspend(t.Context(), &v1.SuspendRequest{Session: sess}); status.Code(err) != codes.Internal {
					t.Fatalf("suspend: %v, want INTERNAL", err)
				}
			}
			if resumes.Load() != beforeResume || dials.Load() != beforeDial {
				t.Fatal("refusal resumed compute or dialed the harness")
			}
			if got, err := store.Session(sess).Head(); err != nil || got != head {
				t.Fatalf("refusal changed head: %d err=%v, want %d", got, err, head)
			}
			if err := store.Session(sess).Verify(); err != nil {
				t.Fatal(err)
			}
			ctl.mode.Store(0)
			// Capture the current state explicitly, rather than retrying the stale held ref.
			if _, err := c.Suspend(t.Context(), &v1.SuspendRequest{Session: sess}); err != nil {
				t.Fatal(err)
			}
			if _, err := c.Resume(t.Context(), &v1.ResumeRequest{Session: sess}); err != nil {
				t.Fatal(err)
			}
			if err := execHi(c, sess); err != nil {
				t.Fatal(err)
			}
		})
	}
}
