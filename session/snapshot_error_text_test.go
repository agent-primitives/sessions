package session_test

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"google.golang.org/protobuf/proto"

	"github.com/agent-primitives/sessions/api"
	v1 "github.com/agent-primitives/sessions/api/genpb"
	"github.com/agent-primitives/sessions/controller"
	"github.com/agent-primitives/sessions/harness/echoagent"
	"github.com/agent-primitives/sessions/runtime/substrate"
	"github.com/agent-primitives/sessions/sqlitelog"
	"github.com/agent-primitives/sessions/wire"
)

// requireHiddenText checks err, a refusal the service published from a substrate error or
// snapshot URI that carries hidden: its code and ErrorInfo reason, and a message that contains each
// of want but not hidden. An empty reason means the status carries no ErrorInfo. handled, the
// handler's error before gRPC turned it into a status, must still wrap each of causes.
func requireHiddenText(t *testing.T, err, handled error, hidden string, code codes.Code, reason string, want []string, causes ...error) {
	t.Helper()
	if status.Code(err) != code {
		t.Fatalf("want %v, got %v", code, err)
	}
	msg := status.Convert(err).Message()
	if strings.Contains(msg, hidden) {
		t.Errorf("error %q names substrate's text %q", msg, hidden)
	}
	for _, w := range want {
		if !strings.Contains(msg, w) {
			t.Errorf("error %q does not contain %q", msg, w)
		}
	}
	details := status.Convert(err).Details()
	if reason == "" {
		if len(details) != 0 {
			t.Fatalf("details=%v, want none", details)
		}
	} else if len(details) != 1 {
		t.Fatalf("details=%v, want one agentsessions/%s ErrorInfo", details, reason)
	} else if info, ok := details[0].(*errdetails.ErrorInfo); !ok || info.Domain != "agentsessions" || info.Reason != reason || len(info.Metadata) != 0 {
		t.Fatalf("details=%v, want agentsessions/%s without metadata", details, reason)
	}
	for _, cause := range causes {
		if !errors.Is(handled, cause) {
			t.Errorf("the handler's error %v does not wrap %v", handled, cause)
		}
	}
}

// parkingHarness is echoagent's harness until gate is set. From then on each Run asks for
// approval of a gated call, "call", and once that is decided, of a second one, "second", so an Exec
// parks at the first and a Resume after the decision parks at the second. beforeCall, when set,
// runs before each gated call with its ID.
type parkingHarness struct {
	echoagent.Harness
	gate       atomic.Bool
	beforeCall func(id string)
}

func (h *parkingHarness) Run(ctx context.Context, start *api.Start, sink api.EventSink) error {
	if !h.gate.Load() {
		return h.Harness.Run(ctx, start, sink)
	}
	for _, id := range []string{"call", "second"} {
		if h.beforeCall != nil {
			h.beforeCall(id)
		}
		call := publicGateCall(id)
		call.IdempotencyKey = id
		if _, err := sink.ToolCall(ctx, call); err != nil {
			return err
		}
	}
	return sink.Output(ctx, "done")
}

// Suspend, a stateful Fork's checkpoint of the parent (Backend.Snapshot), and the suspend of an
// Exec or a Resume that parks for approval look at the actor and then suspend it. When either call
// finds the actor gone, the refusal is FAILED_PRECONDITION with MEMORY_STATE_LOST; when another
// operation holds the actor's lease, it is ABORTED with COMPUTE_BUSY. The service publishes the
// refusal's text, so the message names the actor but neither substrate's own text from the call's
// error, which the handler's error still wraps, nor the URI of the snapshot the actor holds.
func TestCheckpointRefusalsOmitSubstrateText(t *testing.T) {
	ctx := context.Background()
	const substrateText = "substrate-said: detail-7f3a"
	gone := fmt.Errorf("%w: rpc error: code = NotFound desc = %s", substrate.ErrActorNotFound, substrateText)
	busy := fmt.Errorf("%w: space/x: rpc error: code = Aborted desc = %s", substrate.ErrActorBusy, substrateText)
	for _, tc := range []struct {
		name      string
		get, susp error
		code      codes.Code
		reason    string
		wantMsg   string
		typed     error
	}{
		{name: "actor gone at the look", get: gone, code: codes.FailedPrecondition, reason: "MEMORY_STATE_LOST", wantMsg: "no longer exists", typed: api.ErrMemoryStateLost},
		{name: "actor gone at the suspend", susp: gone, code: codes.FailedPrecondition, reason: "MEMORY_STATE_LOST", wantMsg: "no longer exists", typed: api.ErrMemoryStateLost},
		{name: "busy look", get: busy, code: codes.Aborted, reason: "COMPUTE_BUSY", wantMsg: "get actor before snapshot space/", typed: api.ErrComputeBusy},
		{name: "busy suspend", susp: busy, code: codes.Aborted, reason: "COMPUTE_BUSY", wantMsg: "suspend actor space/", typed: api.ErrComputeBusy},
	} {
		for _, op := range []string{"suspend", "fork", "exec park", "resume park"} {
			t.Run(tc.name+"/"+op, func(t *testing.T) {
				// Every snapshot the actor's suspends capture carries substrateText in its URI.
				ctl := &actorControl{status: map[string]substrate.ActorStatus{}, snapshots: map[string]string{}, snapPrefix: substrateText + "/"}
				store, err := sqlitelog.Open(":memory:")
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = store.Close() })
				har := &parkingHarness{}
				c, handled := serveCapturing(t, store, memoryRegistry(t, ctl, har))
				sess := mustCreate(t, c)
				// A turn, a suspend and a turn leave the actor running and holding the snapshot
				// the suspend captured, which the session's journal also records.
				if err := execHi(c, sess); err != nil {
					t.Fatalf("first turn: %v", err)
				}
				if _, err := c.Suspend(ctx, &v1.SuspendRequest{Session: sess}); err != nil {
					t.Fatalf("suspend: %v", err)
				}
				if err := execHi(c, sess); err != nil {
					t.Fatalf("second turn: %v", err)
				}

				var armed atomic.Bool
				fail := func(err error) func(string) error {
					return func(actor string) error {
						if actor == sess && armed.Load() {
							return err
						}
						return nil
					}
				}
				if tc.get != nil {
					ctl.getErr = fail(tc.get)
				}
				if tc.susp != nil {
					ctl.suspendErr = fail(tc.susp)
				}
				injected := tc.get
				if injected == nil {
					injected = tc.susp
				}
				// armAt arms the injected failure before the gated call id, after the Exec or
				// Resume has its compute and before its park suspends it.
				armAt := func(id string) {
					har.beforeCall = func(got string) {
						if got == id {
							requireHeldSnapshot(t, ctl, sess, substrateText)
							armed.Store(true)
						}
					}
				}
				gated := &v1.ExecRequest{Session: sess, Inputs: []*v1.Message{wire.MessageToProto(api.TextMessage("user", "write it"))}}

				var method string
				switch op {
				case "suspend":
					method = v1.Sessions_Suspend_FullMethodName
					requireHeldSnapshot(t, ctl, sess, substrateText)
					armed.Store(true)
					_, err = c.Suspend(ctx, &v1.SuspendRequest{Session: sess})
				case "fork":
					method = v1.Sessions_Fork_FullMethodName
					requireHeldSnapshot(t, ctl, sess, substrateText)
					armed.Store(true)
					_, err = c.Fork(ctx, &v1.ForkRequest{Session: sess, Count: 1})
				case "exec park":
					method = v1.Sessions_Exec_FullMethodName
					har.gate.Store(true)
					armAt("call")
					_, err = collectPublicExec(c, gated)
				case "resume park":
					method = v1.Sessions_Resume_FullMethodName
					har.gate.Store(true)
					if _, err := collectPublicExec(c, gated); err != nil {
						t.Fatalf("exec that parks at the first gated call: %v", err)
					}
					state, inspectErr := controller.InspectApproval(store.Session(sess), 0)
					if inspectErr != nil || state == nil || state.Request == nil || state.Call.ID != "call" {
						t.Fatalf("gate=%+v, %v, want a pending request for the first gated call", state, inspectErr)
					}
					if _, err := c.Approve(ctx, &v1.ApproveRequest{Session: sess, ExecutionId: state.ExecutionID, ToolCallId: "call", RequestSeq: state.Request.Seq, Approved: proto.Bool(false)}); err != nil {
						t.Fatalf("approve: %v", err)
					}
					armAt("second")
					_, err = c.Resume(ctx, &v1.ResumeRequest{Session: sess})
				}
				if !armed.Load() {
					t.Fatalf("%s did not reach the injected failure: %v", op, err)
				}
				requireHiddenText(t, err, handled(method), substrateText, tc.code, tc.reason,
					[]string{tc.wantMsg, "space/" + sess}, injected, tc.typed)
			})
		}
	}
}

// requireHeldSnapshot checks that the actor named sess holds a snapshot whose URI carries hidden.
func requireHeldSnapshot(t *testing.T, ctl *actorControl, sess, hidden string) {
	t.Helper()
	ctl.mu.Lock()
	snap := ctl.snapshots[sess]
	ctl.mu.Unlock()
	if !strings.Contains(snap, hidden) {
		t.Errorf("the actor holds snapshot %q, want one whose URI carries %q", snap, hidden)
	}
}

// A stateful Fork reads the parent after its checkpoint, before the tag and again after a
// successful tag, to check that it still holds the snapshot the fork took. A parent that holds
// another one is ABORTED with SNAPSHOT_SUPERSEDED, and a read refused because another operation
// holds the parent is ABORTED with COMPUTE_BUSY. Any other read error keeps the code and reason
// its type gives it: UNAVAILABLE with COMPUTE_UNAVAILABLE for an unreachable control plane, and
// INTERNAL for one nothing classifies (substrate's "while getting actor from DB"). The message
// names the parent and its status, but neither snapshot URI nor substrate's own text from the
// read's error, which the handler's error still wraps. TestForkWhoseTagFailsIsClassified covers the read after a failed tag.
func TestForkParentReadRefusalsOmitSubstrateText(t *testing.T) {
	ctx := context.Background()
	const substrateText = "substrate-said: detail-7f3a"
	busy := fmt.Errorf("%w: space/x: rpc error: code = Aborted desc = %s", substrate.ErrActorBusy, substrateText)
	// The adapter returns an Unknown or Internal status as it is, and wraps Unavailable in
	// api.ErrComputeUnavailable.
	unknown := status.Error(codes.Unknown, "while getting actor from DB: "+substrateText)
	unavailable := fmt.Errorf("%w: %w", api.ErrComputeUnavailable, status.Error(codes.Unavailable, substrateText))
	failParentRead := func(cause error) func(*actorControl, string, *atomic.Bool) {
		return func(ctl *actorControl, sess string, checkpointed *atomic.Bool) {
			ctl.onSuspend = func(actor string) {
				if actor == sess {
					checkpointed.Store(true)
				}
			}
			ctl.getErr = func(actor string) error {
				if actor == sess && checkpointed.Load() {
					return cause
				}
				return nil
			}
		}
	}
	for _, tc := range []struct {
		name    string
		arm     func(ctl *actorControl, sess string, checkpointed *atomic.Bool)
		onTag   func(ctl *actorControl, actor string)
		tags    bool // the refusing read is the one after the tag
		code    codes.Code
		reason  string
		wantMsg string
		causes  []error
	}{
		{
			name: "superseded before the tag",
			arm: func(ctl *actorControl, sess string, checkpointed *atomic.Bool) {
				ctl.onSuspend = func(actor string) {
					if actor == sess {
						ctl.mu.Lock()
						ctl.snapshots[actor] = "snap-newer-" + substrateText
						ctl.mu.Unlock()
					}
				}
			},
			code:    codes.Aborted,
			reason:  "SNAPSHOT_SUPERSEDED",
			wantMsg: "(suspended) is no longer suspended holding the snapshot this fork took",
			causes:  []error{substrate.ErrSnapshotSuperseded, api.ErrSnapshotSuperseded},
		},
		{
			name: "superseded after the tag",
			arm:  func(*actorControl, string, *atomic.Bool) {},
			onTag: func(ctl *actorControl, actor string) {
				ctl.mu.Lock()
				ctl.snapshots[actor] = "snap-newer-" + substrateText
				ctl.mu.Unlock()
			},
			tags:    true,
			code:    codes.Aborted,
			reason:  "SNAPSHOT_SUPERSEDED",
			wantMsg: "(suspended) is no longer suspended holding the snapshot this fork took",
			causes:  []error{substrate.ErrSnapshotSuperseded, api.ErrSnapshotSuperseded},
		},
		{
			name:    "busy parent read",
			arm:     failParentRead(busy),
			code:    codes.Aborted,
			reason:  "COMPUTE_BUSY",
			wantMsg: "is busy with another operation",
			causes:  []error{busy, api.ErrComputeBusy},
		},
		{
			name:    "unavailable parent read",
			arm:     failParentRead(unavailable),
			code:    codes.Unavailable,
			reason:  "COMPUTE_UNAVAILABLE",
			wantMsg: "failed",
			causes:  []error{unavailable, api.ErrComputeUnavailable},
		},
		{
			name:    "unclassified parent read",
			arm:     failParentRead(unknown),
			code:    codes.Internal,
			wantMsg: "failed",
			causes:  []error{unknown},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Every snapshot the parent's suspends capture carries substrateText in its URI.
			ctl := &actorControl{status: map[string]substrate.ActorStatus{}, snapshots: map[string]string{}, snapPrefix: substrateText + "/"}
			store, err := sqlitelog.Open(":memory:")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			c, handled := serveCapturing(t, store, memoryRegistry(t, ctl, echoagent.Harness{}))
			sess := mustCreate(t, c)
			if err := execHi(c, sess); err != nil {
				t.Fatalf("first turn: %v", err)
			}
			var checkpointed atomic.Bool
			tc.arm(ctl, sess, &checkpointed)
			var tagged atomic.Bool
			ctl.onTag = func(actor string) {
				tagged.Store(true)
				if tc.onTag != nil {
					tc.onTag(ctl, actor)
				}
			}
			ctl.mu.Lock()
			created := ctl.created
			ctl.mu.Unlock()

			_, err = c.Fork(ctx, &v1.ForkRequest{Session: sess, Count: 1})
			requireHiddenText(t, err, handled(v1.Sessions_Fork_FullMethodName), substrateText, tc.code, tc.reason,
				[]string{tc.wantMsg, "parent " + strconv.Quote(sess)}, tc.causes...)
			if tagged.Load() != tc.tags {
				t.Errorf("tagged=%v, want %v", tagged.Load(), tc.tags)
			}
			ctl.mu.Lock()
			gotCreated := ctl.created
			ctl.mu.Unlock()
			if gotCreated != created {
				t.Errorf("actors created %d->%d: the refused fork may create no child", created, gotCreated)
			}
		})
	}
}
