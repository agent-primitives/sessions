package session_test

import (
	"context"
	"strings"
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

// Developer journey A4: a memory-tier session on a template that only gives VOLUMES fidelity is
// refused on Suspend and on the Resume and Exec that follow, as is a Fork of another such session,
// and the status message alone names the session, the actor, and the required and observed
// fidelity.
func TestCheckpointRefusalNamesSessionActorAndFidelity(t *testing.T) {
	ctl := &actorControl{status: map[string]substrate.ActorStatus{}, snapshots: map[string]string{}}
	store, err := sqlitelog.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	c := newMemoryHost(t, checkpointControl{actorControl: ctl}, store, echoagent.Harness{})
	sess, parent := mustCreate(t, c), mustCreate(t, c)
	for _, s := range []string{sess, parent} {
		if err := execHi(c, s); err != nil {
			t.Fatal(err)
		}
	}
	_, suspendErr := c.Suspend(t.Context(), &v1.SuspendRequest{Session: sess})
	_, resumeErr := c.Resume(t.Context(), &v1.ResumeRequest{Session: sess})
	execErr := execHi(c, sess)
	_, forkErr := c.Fork(t.Context(), &v1.ForkRequest{Session: parent, Count: 1})
	for operation, tc := range map[string]struct {
		session string
		err     error
	}{
		"suspend": {sess, suspendErr},
		"resume":  {sess, resumeErr},
		"exec":    {sess, execErr},
		"fork":    {parent, forkErr},
	} {
		sess, err := tc.session, tc.err
		st := status.Convert(err)
		if st.Code() != codes.FailedPrecondition {
			t.Fatalf("%s: %v, want FAILED_PRECONDITION", operation, err)
		}
		for _, want := range []string{
			operation + `: session "` + sess + `"`,
			"actor space/" + sess + ":",
			"required MEMORY",
			"observed VOLUMES",
		} {
			if !strings.Contains(st.Message(), want) {
				t.Errorf("%s: %q does not contain %q", operation, st.Message(), want)
			}
		}
		if strings.Count(st.Message(), `session "`+sess+`"`) != 1 {
			t.Errorf("%s: %q names the session more than once", operation, st.Message())
		}
	}
}
