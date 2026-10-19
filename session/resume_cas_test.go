package session_test

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/agent-primitives/sessions/api"
	v1 "github.com/agent-primitives/sessions/api/genpb"
	"github.com/agent-primitives/sessions/controller"
	"github.com/agent-primitives/sessions/eventlog"
	"github.com/agent-primitives/sessions/placement"
	"github.com/agent-primitives/sessions/runtime/local"
)

func TestResumeExpectedLastSeq(t *testing.T) {
	for _, tc := range []struct {
		name, cursor string
		empty        bool
	}{
		{name: "moved tail", cursor: "inspected"},
		{name: "matching tail", cursor: "current"},
		{name: "absent cursor"},
		{name: "explicit zero on empty journal", cursor: "current", empty: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := openStore(t, ":memory:")
			h := &routingHarness{id: "descriptor-a", version: "v1"}
			b := &publicGateBackend{Backend: local.New(h)}
			t.Cleanup(func() { _ = b.Close() })
			var effects atomic.Int32
			p := placement.New(b, nil, dedupAllTools, placement.WithToolExecutor(func(context.Context, controller.ToolCallContext, api.ToolCall) (api.ToolResult, error) {
				effects.Add(1)
				return api.ToolResult{Output: map[string]any{"receipt": "recovered"}}, nil
			}))
			c := serveRegistry(t, store, routingRegistry(t, map[string]*placement.Placer{"alias-a": p}))
			uid := mustCreate(t, c)
			inspected, err := c.GetSession(t.Context(), &v1.GetSessionRequest{Uid: uid})
			if err != nil {
				t.Fatal(err)
			}
			log := store.Session(uid)
			if !tc.empty {
				// Another turn starts after inspection, then crashes with an unanswered intent.
				seedRoutingInvocation(t, log, &api.ExecutionStart{InputCount: proto.Int64(1), Harness: "alias-a", HarnessVersion: "v1"}, false)
			}
			before := routingRecords(t, log)
			current, err := c.GetSession(t.Context(), &v1.GetSessionRequest{Uid: uid})
			if err != nil {
				t.Fatal(err)
			}
			req := &v1.ResumeRequest{Session: uid}
			switch tc.cursor {
			case "inspected":
				req.ExpectedLastSeq = proto.Int64(inspected.GetLastSeq())
			case "current":
				req.ExpectedLastSeq = proto.Int64(current.GetLastSeq())
			}
			resumed, err := c.Resume(t.Context(), req)
			if tc.cursor == "inspected" {
				if status.Code(err) != codes.Aborted {
					t.Fatalf("Resume after journal movement = %v, want Aborted", err)
				}
				values := fmt.Sprintf("expected_last_seq %d, head %d", *req.ExpectedLastSeq, current.GetLastSeq())
				if !strings.Contains(status.Convert(err).Message(), values) {
					t.Fatalf("conflict %v lacks %q", err, values)
				}
				assertRefusedResumeCAS(t, c, uid, log, before, current, b)
				if h.runs.Load() != 0 || effects.Load() != 0 {
					t.Fatal("stale Resume ran the harness or re-drove the tool intent")
				}
				return
			}
			if err != nil || resumed.GetLastSeq() <= current.GetLastSeq() || resumed.GetComputeState() != v1.ComputeState_COMPUTE_LIVE {
				t.Fatalf("Resume = %v, %v; want recovery and LIVE compute", resumed, err)
			}
			wantRuns := int32(1)
			if tc.empty {
				wantRuns = 0
			}
			if h.runs.Load() != wantRuns || effects.Load() != wantRuns || b.restores.Load() != 1 {
				t.Fatalf("recovery runs=%d effects=%d restores=%d", h.runs.Load(), effects.Load(), b.restores.Load())
			}
			routingEvent(t, routingRecords(t, log), api.EventLifecycle)
		})
	}
}

func assertRefusedResumeCAS(t *testing.T, c v1.SessionsClient, uid string, log eventlog.Store, before []eventlog.Record, current *v1.Session, b *publicGateBackend) {
	t.Helper()
	after, err := c.GetSession(t.Context(), &v1.GetSessionRequest{Uid: uid})
	if err != nil || !proto.Equal(current, after) || !reflect.DeepEqual(before, routingRecords(t, log)) {
		t.Fatalf("stale Resume changed records/projection: %v, %v", after, err)
	}
	if b.ioCounts() != ([4]int32{}) {
		t.Fatalf("stale Resume used runtime IO: %v", b.ioCounts())
	}
	fence := before[len(before)-1].Fence
	if next, err := log.NewFence(); err != nil || next != fence+1 {
		t.Fatalf("stale Resume minted a fence: next=%d previous=%d err=%v", next, fence, err)
	}
}

func TestResumeExpectedLastSeqBeforeApprovalRepair(t *testing.T) {
	for _, cut := range []api.EventKind{api.EventToolCall, api.EventApprovalRequest} {
		t.Run(string(cut), func(t *testing.T) {
			store := openStore(t, ":memory:")
			r, b := publicGateRegistry(t, &publicGateHarness{}, nil)
			c := serveRegistry(t, store, r)
			uid := mustCreate(t, c)
			log := store.Session(uid)
			seedPublicGate(t, log, "gate", cut)
			before := routingRecords(t, log)
			current, err := c.GetSession(t.Context(), &v1.GetSessionRequest{Uid: uid})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := c.Resume(t.Context(), &v1.ResumeRequest{Session: uid, ExpectedLastSeq: proto.Int64(0)}); status.Code(err) != codes.Aborted {
				t.Fatalf("stale approval Resume = %v, want Aborted", err)
			}
			assertRefusedResumeCAS(t, c, uid, log, before, current, b)
			resumed, err := c.Resume(t.Context(), &v1.ResumeRequest{Session: uid, ExpectedLastSeq: proto.Int64(current.GetLastSeq())})
			if err != nil || resumed.GetExecState() != v1.ExecState_EXEC_AWAITING || resumed.GetPendingApproval().GetRequestSeq() != 3 || resumed.GetLastSeq() != 3 {
				t.Fatalf("matching approval repair/query = %v, %v", resumed, err)
			}
			if b.ioCounts() != ([4]int32{}) {
				t.Fatal("approval repair/query used compute")
			}
		})
	}
}

func TestResumeExpectedLastSeqPrecedesInvocationValidation(t *testing.T) {
	store := openStore(t, ":memory:")
	r, b := publicGateRegistry(t, &publicGateHarness{}, nil)
	c := serveRegistry(t, store, r)
	uid := mustCreate(t, c)
	log := store.Session(uid)
	fence, err := log.NewFence()
	if err != nil {
		t.Fatal(err)
	}
	// A crash before the promised INPUT commits is invalid for recovery, but still has a head.
	if _, err := log.Append(0, fence, api.Event{Kind: api.EventExecutionStart, ExecutionID: "incomplete", ExecutionStart: &api.ExecutionStart{InputCount: proto.Int64(1)}}); err != nil {
		t.Fatal(err)
	}
	before := routingRecords(t, log)
	for _, tc := range []struct {
		expected *int64
		want     codes.Code
	}{
		{proto.Int64(0), codes.Aborted},
		{proto.Int64(1), codes.FailedPrecondition},
		{nil, codes.FailedPrecondition},
	} {
		if _, err := c.Resume(t.Context(), &v1.ResumeRequest{Session: uid, ExpectedLastSeq: tc.expected}); status.Code(err) != tc.want {
			t.Fatalf("incomplete Resume expected=%v: %v, want %v", tc.expected, err, tc.want)
		}
	}
	if b.ioCounts() != ([4]int32{}) || !reflect.DeepEqual(before, routingRecords(t, log)) {
		t.Fatal("refused incomplete Resume used compute or changed records")
	}
}
