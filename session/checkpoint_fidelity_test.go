package session_test

import (
	"context"
	"sync/atomic"
	"testing"

	v1 "github.com/agent-primitives/sessions/api/genpb"
	"github.com/agent-primitives/sessions/harness/echoagent"
	"github.com/agent-primitives/sessions/runtime/substrate"
	"github.com/agent-primitives/sessions/sqlitelog"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Override only the observed checkpoint evidence; keep the real fake registry's state changes.
type checkpointControl struct {
	*actorControl
	missingURI bool
}

func (c checkpointControl) observed(info substrate.ActorInfo) substrate.ActorInfo {
	if info.Snapshot != "" {
		if c.missingURI {
			info.Snapshot = ""
		} else {
			info.SnapshotFidelity = substrate.SnapshotFidelityVolumes
		}
	}
	return info
}

func (c checkpointControl) SuspendActor(ctx context.Context, ref substrate.ActorRef) (substrate.ActorInfo, error) {
	info, err := c.actorControl.SuspendActor(ctx, ref)
	return c.observed(info), err
}

func (c checkpointControl) GetActor(ctx context.Context, ref substrate.ActorRef) (substrate.ActorInfo, error) {
	info, err := c.actorControl.GetActor(ctx, ref)
	return c.observed(info), err
}

func TestRefusedCheckpointIsFailedPreconditionAndStaysCold(t *testing.T) {
	for _, missingURI := range []bool{false, true} {
		for _, operation := range []string{"suspend", "fork"} {
			t.Run(operation+map[bool]string{false: "/wrong-fidelity", true: "/missing-URI"}[missingURI], func(t *testing.T) {
				var resumes atomic.Int32
				ctl := &actorControl{status: map[string]substrate.ActorStatus{}, snapshots: map[string]string{}, onResume: func(string) { resumes.Add(1) }}
				store, err := sqlitelog.Open(":memory:")
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = store.Close() })
				c := newMemoryHost(t, checkpointControl{actorControl: ctl, missingURI: missingURI}, store, echoagent.Harness{})
				sess := mustCreate(t, c)
				if err := execHi(c, sess); err != nil {
					t.Fatal(err)
				}
				head, err := store.Session(sess).Head()
				if err != nil {
					t.Fatal(err)
				}
				before := resumes.Load()
				if operation == "suspend" {
					_, err = c.Suspend(t.Context(), &v1.SuspendRequest{Session: sess})
				} else {
					_, err = c.Fork(t.Context(), &v1.ForkRequest{Session: sess, Count: 1})
				}
				if status.Code(err) != codes.FailedPrecondition {
					t.Fatalf("%s: %v", operation, err)
				}
				// Neither the interrupted-turn Restore fallback nor Create may revive refused state.
				if _, err := c.Resume(t.Context(), &v1.ResumeRequest{Session: sess}); status.Code(err) != codes.FailedPrecondition {
					t.Fatalf("resume after refusal: %v", err)
				}
				if err := execHi(c, sess); status.Code(err) != codes.FailedPrecondition {
					t.Fatalf("exec after refusal: %v", err)
				}
				ctl.mu.Lock()
				state, created := ctl.status[sess], ctl.created
				ctl.mu.Unlock()
				if state != substrate.StatusSuspended || created != 1 || resumes.Load() != before {
					t.Fatalf("refusal revived/replaced compute: state=%v created=%d resumes=%d", state, created, resumes.Load())
				}
				if got, err := store.Session(sess).Head(); err != nil || got != head {
					t.Fatalf("refusal changed head: %d err=%v want %d", got, err, head)
				}
				if err := store.Session(sess).Verify(); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}
