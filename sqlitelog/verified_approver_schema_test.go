package sqlitelog_test

import (
	"path/filepath"
	"reflect"
	"testing"

	"github.com/agent-primitives/sessions/api"
	"github.com/agent-primitives/sessions/sqlitelog"
	"google.golang.org/protobuf/proto"
)

var verifiedApprover = api.CallerIdentity{Issuer: "https://issuer.example", Subject: "approver"}

func gatedTurn(approver api.CallerIdentity) []api.Event {
	return []api.Event{
		{ExecutionID: "e", Kind: api.EventExecutionStart, ExecutionStart: &api.ExecutionStart{InputCount: proto.Int64(0), Harness: "gate"}},
		{ExecutionID: "e", Kind: api.EventToolCall, ToolCall: &api.ToolCall{ID: "call", Tool: "write", IdempotencyKey: "key", Mediation: api.MediationRequiresApproval}},
		{ExecutionID: "e", Kind: api.EventApprovalRequest, Approval: &api.ApprovalRequest{ToolCallID: "call"}},
		{ExecutionID: "e", Kind: api.EventApprovalResult, ApprovalResult: &api.ApprovalResult{ToolCallID: "call", RequestSeq: 3, Approved: true, VerifiedApprover: approver}},
	}
}

// Creator: the schema version 7 build at d5a98c80 (sqlitelog.Open, Session(...).Append).
// SHA-256: 3df9801f1cdb9f1a249694b5131c2e19ef8a9efb8f0944a3f1859634944c2a87.
// Holds "decided" (a gated call, a decision without an approver, its receipt and END) and
// "parked" (a gated call waiting for a decision).
const approvalV7Fixture = "testdata/approval-v7.db"

// A journal the version 7 build wrote opens: it is stamped with this build's version, gets a fresh
// database's schema, keeps its records, hashes and approval projection, and its decision reads
// without an approver. Its parked call can then take a decision that carries one.
func TestSchemaV7JournalOpens(t *testing.T) {
	path := copyFixture(t, approvalV7Fixture)
	if got := readUserVersion(t, path); got != 7 {
		t.Fatalf("fixture is v%d, want genuine v7", got)
	}
	raw := rawDB(t, path)
	queries := []string{`SELECT * FROM events ORDER BY session,seq`, `SELECT * FROM approval_executions ORDER BY session,execution_id`, `SELECT * FROM approval_calls ORDER BY session,execution_id,call_id`}
	before := make([][][]any, len(queries))
	for i, q := range queries {
		before[i] = queryValues(t, raw, q)
	}

	s := mustOpen(t, path)
	defer s.Close()
	if got := readUserVersion(t, path); got != sqlitelog.SchemaVersion {
		t.Fatalf("user_version = %d after Open, want %d", got, sqlitelog.SchemaVersion)
	}
	fresh := filepath.Join(t.TempDir(), "fresh.db")
	mustOpen(t, fresh).Close()
	if got, want := schemaOf(t, path), schemaOf(t, fresh); !reflect.DeepEqual(got, want) {
		t.Fatalf("migrated v7 schema differs from a fresh database:\nmigrated %v\nfresh    %v", got, want)
	}
	for i, q := range queries {
		if !reflect.DeepEqual(queryValues(t, raw, q), before[i]) {
			t.Fatalf("opening the v7 journal changed persisted data: %s", q)
		}
	}

	decided := s.Session("decided")
	if err := decided.Verify(); err != nil {
		t.Fatalf("v7 journal fails verification: %v", err)
	}
	records, err := decided.Read(4)
	if err != nil || len(records) == 0 || records[0].Event.ApprovalResult == nil {
		t.Fatalf("v7 decision = %+v, %v", records, err)
	}
	if got := records[0].Event.ApprovalResult; !got.Approved || got.RequestSeq != 3 || got.VerifiedApprover != (api.CallerIdentity{}) {
		t.Fatalf("v7 decision reads as %+v, want approved request 3 without an approver", got)
	}

	parked := s.Session("parked")
	fence, err := parked.NewFence()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parked.Append(3, fence, gatedTurn(verifiedApprover)[3]); err != nil {
		t.Fatalf("append a decision with an approver to the v7 journal: %v", err)
	}
	if err := parked.Verify(); err != nil {
		t.Fatalf("decision with a verified approver fails verification: %v", err)
	}
	records, err = parked.Read(4)
	if err != nil || len(records) != 1 || records[0].Event.ApprovalResult.VerifiedApprover != verifiedApprover {
		t.Fatalf("stored decision = %+v, %v", records, err)
	}
}

// A version 7 build refuses any database stamped above 7 (TestOpenRejectsNewerSchema pins that
// rule), so it refuses a journal this build opened, before it can read a verified approver it would
// drop and append under a chain it cannot verify. This pins the stamp that rule reads.
func TestJournalStampIsNewerThanVersion7(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fresh.db")
	mustOpen(t, path).Close()
	if got := readUserVersion(t, path); got <= 7 {
		t.Fatalf("user_version = %d; a version 7 build would open a journal that can hold verified approvers", got)
	}
}
