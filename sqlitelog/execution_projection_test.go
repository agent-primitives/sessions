package sqlitelog

import (
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/agent-primitives/sessions/api"
	"github.com/agent-primitives/sessions/eventlog"
	"google.golang.org/protobuf/proto"
)

func TestExecutionProjectionPrefixesAndMigration(t *testing.T) {
	start := api.Event{ExecutionID: "e", Kind: api.EventExecutionStart, ExecutionStart: &api.ExecutionStart{InputCount: proto.Int64(0)}}
	output := api.Event{ExecutionID: "e", Kind: api.EventOutput, Message: api.TextMessage("assistant", "result")}
	end := api.Event{ExecutionID: "e", Kind: api.EventEnd, End: &api.HarnessEnd{State: "COMPLETED"}}
	failure := api.Event{ExecutionID: "e", Kind: api.EventError, Err: &api.Error{Description: "failed"}}
	fork := api.Event{Kind: api.EventLifecycle, Lifecycle: &api.Lifecycle{Kind: api.LifecycleFork}}
	suspend := api.Event{Kind: api.EventLifecycle, Lifecycle: &api.Lifecycle{Kind: api.LifecycleSuspend}}
	resume := api.Event{Kind: api.EventLifecycle, Lifecycle: &api.Lifecycle{Kind: api.LifecycleResume}}
	gate := approvalEvents("e")
	decision := api.Event{ExecutionID: "e", Kind: api.EventApprovalResult, ApprovalResult: &api.ApprovalResult{ToolCallID: "call", RequestSeq: 3, Approved: true}}
	receipt := api.Event{ExecutionID: "e", Kind: api.EventToolResult, Result: &api.ToolResult{ID: "call", ApprovalRequestSeq: 3, ApprovalDecisionSeq: 5}}
	legacyInput := api.Event{Kind: api.EventInput, Message: api.TextMessage("user", "legacy")}
	legacyEnd, legacyError := end, failure
	legacyEnd.ExecutionID, legacyError.ExecutionID = "", ""
	newStart := start
	newStart.ExecutionID = "new"
	for _, tc := range []struct {
		name   string
		events []api.Event
		want   []api.ExecState
	}{
		{"empty", nil, nil},
		{"lifecycle only", []api.Event{fork, suspend, resume}, []api.ExecState{api.ExecPending, api.ExecPending, api.ExecPending}},
		{"successful", []api.Event{start, output, end, suspend, resume}, []api.ExecState{api.ExecRunning, api.ExecRunning, api.ExecCompleted, api.ExecCompleted, api.ExecCompleted}},
		{"failed then recovered", []api.Event{start, failure, suspend, resume, output, end}, []api.ExecState{api.ExecRunning, api.ExecFailed, api.ExecFailed, api.ExecFailed, api.ExecRunning, api.ExecCompleted}},
		{"latest first seen", []api.Event{start, end, newStart, failure}, []api.ExecState{api.ExecRunning, api.ExecCompleted, api.ExecRunning, api.ExecRunning}},
		{"approval", []api.Event{gate[0], gate[1], gate[2], suspend, decision, receipt, end}, []api.ExecState{api.ExecRunning, api.ExecRunning, api.ExecAwaiting, api.ExecAwaiting, api.ExecRunning, api.ExecRunning, api.ExecCompleted}},
		{"pending survives error", []api.Event{gate[0], gate[1], gate[2], failure}, []api.ExecState{api.ExecRunning, api.ExecRunning, api.ExecAwaiting, api.ExecAwaiting}},
		{"fork mid execution", []api.Event{start, fork}, []api.ExecState{api.ExecRunning, api.ExecRunning}},
		{"fork after completion", []api.Event{start, end, fork}, []api.ExecState{api.ExecRunning, api.ExecCompleted, api.ExecCompleted}},
		{"inherited approval", []api.Event{gate[0], gate[1], gate[2], fork}, []api.ExecState{api.ExecRunning, api.ExecRunning, api.ExecAwaiting, api.ExecRunning}},
		{"legacy turns", []api.Event{legacyInput, legacyEnd, legacyInput, legacyError, newStart}, []api.ExecState{api.ExecRunning, api.ExecCompleted, api.ExecRunning, api.ExecFailed, api.ExecRunning}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := approvalOpen(t, ":memory:")
			if err := store.PutSession(SessionMeta{UID: "s"}); err != nil {
				t.Fatal(err)
			}
			assertExecutionProjection(t, store, "s", api.ExecPending)
			for i, ev := range tc.events {
				approvalAppend(t, store.Session("s"), int64(i), 0, ev)
				assertExecutionProjection(t, store, "s", tc.want[i])
			}
			before := approvalInfo(t, store, "s")
			deleted, err := store.DeleteSession(t.Context(), "s")
			if err != nil || deleted.ExecState != before.ExecState {
				t.Fatalf("delete state=%s err=%v, want %s", deleted.ExecState, err, before.ExecState)
			}

			path, raw := executionV6Fixture(t)
			for _, ev := range tc.events {
				appendV6Execution(t, raw, "s", ev)
			}
			journal, metadata := approvalStored(t, raw)
			migrated := approvalOpen(t, path)
			want := api.ExecPending
			if len(tc.want) > 0 {
				want = tc.want[len(tc.want)-1]
			}
			assertExecutionProjection(t, migrated, "s", want)
			if gotJournal, gotMetadata := approvalStored(t, raw); gotJournal != journal || gotMetadata != metadata {
				t.Fatal("v7 migration rewrote events, hashes, fences or original metadata")
			}
			migrated.Close()
			assertExecutionProjection(t, approvalOpen(t, path), "s", want)
		})
	}
}

func assertExecutionProjection(t *testing.T, store *Store, uid string, want api.ExecState) {
	t.Helper()
	info := approvalInfo(t, store, uid)
	records, err := store.Session(uid).Read(1)
	if err != nil {
		t.Fatal(err)
	}
	folded := foldExecutionState(records)
	if info.ExecState != want || info.ExecState != folded {
		t.Fatalf("cursor %d: state=%s fold=%s want=%s", info.LastSeq, info.ExecState, folded, want)
	}
	page, err := store.ListSessions(ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, listed := range page.Sessions {
		if listed.UID == uid {
			if !reflect.DeepEqual(listed, info) {
				t.Fatalf("Get/List differ: %+v != %+v", info, listed)
			}
			return
		}
	}
	t.Fatal("session absent from list")
}

// This read-time oracle folds whole executions, independently of the incremental SQL reducer.
// Its inputs are the well-formed histories above; invalid evidence is exercised separately.
func foldExecutionState(records []eventlog.Record) api.ExecState {
	var latest string
	byID := make(map[string][]eventlog.Record)
	var forkSeq int64
	for _, record := range records {
		ev := record.Event
		if ev.Kind == api.EventLifecycle {
			if ev.Lifecycle.Kind == api.LifecycleFork {
				forkSeq = record.Seq
			}
			continue
		}
		if _, seen := byID[ev.ExecutionID]; !seen {
			latest = ev.ExecutionID
		}
		byID[ev.ExecutionID] = append(byID[ev.ExecutionID], record)
	}
	execution := byID[latest]
	if len(execution) == 0 {
		return api.ExecPending
	}
	var callSeq, requestSeq int64
	for _, record := range execution {
		switch record.Event.Kind {
		case api.EventToolCall:
			callSeq = record.Seq
		case api.EventApprovalRequest:
			requestSeq = record.Seq
		case api.EventApprovalResult, api.EventEnd:
			requestSeq = 0
		}
	}
	if requestSeq > 0 && callSeq > forkSeq {
		return api.ExecAwaiting
	}
	switch execution[len(execution)-1].Event.Kind {
	case api.EventEnd:
		return api.ExecCompleted
	case api.EventError:
		return api.ExecFailed
	default:
		return api.ExecRunning
	}
}

func TestExecutionProjectionRefusesAmbiguousEvidence(t *testing.T) {
	for _, tc := range []struct {
		name   string
		events []api.Event
	}{
		{"missing end body", []api.Event{{ExecutionID: "e", Kind: api.EventEnd}}},
		{"missing error body", []api.Event{{ExecutionID: "e", Kind: api.EventError}}},
		{"incomplete invocation", []api.Event{
			{ExecutionID: "e", Kind: api.EventExecutionStart, ExecutionStart: &api.ExecutionStart{InputCount: proto.Int64(1)}},
			{ExecutionID: "e", Kind: api.EventEnd, End: &api.HarnessEnd{State: "COMPLETED"}},
		}},
		{"unknown event", []api.Event{{ExecutionID: "e", Kind: api.EventKind("unknown")}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := approvalOpen(t, ":memory:")
			for i, ev := range tc.events {
				approvalAppend(t, store.Session("s"), int64(i), 0, ev)
			}
			if got := approvalInfo(t, store, "s").ExecState; got != api.ExecRunning {
				t.Fatalf("ambiguous evidence reports %s", got)
			}
		})
	}
}

func TestExecutionProjectionAppendRollback(t *testing.T) {
	store := approvalOpen(t, ":memory:")
	log := store.Session("s")
	fence, err := log.NewFence()
	if err != nil {
		t.Fatal(err)
	}
	approvalAppend(t, log, 0, fence, api.Event{ExecutionID: "e", Kind: api.EventError, Err: &api.Error{Description: "failed"}})
	before := approvalInfo(t, store, "s")
	approvalExec(t, store.db, `CREATE TRIGGER fail_outcome BEFORE UPDATE OF outcome ON approval_executions BEGIN SELECT RAISE(ABORT,'outcome fault'); END`)
	end := api.Event{ExecutionID: "e", Kind: api.EventEnd, End: &api.HarnessEnd{State: "COMPLETED"}}
	for _, tc := range []struct {
		seq, fence int64
		want       error
	}{{0, fence, eventlog.ErrConflict}, {1, fence - 1, eventlog.ErrFenced}, {1, fence, nil}} {
		_, err := log.Append(tc.seq, tc.fence, end)
		if tc.want != nil && !errors.Is(err, tc.want) || tc.want == nil && (err == nil || !strings.Contains(err.Error(), "outcome fault")) {
			t.Fatalf("append error=%v, want %v", err, tc.want)
		}
		if got := approvalInfo(t, store, "s"); got != before {
			t.Fatalf("failed append changed snapshot: %+v", got)
		}
	}
	approvalExec(t, store.db, `DROP TRIGGER fail_outcome`)
	approvalAppend(t, log, 1, fence, end)
	assertExecutionProjection(t, store, "s", api.ExecCompleted)
}

func TestExecutionProjectionStaleWriter(t *testing.T) {
	for _, kind := range []api.EventKind{api.EventEnd, api.EventError} {
		t.Run(string(kind), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "mixed.db")
			store := approvalOpen(t, path)
			log := store.Session("s")
			ev := api.Event{ExecutionID: "e", Kind: kind}
			if kind == api.EventEnd {
				ev.End = &api.HarnessEnd{State: "COMPLETED"}
			} else {
				ev.Err = &api.Error{Description: "failed"}
			}
			approvalAppend(t, log, 0, 0, ev)
			before := approvalInfo(t, store, "s")
			// Simulate an already-open pre-projection writer without touching the summary.
			tx, err := store.db.Begin()
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := appendRecord(t.Context(), tx, "s", 1, 0, api.Event{
				ExecutionID: "new", Kind: api.EventOutput, Message: api.TextMessage("assistant", "progress"),
			}); err != nil {
				tx.Rollback()
				t.Fatal(err)
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			for phase := 0; phase < 3; phase++ {
				info := approvalInfo(t, store, "s")
				if info.ExecState != api.ExecRunning {
					t.Fatalf("phase %d: stale projection reports %s", phase, info.ExecState)
				}
				page, err := store.ListSessions(ListOptions{})
				if err != nil || len(page.Sessions) != 1 || page.Sessions[0] != info {
					t.Fatalf("stale Get/List differ: %+v %v", page, err)
				}
				switch phase {
				case 0:
					approvalAppend(t, store.Session("s"), 2, 0, ev)
				case 1:
					store.Close()
					store = approvalOpen(t, path)
				}
			}
			deleted, err := store.DeleteSession(t.Context(), "s")
			if err != nil || deleted.ExecState != api.ExecRunning {
				t.Fatalf("deletion restored stale outcome: %+v %v", deleted, err)
			}
			if before.ExecState == api.ExecRunning || before.LastSeq != 1 {
				t.Fatal("later appends changed captured snapshot")
			}
		})
	}
}

func executionV6Fixture(t *testing.T) (string, *sql.DB) {
	t.Helper()
	path, raw := approvalFixture(t, false)
	tx, err := raw.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for _, migrate := range []func(*sql.Tx) error{migrateV2ToV3, migrateV3ToV4, migrateV4ToV5, migrateV5ToV6} {
		if err := migrate(tx); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := tx.Exec(`INSERT INTO sessions(session,project) VALUES('s','default'); PRAGMA user_version=6;`); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return path, raw
}

func appendV6Execution(t *testing.T, db *sql.DB, uid string, ev api.Event) {
	t.Helper()
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	var head int64
	if err := tx.QueryRow(`SELECT COALESCE(MAX(seq),0) FROM events WHERE session=?`, uid).Scan(&head); err != nil {
		t.Fatal(err)
	}
	record, _, err := appendRecord(t.Context(), tx, uid, head, 0, ev)
	if err != nil {
		t.Fatal(err)
	}
	if err := touchSession(tx, uid, DefaultProject, ev); err != nil {
		t.Fatal(err)
	}
	if err := projectApproval(tx, uid, record.Seq, ev); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func TestExecutionMigrationReadOnlyAndRollback(t *testing.T) {
	path, raw := executionV6Fixture(t)
	journal, metadata := approvalStored(t, raw)
	for _, mode := range []string{"read-only", "SQL fault"} {
		t.Run(mode, func(t *testing.T) {
			dsn := path
			if mode == "read-only" {
				dsn = "file:" + filepath.ToSlash(path) + "?mode=ro"
			} else {
				approvalExec(t, raw, `CREATE TRIGGER fail_v7 BEFORE DELETE ON approval_executions BEGIN SELECT RAISE(ABORT,'v7 fault'); END`)
			}

			store, err := Open(dsn)
			if err == nil {
				store.Close()
				t.Fatal("migration unexpectedly succeeded")
			}
			if !strings.Contains(err.Error(), "migrate") {
				t.Fatalf("unclear migration failure: %v", err)
			}
			var stamp, columns int
			if err := raw.QueryRow(`SELECT user_version,(SELECT COUNT(*) FROM pragma_table_info('approval_executions') WHERE name='outcome') FROM pragma_user_version`).Scan(&stamp, &columns); err != nil || stamp != 6 || columns != 0 {
				t.Fatalf("half migration: version=%d columns=%d err=%v", stamp, columns, err)
			}
			if gotJournal, gotMetadata := approvalStored(t, raw); gotJournal != journal || gotMetadata != metadata {
				t.Fatal("failed migration changed persistent evidence")
			}
		})
	}
	approvalExec(t, raw, `DROP TRIGGER fail_v7`)
	store := approvalOpen(t, path)
	assertExecutionProjection(t, store, "s", api.ExecPending)
	store.Close()
	readOnly := approvalOpen(t, "file:"+filepath.ToSlash(path)+"?mode=ro")
	assertExecutionProjection(t, readOnly, "s", api.ExecPending)
}

func TestExecutionMigrationUndecodableSession(t *testing.T) {
	path, raw := executionV6Fixture(t)
	approvalExec(t, raw, `UPDATE events SET event=x'ff' WHERE session='bad' AND seq=2`)
	store := approvalOpen(t, path)
	if got := approvalInfo(t, store, "bad").ExecState; got != api.ExecRunning {
		t.Fatalf("undecodable history reports %s", got)
	}
	if got := approvalInfo(t, store, "healthy").ExecState; got != api.ExecAwaiting {
		t.Fatalf("corrupt neighbor affected healthy session: %s", got)
	}
	if _, err := store.Session("bad").Read(1); err == nil {
		t.Fatal("migration repaired corrupt bytes")
	}
}
