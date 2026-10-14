package sqlitelog_test

import (
	"errors"
	"reflect"
	"testing"

	"github.com/aramase/agentsessions/api"
	"github.com/aramase/agentsessions/controller"
	"github.com/aramase/agentsessions/sqlitelog"
	"google.golang.org/protobuf/proto"
)

// Append remains permissive, but reporting cannot use a body or enum discarded by wire encoding.
func TestApprovalProjectionUsesPersistedEvent(t *testing.T) {
	start := api.Event{ExecutionID: "execution", Kind: api.EventExecutionStart, ExecutionStart: &api.ExecutionStart{InputCount: proto.Int64(0)}}
	call := api.Event{ExecutionID: "execution", Kind: api.EventToolCall, ToolCall: &api.ToolCall{ID: "call", IdempotencyKey: "key", Mediation: api.MediationRequiresApproval}}
	request := api.Event{ExecutionID: "execution", Kind: api.EventApprovalRequest, Approval: &api.ApprovalRequest{ToolCallID: "call"}}
	message := api.TextMessage("assistant", "selected wire body")
	pending := api.ApprovalRef{ExecutionID: "execution", ToolCallID: "call", RequestSeq: 3}
	for _, tc := range []struct {
		name      string
		events    []api.Event
		checkSeq  int64
		wantEvent api.Event
		wantRef   api.ApprovalRef
		wantErr   error
	}{
		{
			name:     "message discards approval request",
			events:   []api.Event{start, call, {ExecutionID: "execution", Kind: api.EventApprovalRequest, Message: message, Approval: request.Approval}},
			checkSeq: 3, wantEvent: api.Event{ExecutionID: "execution", Kind: api.EventApprovalRequest, Message: message},
			wantErr: controller.ErrInvalidExecutionLog,
		},
		{
			name:     "message discards execution start",
			events:   []api.Event{{ExecutionID: "execution", Kind: api.EventExecutionStart, Message: message, ExecutionStart: start.ExecutionStart}, call, request},
			checkSeq: 1, wantEvent: api.Event{ExecutionID: "execution", Kind: api.EventExecutionStart, Message: message},
			wantErr: controller.ErrInvalidExecutionLog,
		},
		{
			name:     "message discards gated call",
			events:   []api.Event{start, {ExecutionID: "execution", Kind: api.EventToolCall, Message: message, ToolCall: call.ToolCall}, request},
			checkSeq: 2, wantEvent: api.Event{ExecutionID: "execution", Kind: api.EventToolCall, Message: message},
			wantErr: controller.ErrInvalidExecutionLog,
		},
		{
			name:     "message discards fork marker",
			events:   []api.Event{start, call, request, {Kind: api.EventLifecycle, Message: message, Lifecycle: &api.Lifecycle{Kind: api.LifecycleFork}}},
			checkSeq: 4, wantEvent: api.Event{Kind: api.EventLifecycle, Message: message},
			wantRef: pending,
		},
		{
			name:     "unknown event kind normalizes to unspecified",
			events:   []api.Event{start, call, request, {ExecutionID: "execution", Kind: api.EventKind("EVENT_ERROR"), Err: &api.Error{Description: "not a recognized Go kind"}}},
			checkSeq: 4, wantEvent: api.Event{ExecutionID: "execution", Err: &api.Error{Description: "not a recognized Go kind"}},
			wantErr: controller.ErrInvalidExecutionLog,
		},
		{
			name:     "unknown mediation normalizes to unspecified",
			events:   []api.Event{start, {ExecutionID: "execution", Kind: api.EventToolCall, ToolCall: &api.ToolCall{ID: "ordinary", Mediation: api.Mediation("MEDIATION_REQUIRES_APPROVAL")}}, call, request},
			checkSeq: 2, wantEvent: api.Event{ExecutionID: "execution", Kind: api.EventToolCall, ToolCall: &api.ToolCall{ID: "ordinary"}},
			wantRef: api.ApprovalRef{ExecutionID: "execution", ToolCallID: "call", RequestSeq: 4},
		},
		{
			name:     "unknown lifecycle kind normalizes to unspecified",
			events:   []api.Event{start, call, request, {Kind: api.EventLifecycle, Lifecycle: &api.Lifecycle{Kind: api.LifecycleKind("LIFECYCLE_FORK")}}},
			checkSeq: 4, wantEvent: api.Event{Kind: api.EventLifecycle, Lifecycle: &api.Lifecycle{}},
			wantRef: pending,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, err := sqlitelog.Open(":memory:")
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			log := store.Session("s")
			for i, ev := range tc.events {
				if _, err := log.Append(int64(i), 0, ev); err != nil {
					t.Fatalf("permissive append seq %d: %v", i+1, err)
				}
			}
			records, err := log.Read(tc.checkSeq)
			if err != nil || len(records) == 0 {
				t.Fatalf("read seq %d: records=%v err=%v", tc.checkSeq, records, err)
			}
			if !reflect.DeepEqual(records[0].Event, tc.wantEvent) {
				t.Fatalf("persisted event=%+v want=%+v", records[0].Event, tc.wantEvent)
			}
			if err := log.Verify(); err != nil {
				t.Fatalf("wire normalization changed hash chain: %v", err)
			}
			strict, err := controller.InspectApproval(log, 0)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("strict inspection=%+v err=%v want=%v", strict, err, tc.wantErr)
			}
			if tc.wantRef.RequestSeq > 0 && (strict == nil || strict.Request == nil || strict.Request.Seq != tc.wantRef.RequestSeq) {
				t.Fatalf("reference unsupported by persisted evidence: %+v", strict)
			}
			info, err := store.SessionInfo("s")
			if err != nil {
				t.Fatal(err)
			}
			if info.LastSeq != int64(len(tc.events)) || info.PendingApproval != tc.wantRef {
				t.Errorf("metadata cursor=%d ref=%+v want cursor=%d ref=%+v", info.LastSeq, info.PendingApproval, len(tc.events), tc.wantRef)
			}
			page, err := store.ListSessions(sqlitelog.ListOptions{})
			if err != nil || len(page.Sessions) != 1 || page.Sessions[0].PendingApproval != tc.wantRef {
				t.Errorf("listing=%+v err=%v want ref=%+v", page, err, tc.wantRef)
			}
		})
	}
}
