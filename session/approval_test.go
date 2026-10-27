package session_test

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/agent-primitives/sessions/api"
	v1 "github.com/agent-primitives/sessions/api/genpb"
	"github.com/agent-primitives/sessions/controller"
	"github.com/agent-primitives/sessions/eventlog"
	"github.com/agent-primitives/sessions/placement"
	"github.com/agent-primitives/sessions/runtime/local"
	"github.com/agent-primitives/sessions/sqlitelog"
	"github.com/agent-primitives/sessions/wire"
)

// Real local Harness.Connect exercises park ACK/EOF, placement snapshot and SQL projection.
type publicGateHarness struct {
	runs    atomic.Int32
	starts  chan api.Start
	version string
	second  bool
}

func (h *publicGateHarness) Describe(context.Context) (api.Descriptor, error) {
	version := h.version
	if version == "" {
		version = "v1"
	}
	return api.Descriptor{ID: "gate", Version: version, Capabilities: api.Capabilities{Resumability: api.ResumabilityStatelessReplay}}, nil
}
func (h *publicGateHarness) Run(ctx context.Context, start *api.Start, sink api.EventSink) error {
	h.runs.Add(1)
	if h.starts != nil {
		h.starts <- *start
	}
	result, err := sink.ToolCall(ctx, publicGateCall("call"))
	if err != nil {
		return err
	}
	if h.second {
		if _, err := sink.ToolCall(ctx, publicGateCall("second")); err != nil {
			return err
		}
	}
	return sink.Output(ctx, result.ID+":"+string(result.Code))
}
func publicGateCall(id string) api.ToolCall {
	return api.ToolCall{ID: id, Tool: "write", IdempotencyKey: "shared-key", Mediation: api.MediationRequiresApproval, Args: map[string]any{"path": "file"}}
}

type publicGateBackend struct {
	*local.Backend
	describes, creates, restores, snapshots atomic.Int32
	snapshotErr                             error
	beforeSnapshot                          func()
}

func (b *publicGateBackend) Describe(ctx context.Context) (api.Descriptor, error) {
	b.describes.Add(1)
	return b.Backend.Describe(ctx)
}
func (b *publicGateBackend) Create(ctx context.Context, spec *api.SessionSpec) (api.Incarnation, error) {
	b.creates.Add(1)
	return b.Backend.Create(ctx, spec)
}
func (b *publicGateBackend) Restore(ctx context.Context, ref api.SnapshotRef) (api.Incarnation, error) {
	b.restores.Add(1)
	return b.Backend.Restore(ctx, ref)
}
func (b *publicGateBackend) Snapshot(ctx context.Context, inc api.Incarnation, kind api.SnapshotKind) (api.SnapshotRef, error) {
	b.snapshots.Add(1)
	if b.beforeSnapshot != nil {
		b.beforeSnapshot()
	}
	if b.snapshotErr != nil {
		return api.SnapshotRef{}, b.snapshotErr
	}
	return b.Backend.Snapshot(ctx, inc, kind)
}
func (b *publicGateBackend) ioCounts() [4]int32 {
	return [4]int32{b.describes.Load(), b.creates.Load(), b.restores.Load(), b.snapshots.Load()}
}
func publicGateRegistry(t *testing.T, h *publicGateHarness, executor controller.ToolFunc) (*placement.Registry, *publicGateBackend) {
	t.Helper()
	b := &publicGateBackend{Backend: local.New(h)}
	t.Cleanup(func() { _ = b.Close() })
	opts := []placement.Option{}
	if executor != nil {
		opts = append(opts, placement.WithToolExecutor(executor))
	}
	r, err := placement.NewRegistry("gate", map[string]*placement.Placer{"gate": placement.New(b, nil, opts...)})
	if err != nil {
		t.Fatal(err)
	}
	return r, b
}
func seedPublicGate(t *testing.T, log eventlog.Store, harness string, cut api.EventKind) {
	t.Helper()
	f, err := log.NewFence()
	if err != nil {
		t.Fatal(err)
	}
	call := publicGateCall("call")
	events := []api.Event{
		{Kind: api.EventExecutionStart, ExecutionStart: &api.ExecutionStart{Harness: harness, HarnessVersion: "v1", InputCount: proto.Int64(0), Config: []byte("original"), ResumeFromSeq: 7}},
		{Kind: api.EventToolCall, ToolCall: &call},
		{Kind: api.EventApprovalRequest, Approval: &api.ApprovalRequest{ToolCallID: "call"}},
	}
	for i, ev := range events {
		ev.ExecutionID = "execution"
		if _, err := log.Append(int64(i), f, ev); err != nil {
			t.Fatal(err)
		}
		if ev.Kind == cut {
			return
		}
	}
}
func publicDecision(uid string) *v1.ApproveRequest {
	return &v1.ApproveRequest{Session: uid, ExecutionId: "execution", ToolCallId: "call", RequestSeq: 3, Approved: proto.Bool(false)}
}
func collectPublicExec(c v1.SessionsClient, req *v1.ExecRequest) ([]*v1.ExecUpdate, error) {
	stream, err := c.Exec(context.Background(), req)
	if err != nil {
		return nil, err
	}
	var frames []*v1.ExecUpdate
	for {
		frame, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return frames, nil
		}
		if err != nil {
			return frames, err
		}
		frames = append(frames, frame)
	}
}

func TestApprovalPublicValidation(t *testing.T) {
	store := openStore(t, ":memory:")
	r, b := publicGateRegistry(t, &publicGateHarness{}, nil)
	c := serveRegistry(t, store, r)
	for _, tc := range []struct {
		name   string
		change func(*v1.ApproveRequest)
		want   codes.Code
	}{
		{"session", func(q *v1.ApproveRequest) { q.Session = "" }, codes.InvalidArgument},
		{"execution", func(q *v1.ApproveRequest) { q.ExecutionId = "" }, codes.InvalidArgument},
		{"call", func(q *v1.ApproveRequest) { q.ToolCallId = "" }, codes.InvalidArgument},
		{"seq", func(q *v1.ApproveRequest) { q.RequestSeq = 0 }, codes.InvalidArgument},
		{"negative", func(q *v1.ApproveRequest) { q.RequestSeq = -1 }, codes.InvalidArgument},
		{"presence", func(q *v1.ApproveRequest) { q.Approved = nil }, codes.InvalidArgument},
		{"unknown", func(q *v1.ApproveRequest) {}, codes.NotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q := publicDecision("unknown")
			tc.change(q)
			if _, err := c.Approve(t.Context(), q); status.Code(err) != tc.want {
				t.Fatalf("Approve=%v", err)
			}
		})
	}
	if b.ioCounts() != ([4]int32{}) {
		t.Fatal("validation touched runtime")
	}
	list, err := c.ListSessions(t.Context(), &v1.ListSessionsRequest{})
	if err != nil || len(list.Sessions) != 0 {
		t.Fatalf("unknown decision created journal: %v %v", list, err)
	}
}
func TestPublicApprovalParkDiscoveryReopenRecoveryAndFrames(t *testing.T) {
	for _, approved := range []bool{false, true} {
		t.Run(map[bool]string{false: "deny", true: "approve"}[approved], func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "journal.db")
			store := openStore(t, path)
			h := &publicGateHarness{starts: make(chan api.Start, 10)}
			var effects atomic.Int32
			r, b := publicGateRegistry(t, h, func(_ context.Context, scope controller.ToolCallContext, call api.ToolCall) (api.ToolResult, error) {
				effects.Add(1)
				if scope.SessionUID == "" || call.IdempotencyKey != "shared-key" {
					t.Error("missing session-scoped identity")
				}
				return api.ToolResult{Output: map[string]any{"ok": true}}, nil
			})
			c := serveRegistry(t, store, r)
			uid := mustCreate(t, c)
			frames, err := collectPublicExec(c, &v1.ExecRequest{Session: uid, Inputs: []*v1.Message{wire.MessageToProto(api.TextMessage("user", "write"))}, Config: []byte("original"), ResumeFromSeq: 7})
			if err != nil {
				t.Fatalf("park should be OK EOF: %v", err)
			}
			if len(frames) < 3 || frames[0].GetSession().GetLastSeq() != 0 {
				t.Fatalf("initial frame=%v", frames)
			}
			paused := frames[len(frames)-1].GetSession()
			if paused == nil || paused.GetMetadata().GetUid() != uid || paused.GetExecState() != v1.ExecState_EXEC_AWAITING || paused.GetComputeState() != v1.ComputeState_COMPUTE_COLD || paused.GetPendingApproval().GetRequestSeq() <= 0 {
				t.Fatalf("final pause frame=%v", paused)
			}
			records := routingRecords(t, store.Session(uid))
			if paused.GetLastSeq() != int64(len(records)) {
				t.Fatalf("final cursor=%d journal=%d", paused.LastSeq, len(records))
			}
			for _, rec := range records {
				if rec.Event.Kind == api.EventEnd || rec.Event.Kind == api.EventError {
					t.Fatalf("park fabricated %s", rec.Event.Kind)
				}
			}
			if effects.Load() != 0 {
				t.Fatal("effect before decision")
			}
			// Reopen through a fresh service: discovery is journal authority, not a service flag.
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			store = openStore(t, path)
			c = serveRegistry(t, store, r)
			got, err := c.GetSession(t.Context(), &v1.GetSessionRequest{Uid: uid})
			if err != nil || !proto.Equal(got.GetPendingApproval(), paused.GetPendingApproval()) {
				t.Fatalf("reopened Get=%v %v", got, err)
			}
			listed, err := c.ListSessions(t.Context(), &v1.ListSessionsRequest{})
			if err != nil || len(listed.Sessions) != 1 || !proto.Equal(listed.Sessions[0].PendingApproval, paused.PendingApproval) {
				t.Fatalf("reopened List=%v %v", listed, err)
			}
			beforeIO := b.ioCounts()
			before := routingRecords(t, store.Session(uid))
			query, err := c.Resume(t.Context(), &v1.ResumeRequest{Session: uid})
			if err != nil || !proto.Equal(query.PendingApproval, paused.PendingApproval) || query.LastSeq != paused.LastSeq || query.ComputeState != paused.ComputeState {
				t.Fatalf("pending Resume=%v %v", query, err)
			}
			noinput, err := collectPublicExec(c, &v1.ExecRequest{Session: uid, Harness: "caller-must-not-reroute", Config: []byte("wrong"), ResumeFromSeq: 999})
			if err != nil || len(noinput) != 2 || !proto.Equal(noinput[0].GetSession(), noinput[1].GetSession()) {
				t.Fatalf("pending noinput=%v %v", noinput, err)
			}
			if b.ioCounts() != beforeIO || !reflect.DeepEqual(before, routingRecords(t, store.Session(uid))) {
				t.Fatal("pending query used compute or mutated log")
			}
			for _, decided := range []bool{false, true} {
				if decided {
					ref := paused.PendingApproval
					q := &v1.ApproveRequest{Session: uid, ExecutionId: ref.ExecutionId, ToolCallId: ref.ToolCallId, RequestSeq: ref.RequestSeq, Approved: proto.Bool(approved)}
					if _, err := c.Approve(t.Context(), q); err != nil {
						t.Fatal(err)
					}
				}
				_, err := collectPublicExec(c, &v1.ExecRequest{Session: uid, Harness: "unserved", Inputs: []*v1.Message{wire.MessageToProto(api.TextMessage("user", "new"))}})
				if status.Code(err) != codes.FailedPrecondition {
					t.Fatalf("new input decided=%v: %v", decided, err)
				}
				if b.ioCounts() != beforeIO {
					t.Fatal("owned input block depends on compute")
				}
			}
			recovered, err := collectPublicExec(c, &v1.ExecRequest{Session: uid, Harness: "ignore", Config: []byte("wrong"), ResumeFromSeq: 999})
			if err != nil {
				t.Fatalf("decided recovery=%v", err)
			}
			current := recovered[len(recovered)-1].GetSession()
			if current == nil || current.PendingApproval != nil || current.ComputeState != v1.ComputeState_COMPUTE_LIVE {
				t.Fatalf("final recovery=%v", current)
			}
			if current.LastSeq <= paused.LastSeq || current.LastSeq != int64(len(routingRecords(t, store.Session(uid)))) {
				t.Fatal("recovery final cursor wrong")
			}
			first, second := <-h.starts, <-h.starts
			if first.ExecutionID != second.ExecutionID || string(second.Config) != "original" || second.ResumeFromSeq != 7 || len(second.Inputs) != 1 || second.Inputs[0].Text() != "write" {
				t.Fatalf("recovery rewrote invocation: first=%+v second=%+v", first, second)
			}
			wantEffects := int32(0)
			if approved {
				wantEffects = 1
			}
			if effects.Load() != wantEffects {
				t.Fatalf("effects=%d want %d", effects.Load(), wantEffects)
			}
			ref := paused.PendingApproval
			retry, err := c.Approve(t.Context(), &v1.ApproveRequest{Session: uid, ExecutionId: ref.ExecutionId, ToolCallId: ref.ToolCallId, RequestSeq: ref.RequestSeq, Approved: proto.Bool(approved)})
			if err != nil || retry.Decision.GetSeq() <= ref.RequestSeq || retry.Session.LastSeq != current.LastSeq {
				t.Fatalf("historical retry=%v %v", retry, err)
			}
			if _, err := collectPublicExec(c, &v1.ExecRequest{Session: uid, Inputs: []*v1.Message{wire.MessageToProto(api.TextMessage("user", "new"))}}); err != nil {
				t.Fatalf("receipt did not unlock new turn: %v", err)
			}
			beforeRetry := routingRecords(t, store.Session(uid))
			afterTurn, err := c.Approve(t.Context(), &v1.ApproveRequest{Session: uid, ExecutionId: ref.ExecutionId, ToolCallId: ref.ToolCallId, RequestSeq: ref.RequestSeq, Approved: proto.Bool(approved)})
			if err != nil || !proto.Equal(afterTurn.GetDecision(), retry.GetDecision()) || afterTurn.GetSession().GetPendingApproval().GetExecutionId() == ref.ExecutionId || !reflect.DeepEqual(beforeRetry, routingRecords(t, store.Session(uid))) {
				t.Fatalf("exact retry after newer turn changed decision/current gate: %v %v", afterTurn, err)
			}
		})
	}
}

func TestPublicApprovalRepairAndRecoveryCASDeadline(t *testing.T) {
	for _, cut := range []api.EventKind{api.EventToolCall, api.EventApprovalRequest} {
		t.Run(string(cut), func(t *testing.T) {
			store := openStore(t, ":memory:")
			r, b := publicGateRegistry(t, &publicGateHarness{}, nil)
			c := serveRegistry(t, store, r)
			uid := mustCreate(t, c)
			seedPublicGate(t, store.Session(uid), "gate", cut)
			before := routingRecords(t, store.Session(uid))
			for _, q := range []*v1.ExecRequest{{Session: uid, ExpectedLastSeq: proto.Int64(0)}, {Session: uid, DeadlineUnix: time.Now().Add(-time.Minute).Unix()}} {
				_, err := collectPublicExec(c, q)
				want := codes.Aborted
				if q.DeadlineUnix != 0 {
					want = codes.DeadlineExceeded
				}
				if status.Code(err) != want {
					t.Fatalf("recovery precondition=%v want %v", err, want)
				}
				if !reflect.DeepEqual(before, routingRecords(t, store.Session(uid))) {
					t.Fatal("failed recovery wrote repair")
				}
			}
			frames, err := collectPublicExec(c, &v1.ExecRequest{Session: uid, ExpectedLastSeq: proto.Int64(int64(len(before)))})
			if err != nil {
				t.Fatal(err)
			}
			final := frames[len(frames)-1].GetSession()
			if final.GetLastSeq() != 3 || final.GetPendingApproval().GetRequestSeq() != 3 || final.GetExecState() != v1.ExecState_EXEC_AWAITING || final.GetComputeState() != v1.ComputeState_COMPUTE_LIVE {
				t.Fatalf("repair/query=%v", final)
			}
			if b.ioCounts() != ([4]int32{}) {
				t.Fatal("repair used compute")
			}
			if cut == api.EventToolCall && (len(frames) != 3 || frames[1].GetRecord().GetEvent().GetKind() != v1.EventKind_EVENT_APPROVAL_REQUEST) {
				t.Fatalf("repair observer frames=%v", frames)
			}
		})
	}
}

func TestPublicApprovalMissingExecutorRecoverableAndInheritedDecisionRefused(t *testing.T) {
	for _, completed := range []bool{false, true} {
		t.Run(map[bool]string{false: "awaiting inherited refusal", true: "completed inherited receipt"}[completed], func(t *testing.T) {
			store := openStore(t, ":memory:")
			r, b := publicGateRegistry(t, &publicGateHarness{}, nil)
			c := serveRegistry(t, store, r)
			uid := mustCreate(t, c)
			seedPublicGate(t, store.Session(uid), "gate", api.EventApprovalRequest)
			atSeq := int64(3)
			if completed {
				dec, err := c.Approve(t.Context(), publicDecision(uid))
				if err != nil {
					t.Fatal(err)
				}
				d := dec.Decision
				if _, err = store.Session(uid).Append(d.Seq, d.Fence, api.Event{ExecutionID: "execution", Kind: api.EventToolResult, Result: &api.ToolResult{ID: "call", IsError: true, Code: api.ToolResultCodeApprovalDenied, ApprovalRequestSeq: 3, ApprovalDecisionSeq: d.Seq}}); err != nil {
					t.Fatal(err)
				}
				atSeq = 5
			}
			children, err := c.Fork(t.Context(), &v1.ForkRequest{Session: uid, AtSeq: atSeq, Count: 1})
			if err != nil {
				t.Fatal(err)
			}
			child := children.Children[0].Metadata.Uid
			if completed {
				resumed, err := c.Resume(t.Context(), &v1.ResumeRequest{Session: child})
				if err != nil || resumed.PendingApproval != nil {
					t.Fatalf("completed inherited receipt=%v %v", resumed, err)
				}
				return
			}
			if _, err := c.Approve(t.Context(), publicDecision(child)); status.Code(err) != codes.FailedPrecondition {
				t.Fatalf("inherited decision=%v", err)
			}
			if _, err := c.Resume(t.Context(), &v1.ResumeRequest{Session: child}); status.Code(err) != codes.FailedPrecondition {
				t.Fatalf("inherited resume=%v", err)
			}
			if _, err := collectPublicExec(c, &v1.ExecRequest{Session: child}); status.Code(err) != codes.FailedPrecondition {
				t.Fatalf("inherited no-input recovery=%v", err)
			}
			frames, err := collectPublicExec(c, &v1.ExecRequest{Session: child, Inputs: []*v1.Message{wire.MessageToProto(api.TextMessage("user", "explicit new turn"))}})
			if err != nil || frames[len(frames)-1].GetSession().GetPendingApproval().GetExecutionId() == "execution" || frames[len(frames)-1].GetSession().GetPendingApproval() == nil {
				t.Fatalf("inherited child cannot explicitly start its own gate: %v %v", frames, err)
			}

			// Missing executor leaves the parent decision recoverable without another snapshot.
			beforeSnapshots := b.snapshots.Load()
			q := publicDecision(uid)
			q.Approved = proto.Bool(true)
			if _, err := c.Approve(t.Context(), q); err != nil {
				t.Fatal(err)
			}
			_, err = c.Resume(t.Context(), &v1.ResumeRequest{Session: uid})
			if status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "no tool executor") {
				t.Fatalf("missing executor=%v", err)
			}
			got, err := c.GetSession(t.Context(), &v1.GetSessionRequest{Uid: uid})
			if err != nil || got.PendingApproval != nil || got.LastSeq != 4 {
				t.Fatalf("recoverable decision state=%v %v", got, err)
			}
			if b.snapshots.Load() != beforeSnapshots {
				t.Fatal("missing executor snapshotted")
			}
		})
	}
}

func TestPublicApprovalDecidedRecordedRoute(t *testing.T) {
	for _, tc := range []struct {
		name, version, descriptor string
		static, unserved, allowed bool
	}{
		{name: "exact version", version: "v1", allowed: true},
		{name: "version changed", version: "v2"},
		{name: "registration descriptor pin", version: "v1", descriptor: "expected-other-id"},
		{name: "static override", version: "v1", static: true, allowed: true},
		{name: "unserved recorded harness", version: "v1", unserved: true},
	} {
		for _, operation := range []string{"Resume", "Exec"} {
			t.Run(tc.name+"/"+operation, func(t *testing.T) {
				store := openStore(t, ":memory:")
				if err := store.PutSession(sqlitelog.SessionMeta{UID: "s", Harness: "default"}); err != nil {
					t.Fatal(err)
				}
				h := &publicGateHarness{version: tc.version, starts: make(chan api.Start, 2)}
				defaultHarness := &publicGateHarness{}
				a := &publicGateBackend{Backend: local.New(defaultHarness)}
				b := &publicGateBackend{Backend: local.New(h)}
				t.Cleanup(func() { _ = a.Close(); _ = b.Close() })
				var models atomic.Int32
				model := func(context.Context, api.ModelRequest) (api.ModelResponse, error) {
					models.Add(1)
					return api.ModelResponse{}, nil
				}
				opts := []placement.Option{}
				if tc.descriptor != "" {
					opts = append(opts, placement.WithDescriptorID(tc.descriptor))
				}
				routes := map[string]*placement.Placer{"default": placement.New(a, model)}
				if !tc.unserved {
					routes["recorded"] = placement.New(b, model, opts...)
				}
				r, err := placement.NewRegistry("default", routes)
				if err != nil {
					t.Fatal(err)
				}
				c := serveRegistry(t, store, r)
				decision := publicDecision("s")
				var first api.Start
				if tc.static {
					frames, err := collectPublicExec(c, &v1.ExecRequest{Session: "s", Harness: "recorded", Config: []byte("original"), ResumeFromSeq: 7, Inputs: []*v1.Message{wire.MessageToProto(api.TextMessage("user", "write"))}})
					if err != nil {
						t.Fatal(err)
					}
					ref := frames[len(frames)-1].GetSession().GetPendingApproval()
					decision.ExecutionId, decision.ToolCallId, decision.RequestSeq = ref.ExecutionId, ref.ToolCallId, ref.RequestSeq
					first = <-h.starts
					// A stored-default registration must not replace the recorded static override.
					if _, _, err := store.RegisterHarness(sqlitelog.HarnessRecord{Name: "default", UID: "reg", Spec: "{}", SpecDigest: "digest"}); err != nil {
						t.Fatal(err)
					}
				} else {
					seedPublicGate(t, store.Session("s"), "recorded", api.EventApprovalRequest)
				}
				if _, err := c.Approve(t.Context(), decision); err != nil {
					t.Fatal(err)
				}
				before := routingRecords(t, store.Session("s"))
				defaultIO := a.ioCounts() // the initial override may Describe the stored harness
				var current *v1.Session
				if operation == "Resume" {
					current, err = c.Resume(t.Context(), &v1.ResumeRequest{Session: "s"})
				} else {
					var frames []*v1.ExecUpdate
					frames, err = collectPublicExec(c, &v1.ExecRequest{Session: "s", Harness: "wrong", Config: []byte("wrong"), ResumeFromSeq: 999})
					if err == nil {
						current = frames[len(frames)-1].GetSession()
					}
				}
				want := codes.FailedPrecondition
				if tc.allowed {
					want = codes.OK
				}
				if status.Code(err) != want {
					t.Fatalf("decided recovery=%v want %v", err, want)
				}
				if a.ioCounts() != defaultIO || defaultHarness.runs.Load() != 0 || models.Load() != 0 {
					t.Fatalf("fallback: default IO=%v runs=%d models=%d", a.ioCounts(), defaultHarness.runs.Load(), models.Load())
				}
				if !tc.allowed {
					if h.runs.Load() != 0 || !reflect.DeepEqual(before, routingRecords(t, store.Session("s"))) || tc.unserved && b.ioCounts() != ([4]int32{}) {
						t.Fatal("refused route/version/id used compute or wrote execution records")
					}
					return
				}
				start := <-h.starts
				if string(start.Config) != "original" || start.ResumeFromSeq != 7 || b.restores.Load() != 1 || current.GetHarness() != "default" || current.PendingApproval != nil {
					t.Fatalf("recorded recovery=%+v session=%v restores=%d", start, current, b.restores.Load())
				}
				if tc.static {
					if start.ExecutionID != first.ExecutionID || len(start.Inputs) != 1 || start.Inputs[0].Text() != "write" {
						t.Fatalf("static override rewrote invocation: first=%+v recovered=%+v", first, start)
					}
				} else if start.ExecutionID != "execution" || len(start.Inputs) != 0 {
					t.Fatalf("seeded invocation rewritten: %+v", start)
				}
			})
		}
	}
}

type publicReceiptHarness struct{}

func (publicReceiptHarness) Describe(ctx context.Context) (api.Descriptor, error) {
	return (&publicGateHarness{}).Describe(ctx)
}

func (publicReceiptHarness) Run(ctx context.Context, _ *api.Start, sink api.EventSink) error {
	result, err := sink.ToolCall(ctx, publicGateCall("call"))
	if err != nil {
		return err
	}
	_, err = sink.Model(ctx, api.ModelRequest{Model: "test-model", Messages: []api.Message{
		{Role: "tool", Parts: []api.Part{{ToolResult: &result}}},
	}})
	return err
}

func TestPublicApprovalExecutorFailureReceiptContinuesModel(t *testing.T) {
	store := openStore(t, ":memory:")
	b := local.New(publicReceiptHarness{})
	t.Cleanup(func() { _ = b.Close() })
	var effects, models atomic.Int32
	p := placement.New(b, func(_ context.Context, req api.ModelRequest) (api.ModelResponse, error) {
		models.Add(1)
		if len(req.Messages) != 1 || len(req.Messages[0].Parts) != 1 {
			t.Errorf("model did not receive the tool receipt: %+v", req)
		} else if receipt := req.Messages[0].Parts[0].ToolResult; receipt == nil || receipt.ID != "call" || !receipt.IsError || receipt.Code != api.ToolResultCodeExecutorError || receipt.Error != "executor offline" || (&api.Message{Parts: receipt.Content}).Text() != "Tool execution failed." || receipt.ApprovalRequestSeq <= 0 || receipt.ApprovalDecisionSeq <= receipt.ApprovalRequestSeq {
			t.Errorf("model receipt lost failure/correlation: %+v", receipt)
		}
		return api.ModelResponse{Message: *api.TextMessage("assistant", "handled executor failure")}, nil
	}, placement.WithToolExecutor(func(_ context.Context, scope controller.ToolCallContext, _ api.ToolCall) (api.ToolResult, error) {
		effects.Add(1)
		if scope.SessionUID == "" {
			t.Error("executor missing session scope")
		}
		return api.ToolResult{}, errors.New("executor offline")
	}))
	r, err := placement.NewRegistry("gate", map[string]*placement.Placer{"gate": p})
	if err != nil {
		t.Fatal(err)
	}
	c := serveRegistry(t, store, r)
	uid := mustCreate(t, c)
	frames, err := collectPublicExec(c, &v1.ExecRequest{Session: uid})
	if err != nil {
		t.Fatal(err)
	}
	ref := frames[len(frames)-1].GetSession().GetPendingApproval()
	decision, err := c.Approve(t.Context(), &v1.ApproveRequest{Session: uid, ExecutionId: ref.ExecutionId, ToolCallId: ref.ToolCallId, RequestSeq: ref.RequestSeq, Approved: proto.Bool(true)})
	if err != nil {
		t.Fatal(err)
	}
	current, err := c.Resume(t.Context(), &v1.ResumeRequest{Session: uid})
	if err != nil {
		t.Fatalf("handled executor receipt became RPC error: %v", err)
	}
	if current.PendingApproval != nil || effects.Load() != 1 || models.Load() != 1 {
		t.Fatalf("completed recovery=%v effects=%d models=%d", current, effects.Load(), models.Load())
	}
	records := routingRecords(t, store.Session(uid))
	receipt := routingEvent(t, records, api.EventToolResult).Result
	if receipt.Code != api.ToolResultCodeExecutorError || !receipt.IsError || receipt.ApprovalRequestSeq != ref.RequestSeq || receipt.ApprovalDecisionSeq != decision.Decision.Seq {
		t.Fatalf("durable failure receipt=%+v", receipt)
	}
	if output := routingEvent(t, records, api.EventOutput).Message; output == nil || output.Text() != "handled executor failure" {
		t.Fatalf("model continuation output=%+v", output)
	}
	for _, record := range records {
		if record.Event.Kind == api.EventError {
			t.Fatalf("handled receipt journaled ERROR: %+v", record)
		}
	}
}

func TestApprovalAllRepliesAndStrictCommand(t *testing.T) {
	store := openStore(t, ":memory:")
	r, _ := publicGateRegistry(t, &publicGateHarness{}, nil)
	c := serveRegistry(t, store, r)
	created, err := c.CreateSession(t.Context(), &v1.CreateSessionRequest{Session: &v1.Session{PendingApproval: &v1.ApprovalRef{RequestSeq: 99}, ExecState: v1.ExecState_EXEC_AWAITING}})
	if err != nil || created.LastSeq != 0 || created.PendingApproval != nil || created.ExecState != v1.ExecState_EXEC_PENDING {
		t.Fatalf("create=%v %v", created, err)
	}
	uid := created.Metadata.Uid
	seedPublicGate(t, store.Session(uid), "gate", api.EventApprovalRequest)
	check := func(name string, s *v1.Session, want bool) {
		t.Helper()
		head, _ := store.Session(s.GetMetadata().GetUid()).Head()
		if s.LastSeq != head || (s.PendingApproval != nil) != want {
			t.Fatalf("%s=%v head=%d", name, s, head)
		}
	}
	got, err := c.GetSession(t.Context(), &v1.GetSessionRequest{Uid: uid})
	if err != nil {
		t.Fatal(err)
	}
	check("Get", got, true)
	page, err := c.ListSessions(t.Context(), &v1.ListSessionsRequest{PageSize: 1})
	if err != nil || len(page.Sessions) != 1 {
		t.Fatalf("List=%v %v", page, err)
	}
	check("List", page.Sessions[0], true)
	children, err := c.Fork(t.Context(), &v1.ForkRequest{Session: uid, AtSeq: 3, Count: 1})
	if err != nil || len(children.Children) != 1 {
		t.Fatalf("Fork=%v %v", children, err)
	}
	check("Fork", children.Children[0], false)
	suspended, err := c.Suspend(t.Context(), &v1.SuspendRequest{Session: uid})
	if err != nil {
		t.Fatal(err)
	}
	check("Suspend", suspended, true)
	decided, err := c.Approve(t.Context(), publicDecision(uid))
	if err != nil {
		t.Fatal(err)
	}
	check("Approve", decided.Session, false)
	// Reporting is forgiving; commands still reject the same malformed durable evidence.
	log := store.Session(uid)
	head, _ := log.Head()
	f, _ := log.NewFence()
	if _, err = log.Append(head, f, api.Event{ExecutionID: "execution", Kind: api.EventOutput, Message: api.TextMessage("assistant", "illegal before receipt")}); err != nil {
		t.Fatal(err)
	}
	if _, err = c.GetSession(t.Context(), &v1.GetSessionRequest{Uid: uid}); err != nil {
		t.Fatalf("report rejected corruption=%v", err)
	}
	if _, err = c.Approve(t.Context(), publicDecision(uid)); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("strict approve=%v", err)
	}
}

func TestApprovalSecondPark(t *testing.T) {
	store := openStore(t, ":memory:")
	r, _ := publicGateRegistry(t, &publicGateHarness{second: true}, nil)
	c := serveRegistry(t, store, r)
	uid := mustCreate(t, c)
	frames, err := collectPublicExec(c, &v1.ExecRequest{Session: uid})
	if err != nil {
		t.Fatal(err)
	}
	first := frames[len(frames)-1].GetSession().GetPendingApproval()
	q := &v1.ApproveRequest{Session: uid, ExecutionId: first.ExecutionId, ToolCallId: first.ToolCallId, RequestSeq: first.RequestSeq, Approved: proto.Bool(false)}
	original, err := c.Approve(t.Context(), q)
	if err != nil {
		t.Fatal(err)
	}
	after, err := collectPublicExec(c, &v1.ExecRequest{Session: uid})
	if err != nil {
		t.Fatal(err)
	}
	next := after[len(after)-1].GetSession()
	if next.PendingApproval.GetToolCallId() != "second" || next.ComputeState != v1.ComputeState_COMPUTE_COLD {
		t.Fatalf("second=%v", next)
	}
	recs, err := store.Session(uid).Read(original.Session.LastSeq + 1)
	if err != nil {
		t.Fatal(err)
	}
	for _, rec := range recs {
		if rec.Event.Kind == api.EventEnd || rec.Event.Kind == api.EventError || rec.Event.Lifecycle != nil && rec.Event.Lifecycle.Kind == api.LifecycleResume {
			t.Fatalf("second park terminal=%v", rec)
		}
	}
	retry, err := c.Approve(t.Context(), q)
	if err != nil || !proto.Equal(retry.Decision, original.Decision) || !proto.Equal(retry.Session.PendingApproval, next.PendingApproval) {
		t.Fatalf("historical retry=%v %v", retry, err)
	}
}

func TestApprovalBusyVersusBackendAborted(t *testing.T) {
	store := openStore(t, ":memory:")
	r, b := publicGateRegistry(t, &publicGateHarness{}, nil)
	c := serveRegistry(t, store, r)
	uid := mustCreate(t, c)
	b.snapshotErr = status.Error(codes.Aborted, "backend conflict")
	b.beforeSnapshot = func() {
		info, err := store.SessionInfo(uid)
		if err != nil {
			t.Fatal(err)
		}
		ref := info.PendingApproval
		_, err = c.Approve(t.Context(), &v1.ApproveRequest{Session: uid, ExecutionId: ref.ExecutionID, ToolCallId: ref.ToolCallID, RequestSeq: ref.RequestSeq, Approved: proto.Bool(false)})
		if status.Code(err) != codes.Aborted {
			t.Errorf("local busy=%v", err)
		}
	}
	frames, err := collectPublicExec(c, &v1.ExecRequest{Session: uid})
	if status.Code(err) != codes.Internal || frames[len(frames)-1].GetSession() != nil {
		t.Fatalf("backend Aborted fabricated pause: frames=%v err=%v", frames, err)
	}
	info, err := store.SessionInfo(uid)
	if err != nil {
		t.Fatal(err)
	}
	ref := info.PendingApproval
	if _, err = c.Approve(t.Context(), &v1.ApproveRequest{Session: uid, ExecutionId: ref.ExecutionID, ToolCallId: ref.ToolCallID, RequestSeq: ref.RequestSeq, Approved: proto.Bool(false)}); err != nil {
		t.Fatalf("failed snapshot retained guard=%v", err)
	}
	b.beforeSnapshot = nil
	b.snapshotErr = nil
	suspended, err := c.Suspend(t.Context(), &v1.SuspendRequest{Session: uid})
	if err != nil || suspended.ComputeState != v1.ComputeState_COMPUTE_COLD {
		t.Fatalf("snapshot retry=%v %v", suspended, err)
	}
}

// Over the public API, a transient policy refusal on an approved call is FAILED_PRECONDITION with
// no receipt, and the next Resume asks again. A definitive refusal commits a POLICY_DENIED receipt
// that the harness hands to the model, and a later Resume does not ask again.
func TestPublicApprovalToolPolicyRefusals(t *testing.T) {
	store := openStore(t, ":memory:")
	b := local.New(publicReceiptHarness{})
	t.Cleanup(func() { _ = b.Close() })
	var effects, models, asked atomic.Int32
	verdicts := []controller.ToolPolicyVerdict{controller.ToolPolicyTransientRefusal, controller.ToolPolicyDefinitiveRefusal}
	p := placement.New(b, func(_ context.Context, req api.ModelRequest) (api.ModelResponse, error) {
		models.Add(1)
		if len(req.Messages) != 1 || len(req.Messages[0].Parts) != 1 {
			t.Errorf("model did not receive the tool receipt: %+v", req)
		} else if receipt := req.Messages[0].Parts[0].ToolResult; receipt == nil || receipt.Code != api.ToolResultCodePolicyDenied || !receipt.IsError || receipt.Error != "grant revoked" {
			t.Errorf("model receipt = %+v", receipt)
		}
		return api.ModelResponse{Message: *api.TextMessage("assistant", "handled refusal")}, nil
	}, placement.WithToolExecutor(func(context.Context, controller.ToolCallContext, api.ToolCall) (api.ToolResult, error) {
		effects.Add(1)
		return api.ToolResult{}, nil
	}), placement.WithToolPolicy(func(_ context.Context, in controller.ToolPolicyInput) controller.ToolPolicyDecision {
		n := asked.Add(1)
		if in.Approval == nil || !in.Approval.Decision.Approved || in.Scope.SessionUID == "" {
			t.Errorf("hook input = %+v", in)
		}
		return controller.ToolPolicyDecision{Verdict: verdicts[n-1], Reason: map[int32]string{1: "provider outage", 2: "grant revoked"}[n]}
	}))
	r, err := placement.NewRegistry("gate", map[string]*placement.Placer{"gate": p})
	if err != nil {
		t.Fatal(err)
	}
	c := serveRegistry(t, store, r)
	uid := mustCreate(t, c)
	frames, err := collectPublicExec(c, &v1.ExecRequest{Session: uid})
	if err != nil {
		t.Fatal(err)
	}
	if asked.Load() != 0 {
		t.Fatal("hook asked before the approval")
	}
	ref := frames[len(frames)-1].GetSession().GetPendingApproval()
	decision, err := c.Approve(t.Context(), &v1.ApproveRequest{Session: uid, ExecutionId: ref.ExecutionId, ToolCallId: ref.ToolCallId, RequestSeq: ref.RequestSeq, Approved: proto.Bool(true)})
	if err != nil {
		t.Fatal(err)
	}
	before := routingRecords(t, store.Session(uid))
	if _, err := c.Resume(t.Context(), &v1.ResumeRequest{Session: uid}); status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "provider outage") {
		t.Fatalf("transient refusal Resume = %v, want FAILED_PRECONDITION with the reason", err)
	}
	for _, record := range routingRecords(t, store.Session(uid))[len(before):] {
		if record.Event.Kind == api.EventToolResult || record.Event.Kind == api.EventError || record.Event.Kind == api.EventEnd {
			t.Fatalf("transient refusal journaled %s", record.Event.Kind)
		}
	}
	current, err := c.Resume(t.Context(), &v1.ResumeRequest{Session: uid})
	if err != nil {
		t.Fatalf("definitive refusal became an RPC error: %v", err)
	}
	if current.PendingApproval != nil || effects.Load() != 0 || models.Load() != 1 || asked.Load() != 2 {
		t.Fatalf("session=%v effects=%d models=%d asked=%d", current, effects.Load(), models.Load(), asked.Load())
	}
	records := routingRecords(t, store.Session(uid))
	receipt := routingEvent(t, records, api.EventToolResult).Result
	if receipt.Code != api.ToolResultCodePolicyDenied || !receipt.IsError || receipt.ApprovalRequestSeq != ref.RequestSeq || receipt.ApprovalDecisionSeq != decision.Decision.Seq {
		t.Fatalf("durable refusal receipt=%+v", receipt)
	}
	if _, err := c.Resume(t.Context(), &v1.ResumeRequest{Session: uid}); err != nil || asked.Load() != 2 || effects.Load() != 0 {
		t.Fatalf("Resume after the receipt = %v asked=%d effects=%d", err, asked.Load(), effects.Load())
	}
}

type publicOrdinaryToolHarness struct{}

func (publicOrdinaryToolHarness) Describe(ctx context.Context) (api.Descriptor, error) {
	return (&publicGateHarness{}).Describe(ctx)
}

func (publicOrdinaryToolHarness) Run(ctx context.Context, _ *api.Start, sink api.EventSink) error {
	_, err := sink.ToolCall(ctx, api.ToolCall{ID: "call", Tool: "write", IdempotencyKey: "key", Mediation: api.MediationControllerMediated})
	return err
}

// A transient policy refusal of an ordinary call that ends the turn is FAILED_PRECONDITION on
// Exec, with the intent left without a receipt and the executor not called.
func TestPublicOrdinaryToolPolicyTransientRefusal(t *testing.T) {
	store := openStore(t, ":memory:")
	b := local.New(publicOrdinaryToolHarness{})
	t.Cleanup(func() { _ = b.Close() })
	var effects atomic.Int32
	p := placement.New(b, nil, placement.WithToolExecutor(func(context.Context, controller.ToolCallContext, api.ToolCall) (api.ToolResult, error) {
		effects.Add(1)
		return api.ToolResult{}, nil
	}), placement.WithToolPolicy(func(context.Context, controller.ToolPolicyInput) controller.ToolPolicyDecision {
		return controller.ToolPolicyDecision{Verdict: controller.ToolPolicyTransientRefusal, Reason: "provider outage"}
	}))
	r, err := placement.NewRegistry("gate", map[string]*placement.Placer{"gate": p})
	if err != nil {
		t.Fatal(err)
	}
	c := serveRegistry(t, store, r)
	uid := mustCreate(t, c)
	if _, err := collectPublicExec(c, &v1.ExecRequest{Session: uid, Inputs: []*v1.Message{wire.MessageToProto(api.TextMessage("user", "go"))}}); status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "provider outage") {
		t.Fatalf("Exec = %v, want FAILED_PRECONDITION with the reason", err)
	}
	records := routingRecords(t, store.Session(uid))
	for _, record := range records {
		if record.Event.Kind == api.EventToolResult {
			t.Fatalf("transient refusal journaled a receipt: %+v", record)
		}
	}
	if routingEvent(t, records, api.EventToolCall).ToolCall == nil || effects.Load() != 0 {
		t.Fatalf("intent missing or executor ran: effects=%d", effects.Load())
	}
}

// Over the public API, an approved call's executor reply carrying the host-owned POLICY_DENIED
// code is FAILED_PRECONDITION with no receipt, ERROR or END, and the decision stays recoverable.
func TestPublicApprovalExecutorPolicyDeniedRejected(t *testing.T) {
	store := openStore(t, ":memory:")
	b := local.New(publicReceiptHarness{})
	t.Cleanup(func() { _ = b.Close() })
	var effects atomic.Int32
	p := placement.New(b, nil, placement.WithToolExecutor(func(context.Context, controller.ToolCallContext, api.ToolCall) (api.ToolResult, error) {
		effects.Add(1)
		return api.ToolResult{IsError: true, Code: api.ToolResultCodePolicyDenied, Error: "forged"}, nil
	}))
	r, err := placement.NewRegistry("gate", map[string]*placement.Placer{"gate": p})
	if err != nil {
		t.Fatal(err)
	}
	c := serveRegistry(t, store, r)
	uid := mustCreate(t, c)
	frames, err := collectPublicExec(c, &v1.ExecRequest{Session: uid})
	if err != nil {
		t.Fatal(err)
	}
	ref := frames[len(frames)-1].GetSession().GetPendingApproval()
	if _, err := c.Approve(t.Context(), &v1.ApproveRequest{Session: uid, ExecutionId: ref.ExecutionId, ToolCallId: ref.ToolCallId, RequestSeq: ref.RequestSeq, Approved: proto.Bool(true)}); err != nil {
		t.Fatal(err)
	}
	before := routingRecords(t, store.Session(uid))
	if _, err := c.Resume(t.Context(), &v1.ResumeRequest{Session: uid}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("Resume with a forged POLICY_DENIED = %v, want FAILED_PRECONDITION", err)
	}
	for _, record := range routingRecords(t, store.Session(uid))[len(before):] {
		if record.Event.Kind == api.EventToolResult || record.Event.Kind == api.EventError || record.Event.Kind == api.EventEnd {
			t.Fatalf("forged POLICY_DENIED journaled %s", record.Event.Kind)
		}
	}
	if effects.Load() != 1 {
		t.Fatalf("executor calls = %d, want 1", effects.Load())
	}
	if _, err := c.Resume(t.Context(), &v1.ResumeRequest{Session: uid}); status.Code(err) != codes.FailedPrecondition || effects.Load() != 2 {
		t.Fatalf("second Resume = %v (executor calls %d), want the decision re-driven and refused again", err, effects.Load())
	}
}
