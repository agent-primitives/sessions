package sqlitelog

import (
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"

	"github.com/aramase/agentsessions/api"
	"github.com/aramase/agentsessions/canon"
	"github.com/aramase/agentsessions/eventlog"
	"github.com/aramase/agentsessions/wire"
	"google.golang.org/protobuf/proto"
)

func projectionEvents(id string) []api.Event {
	return []api.Event{
		{ExecutionID: id, Kind: api.EventExecutionStart, ExecutionStart: &api.ExecutionStart{InputCount: proto.Int64(0), Harness: "gate"}},
		{ExecutionID: id, Kind: api.EventToolCall, ToolCall: &api.ToolCall{ID: "call", IdempotencyKey: "key", Mediation: api.MediationRequiresApproval}},
		{ExecutionID: id, Kind: api.EventApprovalRequest, Approval: &api.ApprovalRequest{ToolCallID: "call"}},
	}
}

// Build an actual pre-projection database from the frozen schema rungs, not by deleting
// arbitrary columns from today's shape. Journal encoding/hashing is unchanged across rungs.
func projectionFixture(t *testing.T, version int) (string, *sql.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(schema); err != nil {
		t.Fatal(err)
	}
	if version == 2 {
		if _, err := db.Exec(v1ToV2); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec("PRAGMA user_version = " + strconv.Itoa(version)); err != nil {
		t.Fatal(err)
	}
	for _, uid := range []string{"healthy", "bad"} {
		if _, err := db.Exec(`INSERT INTO sessions(session,project,harness,compute_state,created_at,updated_at) VALUES(?,'default','gate','COLD',10,20)`, uid); err != nil {
			t.Fatal(err)
		}
		prev := ""
		for i, ev := range projectionEvents("execution") {
			seq := int64(i + 1)
			hash, err := canon.HashRecord(prev, seq, ev)
			if err != nil {
				t.Fatal(err)
			}
			blob, err := proto.Marshal(wire.EventToProto(ev))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`INSERT INTO events(session,seq,prev_hash,hash,fence,event) VALUES(?,?,?,?,0,?)`, uid, seq, prev, hash, blob); err != nil {
				t.Fatal(err)
			}
			prev = hash
		}
	}
	return path, db
}

func TestApprovalProjectionMigrationAndReopen(t *testing.T) {
	for _, version := range []int{1, 2} {
		t.Run(strconv.Itoa(version), func(t *testing.T) {
			path, raw := projectionFixture(t, version)
			s, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			for _, uid := range []string{"healthy", "bad"} {
				info, err := s.SessionInfo(uid)
				if err != nil {
					t.Fatal(err)
				}
				want := api.ApprovalRef{ExecutionID: "execution", ToolCallID: "call", RequestSeq: 3}
				if info.LastSeq != 3 || info.ComputeState != api.ComputeCold || info.PendingApproval != want || info.CreatedAt.UnixNano() != 10 || info.UpdatedAt.UnixNano() != 20 {
					t.Fatalf("migrated info=%+v", info)
				}
				if err := s.Session(uid).Verify(); err != nil {
					t.Fatalf("migration rewrote chain: %v", err)
				}
			}
			s.Close()
			s, err = Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			// Resuming the reducer after a reopen must match the original exact request sequence.
			ev := api.Event{ExecutionID: "execution", Kind: api.EventApprovalResult, ApprovalResult: &api.ApprovalResult{ToolCallID: "call", RequestSeq: 3, Approved: true}}
			if _, err := s.Session("healthy").Append(3, 0, ev); err != nil {
				t.Fatal(err)
			}
			got, err := s.SessionInfo("healthy")
			if err != nil || got.LastSeq != 4 || got.PendingApproval.RequestSeq != 0 {
				t.Fatalf("after reopen decision=%+v %v", got, err)
			}
			var stamp int
			if err := raw.QueryRow(`PRAGMA user_version`).Scan(&stamp); err != nil || stamp != SchemaVersion {
				t.Fatalf("schema stamp=%d err=%v", stamp, err)
			}
		})
	}
}

func TestApprovalProjectionCorruptV2Isolated(t *testing.T) {
	path, raw := projectionFixture(t, 2)
	if _, err := raw.Exec(`UPDATE events SET event=x'ff' WHERE session='bad' AND seq=2`); err != nil {
		t.Fatal(err)
	}
	var beforeHash string
	var beforeBlob []byte
	if err := raw.QueryRow(`SELECT hash,event FROM events WHERE session='bad' AND seq=2`).Scan(&beforeHash, &beforeBlob); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatalf("one corrupt journal blocked store opening: %v", err)
	}
	defer s.Close()
	page, err := s.ListSessions(ListOptions{})
	if err != nil || len(page.Sessions) != 2 {
		t.Fatalf("page=%+v err=%v", page, err)
	}
	bad, err := s.SessionInfo("bad")
	if err != nil || bad.LastSeq != 3 || bad.PendingApproval.RequestSeq != 0 {
		t.Fatalf("corrupt info=%+v err=%v", bad, err)
	}
	healthy, err := s.SessionInfo("healthy")
	if err != nil || healthy.PendingApproval.RequestSeq != 3 {
		t.Fatalf("healthy=%+v err=%v", healthy, err)
	}
	// Corrupt bytes still fail strict journal reads; migration must neither rehash nor repair them.
	if _, err := s.Session("bad").Read(1); err == nil {
		t.Fatal("corrupt journal silently repaired")
	}
	var hash string
	var blob []byte
	if err := raw.QueryRow(`SELECT hash,event FROM events WHERE session='bad' AND seq=2`).Scan(&hash, &blob); err != nil || hash != beforeHash || !reflect.DeepEqual(blob, beforeBlob) {
		t.Fatalf("event mutated: %s %x %v", hash, blob, err)
	}
	// Future valid evidence cannot grant authority on an undecodable session.
	for i, ev := range projectionEvents("later") {
		if _, err := s.Session("bad").Append(int64(3+i), 0, ev); err != nil {
			t.Fatal(err)
		}
	}
	bad, err = s.SessionInfo("bad")
	if err != nil || bad.LastSeq != 6 || bad.PendingApproval.RequestSeq != 0 {
		t.Fatalf("corrupt session regained reference: %+v %v", bad, err)
	}
	s.Close()
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	bad, err = s.SessionInfo("bad")
	if err != nil || bad.PendingApproval.RequestSeq != 0 {
		t.Fatalf("reopened corruption isolation=%+v %v", bad, err)
	}
}

func TestApprovalProjectionAppendAtomicity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "atomic.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	log := s.Session("s")
	fence, err := log.NewFence()
	if err != nil {
		t.Fatal(err)
	}
	for i, ev := range projectionEvents("execution") {
		if _, err := log.Append(int64(i), fence, ev); err != nil {
			t.Fatal(err)
		}
	}
	before, err := s.SessionInfo("s")
	if err != nil {
		t.Fatal(err)
	}
	records, err := log.Read(1)
	if err != nil {
		t.Fatal(err)
	}
	decision := api.Event{ExecutionID: "execution", Kind: api.EventApprovalResult, ApprovalResult: &api.ApprovalResult{ToolCallID: "call", RequestSeq: 3, Approved: true}}
	if _, err := log.Append(2, fence, decision); !errors.Is(err, eventlog.ErrConflict) {
		t.Fatalf("CAS=%v", err)
	}
	if _, err := log.Append(3, fence-1, decision); !errors.Is(err, eventlog.ErrFenced) {
		t.Fatalf("fence=%v", err)
	}
	// Fault after event/metadata writes and during reducer persistence must roll everything back.
	if _, err := s.db.Exec(`CREATE TRIGGER fail_projection BEFORE UPDATE ON approval_executions BEGIN SELECT RAISE(ABORT,'projection fault'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := log.Append(3, fence, decision); err == nil {
		t.Fatal("SQL fault accepted")
	}
	after, err := s.SessionInfo("s")
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("partial projection=%+v %v", after, err)
	}
	afterRecords, err := log.Read(1)
	if err != nil || !reflect.DeepEqual(records, afterRecords) {
		t.Fatalf("partial append=%v %v", afterRecords, err)
	}
	if _, err := s.db.Exec(`DROP TRIGGER fail_projection`); err != nil {
		t.Fatal(err)
	}
	if _, err := log.Append(3, fence, decision); err != nil {
		t.Fatal(err)
	}
	after, err = s.SessionInfo("s")
	if err != nil || after.LastSeq != 4 || after.PendingApproval.RequestSeq != 0 {
		t.Fatalf("decision=%+v %v", after, err)
	}
	if before.PendingApproval.RequestSeq != 3 {
		t.Fatalf("captured snapshot mutated: %+v", before)
	}
}

func TestApprovalProjectionMigrationSQLFaultRollsBack(t *testing.T) {
	path, raw := projectionFixture(t, 2)
	if _, err := raw.Exec(`CREATE TRIGGER fail_backfill BEFORE UPDATE ON sessions BEGIN SELECT RAISE(ABORT,'backfill fault'); END`); err != nil {
		t.Fatal(err)
	}
	if s, err := Open(path); err == nil {
		s.Close()
		t.Fatal("SQL backfill fault accepted")
	}
	var stamp int
	if err := raw.QueryRow(`PRAGMA user_version`).Scan(&stamp); err != nil || stamp != 2 {
		t.Fatalf("stamp=%d %v", stamp, err)
	}
	var count int
	if err := raw.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('sessions') WHERE name='last_seq'`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("half-applied rung columns=%d %v", count, err)
	}
	if _, err := raw.Exec(`DROP TRIGGER fail_backfill`); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	info, err := s.SessionInfo("healthy")
	if err != nil || info.PendingApproval.RequestSeq != 3 {
		t.Fatalf("retry migration=%+v %v", info, err)
	}
}
