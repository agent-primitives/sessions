package sqlitelog_test

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/agent-primitives/sessions/api"
	"github.com/agent-primitives/sessions/sqlitelog"
)

// Creator: approval controller slice SHA e600869 (revised unreleased v3 with call_seq).
// SHA-256: b341bfbbbb8aba2f2440ff4c39fe244aeb067163276092055bae53878c5e19da.
// Contains empty, parked and decided-without-receipt sessions, a retired registration
// and a reserved static route, persisted through the first-lander public store API.
const approvalV3Fixture = "testdata/approval-v3.db"

// This file was written by the actual v3 store, not current Open with a relabeled stamp.
// The deletion rung must preserve the whole scalar approval projection and persisted audit.
func TestDeletionUpgradesGenuineApprovalV3(t *testing.T) {
	path := copyFixture(t, approvalV3Fixture)
	raw := rawDB(t, path)
	if got := readUserVersion(t, path); got != 3 {
		t.Fatalf("fixture is v%d, want genuine v3", got)
	}
	beforeSessions := queryValues(t, raw, `SELECT * FROM sessions ORDER BY session`)
	queries := []string{`SELECT * FROM events ORDER BY session,seq`, `SELECT * FROM approval_executions ORDER BY session,execution_id`, `SELECT * FROM approval_calls ORDER BY session,execution_id,call_id`, `SELECT * FROM harnesses ORDER BY name`, `SELECT * FROM reserved_harness_names ORDER BY name`}
	before := make([][][]any, len(queries))
	for i, q := range queries {
		before[i] = queryValues(t, raw, q)
	}
	s := mustOpen(t, path)
	if got := readUserVersion(t, path); got != sqlitelog.SchemaVersion {
		t.Errorf("upgrade stamp=%d, want v%d", got, sqlitelog.SchemaVersion)
	}
	for i, q := range queries {
		if !reflect.DeepEqual(queryValues(t, raw, q), before[i]) {
			t.Fatalf("v3 upgrade changed persisted data: %s", q)
		}
	}
	afterSessions := queryValues(t, raw, `SELECT session,fence,project,name,harness,model,parent_uid,fork_seq,compute_state,created_at,updated_at,labels,annotations,origin,identity,last_seq,latest_execution,reporting_fork_seq,approval_corrupt,invalid_approval_executions FROM sessions ORDER BY session`)
	if !reflect.DeepEqual(afterSessions, beforeSessions) {
		t.Fatal("upgrade changed v3 metadata/fences/cursor/projection")
	}
	for _, uid := range []string{"empty", "parked", "approved"} {
		info, err := s.SessionInfo(uid)
		if err != nil {
			t.Fatal(err)
		}
		wantSeq := map[string]int64{"empty": 0, "parked": 4, "approved": 5}[uid]
		wantRef := api.ApprovalRef{}
		if uid == "parked" {
			wantRef = api.ApprovalRef{ExecutionID: "execution", ToolCallID: "call", RequestSeq: 3}
		}
		if info.LastSeq != wantSeq || info.PendingApproval != wantRef || info.DeletionState != api.DeletionActive {
			t.Fatalf("migrated %s=%+v", uid, info)
		}
		if err := s.Session(uid).Verify(); err != nil {
			t.Fatal(err)
		}
	}
	s.Close()
	s = mustOpen(t, path)
	fresh := filepath.Join(t.TempDir(), "fresh.db")
	mustOpen(t, fresh).Close()
	if !reflect.DeepEqual(schemaOf(t, path), schemaOf(t, fresh)) {
		t.Fatal("v3 upgrade/fresh/reopen shapes differ")
	}
	info, err := s.DeleteSession(context.Background(), "parked")
	if err != nil || info.LastSeq != 5 || info.PendingApproval.RequestSeq != 3 {
		t.Fatalf("upgraded delete=%+v %v", info, err)
	}
	if err := s.Session("parked").Verify(); err != nil {
		t.Fatal(err)
	}
}

// A failure after two ALTERs must roll back the entire v4 rung, including the stamp.
func TestDeletionV4RungFailureRollsBackGenuineV3(t *testing.T) {
	path := copyFixture(t, approvalV3Fixture)
	raw := rawDB(t, path)
	if _, err := raw.Exec(`ALTER TABLE sessions ADD COLUMN deletion_completed_at INTEGER NOT NULL DEFAULT 0`); err != nil {
		t.Fatal(err)
	}
	shape := schemaOf(t, path)
	events := queryValues(t, raw, `SELECT * FROM events ORDER BY session,seq`)
	s, err := sqlitelog.Open(path)
	if err == nil {
		s.Close()
		t.Fatal("conflicting v4 rung accepted")
	}
	if readUserVersion(t, path) != 3 || !reflect.DeepEqual(shape, schemaOf(t, path)) || !reflect.DeepEqual(events, queryValues(t, raw, `SELECT * FROM events ORDER BY session,seq`)) {
		t.Fatal("failed v4 rung changed shape/stamp/events")
	}
}
