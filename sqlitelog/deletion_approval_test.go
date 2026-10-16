package sqlitelog

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/agent-primitives/sessions/api"
	"github.com/agent-primitives/sessions/controller"
	"github.com/agent-primitives/sessions/eventlog"
)

// A bypass DELETE writer must advance the captured cursor, without reducing approval
// execution evidence or touching compute; even historical decision retries lose authority.
func TestDeletionPreservesApprovalProjectionAndRejectsDecisions(t *testing.T) {
	for _, shape := range []string{"empty", "parked", "approved", "lagging"} {
		t.Run(shape, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "session.db")
			s := deletionStore(t, path)
			if err := s.PutSession(SessionMeta{UID: "s"}); err != nil {
				t.Fatal(err)
			}
			log := s.Session("s")
			decision := api.ApprovalDecision{ExecutionID: "execution", ToolCallID: "call", RequestSeq: 3, Approved: true}
			if shape != "empty" {
				for i, ev := range approvalEvents("execution") {
					if _, err := log.Append(int64(i), 0, ev); err != nil {
						t.Fatal(err)
					}
				}
				if _, err := log.Append(3, 0, api.Event{Kind: api.EventLifecycle, Lifecycle: &api.Lifecycle{Kind: api.LifecycleSuspend}}); err != nil {
					t.Fatal(err)
				}
				if shape == "approved" {
					if _, err := controller.Approve(log, decision); err != nil {
						t.Fatal(err)
					}
				}
			}
			if shape == "lagging" {
				// The last SUSPEND was appended by a writer that skipped the projection.
				if _, err := s.db.Exec(`UPDATE sessions SET last_seq=3 WHERE session='s'`); err != nil {
					t.Fatal(err)
				}
				if info := deletionInfo(t, s, "s"); info.LastSeq != 4 || info.PendingApproval != (api.ApprovalRef{}) {
					t.Fatalf("lagging projection: %+v", info)
				}
			}
			before := captureDeletion(t, s, "s")
			deleted, err := s.DeleteSession(context.Background(), "s")
			if err != nil {
				t.Fatal(err)
			}
			if deleted.LastSeq != before.Info.LastSeq+1 || deleted.LastSeq != deleted.DeletionSeq || deleted.ComputeState != before.Info.ComputeState || deleted.PendingApproval != before.Info.PendingApproval {
				t.Errorf("DELETE lost captured state: before=%+v after=%+v", before.Info, deleted)
			}
			after := captureDeletion(t, s, "s")
			if len(before.Events) > 0 && !reflect.DeepEqual(after.Events[:len(before.Events)], before.Events) {
				t.Fatal("DELETE rewrote journal prefix")
			}
			if shape != "empty" {
				if _, err := controller.Approve(log, decision); !errors.Is(err, eventlog.ErrDeleted) {
					t.Errorf("deleted Approve (including exact retry)=%v, want ErrDeleted", err)
				}
			}
			retry, err := s.DeleteSession(context.Background(), "s")
			if err != nil || !reflect.DeepEqual(retry, deleted) {
				t.Fatalf("retry=%+v %v", retry, err)
			}
			assertDeletionUnchanged(t, s, "s", after)
			if err := log.Verify(); err != nil {
				t.Fatal(err)
			}
			s.Close()
			s = deletionStore(t, path)
			if got := deletionInfo(t, s, "s"); !reflect.DeepEqual(got, deleted) {
				t.Fatalf("reopen=%+v want=%+v", got, deleted)
			}
		})
	}
}

// Failure of the tombstone/cursor write must roll back the bypass writer and approval state.
func TestDeletionApprovalProjectionRollback(t *testing.T) {
	s := deletionStore(t, ":memory:")
	for i, ev := range approvalEvents("execution") {
		if _, err := s.Session("s").Append(int64(i), 0, ev); err != nil {
			t.Fatal(err)
		}
	}
	before := captureDeletion(t, s, "s")
	if _, err := s.db.Exec(`CREATE TRIGGER fail_delete BEFORE UPDATE OF deletion_state ON sessions BEGIN SELECT RAISE(ABORT,'delete rollback'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DeleteSession(context.Background(), "s"); !isConstraint(err) {
		t.Fatalf("DELETE failing update=%v, want SQLite constraint cause", err)
	}
	assertDeletionUnchanged(t, s, "s", before)
}
