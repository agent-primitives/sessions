package sqlitelog

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/aramase/agentsessions/api"
	"github.com/aramase/agentsessions/canon"
	"github.com/aramase/agentsessions/eventlog"
	"github.com/aramase/agentsessions/wire"
	"google.golang.org/protobuf/proto"
)

func approvalEvents(id string) []api.Event {
	return []api.Event{
		{ExecutionID: id, Kind: api.EventExecutionStart, ExecutionStart: &api.ExecutionStart{InputCount: proto.Int64(0), Harness: "gate"}},
		{ExecutionID: id, Kind: api.EventToolCall, ToolCall: &api.ToolCall{ID: "call", IdempotencyKey: "key", Mediation: api.MediationRequiresApproval}},
		{ExecutionID: id, Kind: api.EventApprovalRequest, Approval: &api.ApprovalRequest{ToolCallID: "call"}},
	}
}

// Literal tuples pin each prefix without importing the later strict controller oracle.
func TestApprovalPrefixes(t *testing.T) {
	b := approvalEvents("e")
	s, c, r := b[0], b[1], b[2]
	d := api.Event{ExecutionID: "e", Kind: api.EventApprovalResult, ApprovalResult: &api.ApprovalResult{ToolCallID: "call", RequestSeq: 3, Approved: true}}
	receipt := api.Event{ExecutionID: "e", Kind: api.EventToolResult, Result: &api.ToolResult{ID: "call", ApprovalRequestSeq: 3, ApprovalDecisionSeq: 4}}
	fork := api.Event{Kind: api.EventLifecycle, Lifecycle: &api.Lifecycle{Kind: api.LifecycleFork}}
	other := approvalEvents("latest")
	none, pending := api.ApprovalRef{}, api.ApprovalRef{ExecutionID: "e", ToolCallID: "call", RequestSeq: 3}
	latest := api.ApprovalRef{ExecutionID: "latest", ToolCallID: "call", RequestSeq: 6}
	second := api.Event{ExecutionID: "e", Kind: api.EventToolCall, ToolCall: &api.ToolCall{ID: "second", IdempotencyKey: "second-key", Mediation: api.MediationRequiresApproval}}
	secondRequest := api.Event{ExecutionID: "e", Kind: api.EventApprovalRequest, Approval: &api.ApprovalRequest{ToolCallID: "second"}}
	for _, tc := range []struct {
		name   string
		events []api.Event
		want   []api.ApprovalRef
	}{
		{"decision receipt second call", []api.Event{s, c, r, d, receipt, second, secondRequest}, []api.ApprovalRef{none, none, pending, none, none, none, {ExecutionID: "e", ToolCallID: "second", RequestSeq: 7}}},
		{"duplicate request", []api.Event{s, c, r, r}, []api.ApprovalRef{none, none, pending, none}},
		{"duplicate decision", []api.Event{s, c, r, d, d}, []api.ApprovalRef{none, none, pending, none, none}},
		{"duplicate gated call", []api.Event{s, c, r, d, receipt, c, r}, []api.ApprovalRef{none, none, pending, none, none, none, none}},
		{"unbound receipt poisons later gate", []api.Event{receipt, s, c, r}, []api.ApprovalRef{none, none, none, none}},
		{"denial contradictory receipt", []api.Event{s, c, r, {ExecutionID: "e", Kind: api.EventApprovalResult, ApprovalResult: &api.ApprovalResult{ToolCallID: "call", RequestSeq: 3}}, receipt, second, secondRequest}, []api.ApprovalRef{none, none, pending, none, none, none, none}},
		{"bad receipt sequence", []api.Event{s, c, r, d, {ExecutionID: "e", Kind: api.EventToolResult, Result: &api.ToolResult{ID: "call", ApprovalRequestSeq: 2, ApprovalDecisionSeq: 4}}, second, secondRequest}, []api.ApprovalRef{none, none, pending, none, none, none, none}},
		{"missing input body", []api.Event{{ExecutionID: "e", Kind: api.EventExecutionStart, ExecutionStart: &api.ExecutionStart{InputCount: proto.Int64(1)}}, {ExecutionID: "e", Kind: api.EventInput}, c, r}, []api.ApprovalRef{none, none, none, none}},
		{"missing input count", []api.Event{{ExecutionID: "e", Kind: api.EventExecutionStart, ExecutionStart: &api.ExecutionStart{}}, c, r}, []api.ApprovalRef{none, none, none}},
		{"negative input count", []api.Event{{ExecutionID: "e", Kind: api.EventExecutionStart, ExecutionStart: &api.ExecutionStart{InputCount: proto.Int64(-1)}}, c, r}, []api.ApprovalRef{none, none, none}},
		{"duplicate start", []api.Event{s, s, c, r}, []api.ApprovalRef{none, none, none, none}},
		{"missing call body", []api.Event{s, {ExecutionID: "e", Kind: api.EventToolCall}, c, r}, []api.ApprovalRef{none, none, none, none}},
		{"nongated malformed compatible then gated suppresses", []api.Event{{ExecutionID: "old", Kind: api.EventExecutionStart}, s, c, r, {ExecutionID: "old", Kind: api.EventApprovalRequest}}, []api.ApprovalRef{none, none, none, {ExecutionID: "e", ToolCallID: "call", RequestSeq: 4}, none}},
		{"latest is first seen not last touched", []api.Event{s, c, r, other[0], other[1], other[2], {ExecutionID: "e", Kind: api.EventError, Err: &api.Error{Description: "audit"}}}, []api.ApprovalRef{none, none, pending, none, none, latest, latest}},
		{"fork excludes inherited even if touched", []api.Event{s, c, r, fork, {ExecutionID: "e", Kind: api.EventError}, other[0], other[1], other[2]}, []api.ApprovalRef{none, none, pending, none, none, none, none, {ExecutionID: "latest", ToolCallID: "call", RequestSeq: 8}}},
		{"first seen before fork with late call", []api.Event{s, fork, c, r}, []api.ApprovalRef{none, none, none, none}},
		{"latest nested fork excludes earlier turn", []api.Event{fork, s, c, r, fork, second, secondRequest}, []api.ApprovalRef{none, none, none, {ExecutionID: "e", ToolCallID: "call", RequestSeq: 4}, none, none, none}},
		{"error and suspend preserve pending", []api.Event{s, c, r, {ExecutionID: "e", Kind: api.EventError, Err: &api.Error{Description: "audit"}}, {Kind: api.EventLifecycle, Lifecycle: &api.Lifecycle{Kind: api.LifecycleSuspend}}}, []api.ApprovalRef{none, none, pending, pending, pending}},
		{"end suppresses pending", []api.Event{s, c, r, {ExecutionID: "e", Kind: api.EventEnd, End: &api.HarnessEnd{State: "COMPLETED"}}}, []api.ApprovalRef{none, none, pending, none}},
		{"legacy own first seen", []api.Event{c, r}, []api.ApprovalRef{none, {ExecutionID: "e", ToolCallID: "call", RequestSeq: 2}}},
		{"completed latest hides older pending", []api.Event{s, c, r, other[0], {ExecutionID: "latest", Kind: api.EventEnd, End: &api.HarnessEnd{State: "COMPLETED"}}}, []api.ApprovalRef{none, none, pending, none, none}},
		{"late input repairs old incomplete gate", []api.Event{{ExecutionID: "e", Kind: api.EventExecutionStart, ExecutionStart: &api.ExecutionStart{InputCount: proto.Int64(1)}}, c, r, d, receipt, other[0], other[1], other[2], {ExecutionID: "e", Kind: api.EventInput, Message: api.TextMessage("user", "late")}}, []api.ApprovalRef{none, none, none, none, none, none, none, none, {ExecutionID: "latest", ToolCallID: "call", RequestSeq: 8}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := approvalOpen(t, ":memory:")
			for i, ev := range tc.events {
				approvalAppend(t, store.Session("s"), int64(i), 0, ev)
				info := approvalInfo(t, store, "s")
				if info.LastSeq != int64(i+1) || info.PendingApproval != tc.want[i] {
					t.Fatalf("prefix %d: cursor=%d ref=%+v want=%+v", i+1, info.LastSeq, info.PendingApproval, tc.want[i])
				}
			}
		})
	}
}

func TestApprovalPersistedNormalization(t *testing.T) {
	b := approvalEvents("e")
	message := api.TextMessage("assistant", "selected wire body")
	for _, tc := range []struct {
		name          string
		seq           int
		ev, persisted api.Event
		want          api.ApprovalRef
	}{
		{"request body discarded", 3, api.Event{ExecutionID: "e", Kind: api.EventApprovalRequest, Message: message, Approval: b[2].Approval}, api.Event{ExecutionID: "e", Kind: api.EventApprovalRequest, Message: message}, api.ApprovalRef{}},
		{"start body discarded", 1, api.Event{ExecutionID: "e", Kind: api.EventExecutionStart, Message: message, ExecutionStart: b[0].ExecutionStart}, api.Event{ExecutionID: "e", Kind: api.EventExecutionStart, Message: message}, api.ApprovalRef{}},
		{"call body discarded", 2, api.Event{ExecutionID: "e", Kind: api.EventToolCall, Message: message, ToolCall: b[1].ToolCall}, api.Event{ExecutionID: "e", Kind: api.EventToolCall, Message: message}, api.ApprovalRef{}},
		{"fork body discarded", 4, api.Event{Kind: api.EventLifecycle, Message: message, Lifecycle: &api.Lifecycle{Kind: api.LifecycleFork}}, api.Event{Kind: api.EventLifecycle, Message: message}, api.ApprovalRef{ExecutionID: "e", ToolCallID: "call", RequestSeq: 3}},
		{"unknown event enum", 4, api.Event{ExecutionID: "e", Kind: api.EventKind("EVENT_ERROR"), Err: &api.Error{Description: "unknown"}}, api.Event{ExecutionID: "e", Err: &api.Error{Description: "unknown"}}, api.ApprovalRef{}},
		{"unknown mediation enum", 2, api.Event{ExecutionID: "e", Kind: api.EventToolCall, ToolCall: &api.ToolCall{ID: "ordinary", Mediation: api.Mediation("MEDIATION_REQUIRES_APPROVAL")}}, api.Event{ExecutionID: "e", Kind: api.EventToolCall, ToolCall: &api.ToolCall{ID: "ordinary"}}, api.ApprovalRef{ExecutionID: "e", ToolCallID: "call", RequestSeq: 4}},
		{"unknown lifecycle enum", 4, api.Event{Kind: api.EventLifecycle, Lifecycle: &api.Lifecycle{Kind: api.LifecycleKind("LIFECYCLE_FORK")}}, api.Event{Kind: api.EventLifecycle, Lifecycle: &api.Lifecycle{}}, api.ApprovalRef{ExecutionID: "e", ToolCallID: "call", RequestSeq: 3}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := approvalOpen(t, ":memory:")
			events := append([]api.Event{}, b...)
			if tc.name == "unknown mediation enum" {
				events = []api.Event{b[0], tc.ev, b[1], b[2]}
			} else if tc.seq == 4 {
				events = append(events, tc.ev)
			} else {
				events[tc.seq-1] = tc.ev
			}
			for i, ev := range events {
				approvalAppend(t, store.Session("s"), int64(i), 0, ev)
			}
			records, err := store.Session("s").Read(int64(tc.seq))
			if err != nil || len(records) == 0 || !reflect.DeepEqual(records[0].Event, tc.persisted) {
				t.Fatalf("persisted=%+v err=%v want=%+v", records, err, tc.persisted)
			}
			if err := store.Session("s").Verify(); err != nil {
				t.Fatal(err)
			}
			if got := approvalInfo(t, store, "s").PendingApproval; got != tc.want {
				t.Fatalf("ref=%+v want=%+v", got, tc.want)
			}
		})
	}
}

func TestApprovalComputeAndAtomicity(t *testing.T) {
	ordinary := []api.Event{
		{Kind: api.EventExecutionStart, ExecutionStart: &api.ExecutionStart{}}, {Kind: api.EventInput, Message: api.TextMessage("user", "hi")},
		{Kind: api.EventModelCall, ModelCall: &api.ModelCall{ID: "model"}}, {Kind: api.EventOutput, Message: api.TextMessage("assistant", "hi")},
		{Kind: api.EventToolCall, ToolCall: &api.ToolCall{ID: "call", Mediation: api.MediationControllerMediated}}, {Kind: api.EventToolResult, Result: &api.ToolResult{ID: "call"}},
		{Kind: api.EventUsage, Usage: &api.Usage{}}, {Kind: api.EventError, Err: &api.Error{Description: "error"}}, {Kind: api.EventEnd, End: &api.HarnessEnd{State: "COMPLETED"}},
	}
	for _, ev := range append(ordinary, approvalEvents("e")[2], api.Event{Kind: api.EventApprovalResult, ApprovalResult: &api.ApprovalResult{ToolCallID: "call", RequestSeq: 1}}) {
		t.Run(string(ev.Kind), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "compute.db")
			store := approvalOpen(t, path)
			approvalAppend(t, store.Session("s"), 0, 0, api.Event{Kind: api.EventLifecycle, Lifecycle: &api.Lifecycle{Kind: api.LifecycleSuspend}})
			approvalAppend(t, store.Session("s"), 1, 0, ev)
			want := api.ComputeLive
			if ev.Kind == api.EventApprovalRequest || ev.Kind == api.EventApprovalResult {
				want = api.ComputeCold
			}
			store.Close()
			info := approvalInfo(t, approvalOpen(t, path), "s")
			if info.LastSeq != 2 || info.ComputeState != want {
				t.Fatalf("info=%+v want compute=%s", info, want)
			}
		})
	}
	store := approvalOpen(t, ":memory:")
	log := store.Session("s")
	fence, err := log.NewFence()
	if err != nil {
		t.Fatal(err)
	}
	for i, ev := range approvalEvents("e") {
		approvalAppend(t, log, int64(i), fence, ev)
	}
	if err := store.PutSession(SessionMeta{UID: "s", Project: "custom", Name: "renamed", ParentUID: "parent", ForkSeq: 99}); err != nil {
		t.Fatal(err)
	}
	before := approvalInfo(t, store, "s")
	if before.LastSeq != 3 || before.PendingApproval != (api.ApprovalRef{ExecutionID: "e", ToolCallID: "call", RequestSeq: 3}) || before.Project != "custom" || before.Name != "renamed" {
		t.Fatalf("metadata clobbered projection: %+v", before)
	}
	page, err := store.ListSessions(ListOptions{Project: "custom"})
	if err != nil || len(page.Sessions) != 1 || !reflect.DeepEqual(page.Sessions[0], before) {
		t.Fatalf("listing=%+v err=%v", page, err)
	}
	journal, metadata := approvalStored(t, store.db)
	decision := api.Event{ExecutionID: "e", Kind: api.EventApprovalResult, ApprovalResult: &api.ApprovalResult{ToolCallID: "call", RequestSeq: 3, Approved: true}}
	approvalExec(t, store.db, `CREATE TRIGGER fail_projection BEFORE UPDATE ON approval_executions BEGIN SELECT RAISE(ABORT,'projection fault'); END`)
	for _, tc := range []struct {
		name       string
		seq, fence int64
		want       error
	}{{"CAS", 2, fence, eventlog.ErrConflict}, {"fence", 3, fence - 1, eventlog.ErrFenced}, {"SQL", 3, fence, nil}} {
		_, err := log.Append(tc.seq, tc.fence, decision)
		if tc.want != nil && !errors.Is(err, tc.want) || tc.want == nil && (err == nil || !strings.Contains(err.Error(), "projection fault")) {
			t.Fatalf("%s err=%v want=%v", tc.name, err, tc.want)
		}
		if after := approvalInfo(t, store, "s"); !reflect.DeepEqual(before, after) {
			t.Fatalf("%s partial metadata=%+v", tc.name, after)
		}
		if gotJournal, gotMetadata := approvalStored(t, store.db); gotJournal != journal || gotMetadata != metadata {
			t.Fatalf("%s changed journal bytes or metadata", tc.name)
		}
	}
	approvalExec(t, store.db, `DROP TRIGGER fail_projection`)
	approvalAppend(t, log, 3, fence, decision)
	after := approvalInfo(t, store, "s")
	if after.LastSeq != 4 || after.PendingApproval != (api.ApprovalRef{}) || after.Project != before.Project || after.CreatedAt != before.CreatedAt || before.PendingApproval.RequestSeq != 3 {
		t.Fatalf("retry/snapshot=%+v before=%+v", after, before)
	}
}

func TestApprovalMigration(t *testing.T) {
	for _, mode := range []string{"released-v1", "v2-corrupt", "v2-rollback"} {
		t.Run(mode, func(t *testing.T) {
			path, raw := approvalFixture(t, mode == "released-v1")
			if mode == "v2-corrupt" {
				approvalExec(t, raw, `UPDATE events SET event=x'ff' WHERE session='bad' AND seq=2`)
			}
			journal, metadata := approvalStored(t, raw)
			if mode == "v2-rollback" {
				approvalExec(t, raw, `CREATE TRIGGER fail_backfill BEFORE UPDATE ON sessions BEGIN SELECT RAISE(ABORT,'backfill fault'); END`)
				if store, err := Open(path); err == nil {
					store.Close()
					t.Fatal("SQL backfill fault accepted")
				} else if !strings.Contains(err.Error(), "backfill fault") {
					t.Fatal(err)
				}
				var stamp, objects int
				if err := raw.QueryRow(`SELECT user_version, (SELECT COUNT(*) FROM pragma_table_info('sessions') WHERE name='last_seq') + (SELECT COUNT(*) FROM sqlite_master WHERE name IN ('approval_executions','approval_calls')) FROM pragma_user_version`).Scan(&stamp, &objects); err != nil || stamp != 2 || objects != 0 {
					t.Fatalf("half migration stamp=%d objects=%d err=%v", stamp, objects, err)
				}
				approvalExec(t, raw, `DROP TRIGGER fail_backfill`)
			}
			store := approvalOpen(t, path)
			gotJournal, gotMetadata := approvalStored(t, raw)
			if journal != gotJournal || metadata != gotMetadata {
				t.Fatal("migration changed journal bytes/hashes or old metadata")
			}
			var stamp int
			if err := raw.QueryRow(`PRAGMA user_version`).Scan(&stamp); err != nil || stamp != 3 {
				t.Fatalf("stamp=%d err=%v", stamp, err)
			}
			uids := []string{"sess-43c1f32622cd7ac118bed408", "sess-78ec0aced8e04aa38d6eac9f"}
			if mode != "released-v1" {
				uids = []string{"healthy"}
			}
			for reopen := 0; reopen < 2; reopen++ {
				for _, uid := range uids {
					info := approvalInfo(t, store, uid)
					wantRef := api.ApprovalRef{}
					if mode != "released-v1" && reopen == 0 {
						wantRef = api.ApprovalRef{ExecutionID: "e", ToolCallID: "call", RequestSeq: 3}
					}
					if info.PendingApproval != wantRef || mode == "released-v1" && (info.LastSeq == 0 || info.Harness != "echo") || mode != "released-v1" && (info.LastSeq != int64(3+reopen) || info.CreatedAt.UnixNano() != 10) {
						t.Fatalf("backfill/reopen=%+v want=%+v", info, wantRef)
					}
					if err := store.Session(uid).Verify(); err != nil {
						t.Fatal(err)
					}
				}
				if mode == "v2-corrupt" {
					if info := approvalInfo(t, store, "bad"); info.LastSeq != int64(3+3*reopen) || info.PendingApproval != (api.ApprovalRef{}) {
						t.Fatalf("corrupt regained authority=%+v", info)
					}
					if _, err := store.Session("bad").Read(1); err == nil {
						t.Fatal("corrupt bytes repaired")
					}
					if reopen == 0 {
						for i, ev := range approvalEvents("later") {
							approvalAppend(t, store.Session("bad"), int64(3+i), 0, ev)
						}
					}
				}
				if reopen == 0 {
					store.Close()
					store = approvalOpen(t, path)
					if mode != "released-v1" {
						approvalAppend(t, store.Session("healthy"), 3, 0, api.Event{ExecutionID: "e", Kind: api.EventApprovalResult, ApprovalResult: &api.ApprovalResult{ToolCallID: "call", RequestSeq: 3, Approved: true}})
					}
				}
			}
			if mode == "released-v1" {
				if got, meta := approvalStored(t, raw); got != journal || meta != metadata {
					t.Fatal("reopen changed released fixture")
				}
			}
		})
	}
}

func approvalOpen(t *testing.T, path string) *Store {
	t.Helper()
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}
func approvalInfo(t *testing.T, store *Store, uid string) SessionInfo {
	t.Helper()
	info, err := store.SessionInfo(uid)
	if err != nil {
		t.Fatal(err)
	}
	return info
}
func approvalAppend(t *testing.T, log *Log, seq, fence int64, ev api.Event) {
	t.Helper()
	if _, err := log.Append(seq, fence, ev); err != nil {
		t.Fatalf("append %d: %v", seq+1, err)
	}
}
func approvalExec(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatal(err)
	}
}

// Compare every persisted event byte/link/fence and every pre-v3 metadata column.
func approvalStored(t *testing.T, db *sql.DB) (journal, metadata string) {
	t.Helper()
	if err := db.QueryRow(`SELECT group_concat(value) FROM (SELECT json_array(session,seq,prev_hash,hash,fence,hex(event)) value FROM events ORDER BY session,seq)`).Scan(&journal); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT group_concat(value) FROM (SELECT json_array(session,fence,project,name,harness,model,parent_uid,fork_seq,compute_state,created_at,updated_at,labels,annotations,origin,identity) value FROM sessions ORDER BY session)`).Scan(&metadata); err != nil {
		t.Fatal(err)
	}
	return journal, metadata
}
func approvalFixture(t *testing.T, released bool) (string, *sql.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "old.db")
	blob, err := os.ReadFile("testdata/v0.1.2.db")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, blob, 0600); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if !released {
		approvalExec(t, db, v1ToV2)
		approvalExec(t, db, `PRAGMA user_version=2`)
		for _, uid := range []string{"healthy", "bad"} {
			approvalExec(t, db, `INSERT INTO sessions(session,project,harness,compute_state,created_at,updated_at) VALUES(?,'default','gate','COLD',10,20)`, uid)
			prev := ""
			for i, ev := range approvalEvents("e") {
				seq := int64(i + 1)
				hash, err := canon.HashRecord(prev, seq, ev)
				if err != nil {
					t.Fatal(err)
				}
				blob, err := proto.Marshal(wire.EventToProto(ev))
				if err != nil {
					t.Fatal(err)
				}
				approvalExec(t, db, `INSERT INTO events(session,seq,prev_hash,hash,fence,event) VALUES(?,?,?,?,0,?)`, uid, seq, prev, hash, blob)
				prev = hash
			}
		}
	}
	return path, db
}
