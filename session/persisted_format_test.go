package session_test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"

	"github.com/agent-primitives/sessions/api"
	v1 "github.com/agent-primitives/sessions/api/genpb"
	"github.com/agent-primitives/sessions/canon"
	"github.com/agent-primitives/sessions/controller"
	"github.com/agent-primitives/sessions/harness/echoagent"
	"github.com/agent-primitives/sessions/placement"
	"github.com/agent-primitives/sessions/runtime/local"
	"github.com/agent-primitives/sessions/runtime/remote"
	"github.com/agent-primitives/sessions/session"
	"github.com/agent-primitives/sessions/sqlitelog"
)

// schemaV4Fixture is a journal written by this module's own session.Service and HarnessRegistry
// over sqlitelog, wired as agentsessionsd wires them, at schema version 4. It is the persisted-format
// contract for what the next release writes: a change to the on-disk schema, the event encoding or
// the canonical hash form that this build cannot read back fails here. Regenerate it only for an
// intentional, unreleased format change, with hack/gen-journal-fixture.sh; once a release has written
// schema version 4, freeze it and add a fixture for the next version instead.
const schemaV4Fixture = "../sqlitelog/testdata/schema-v4.db"

const (
	fixtureProject  = "compat"
	fixtureTool     = "charge"
	fixtureModelID  = "fixture-model"
	fixtureRemote   = "remote-echo"
	fixtureRetireBy = "fixture: retired after registration"
)

// fixtureGateHarness calls one REQUIRES_APPROVAL tool, then hands its receipt to the model. It is
// deterministic so the recorded turn replays.
type fixtureGateHarness struct{}

func (fixtureGateHarness) Describe(context.Context) (api.Descriptor, error) {
	return api.Descriptor{
		ID: "fixture-gate", Version: "v1",
		Tools:        []api.ToolSpec{{Name: fixtureTool, Description: "charge a card", Mediation: api.MediationRequiresApproval}},
		Capabilities: api.Capabilities{Resumability: api.ResumabilityStatelessReplay},
	}, nil
}

func (fixtureGateHarness) Run(ctx context.Context, start *api.Start, sink api.EventSink) error {
	result, err := sink.ToolCall(ctx, fixtureCall())
	if err != nil {
		return err
	}
	messages := append([]api.Message(nil), start.Inputs...)
	messages = append(messages, api.Message{Role: "tool", Parts: []api.Part{{ToolResult: &result}}})
	resp, err := sink.Model(ctx, api.ModelRequest{Model: fixtureModelID, Messages: messages})
	if err != nil {
		return err
	}
	return sink.Usage(ctx, resp.Usage)
}

// fixtureCall's arguments exercise every JSON shape a google.protobuf.Struct carries.
func fixtureCall() api.ToolCall {
	return api.ToolCall{
		ID: "call-1", Tool: fixtureTool, IdempotencyKey: "charge-1", Mediation: api.MediationRequiresApproval,
		Args: map[string]any{
			"amount": 12.5, "currency": "USD", "capture": true, "note": nil,
			"items": []any{"a", 2.0, false},
			"card":  map[string]any{"last4": "4242", "exp": map[string]any{"month": 12.0, "year": 2030.0}},
		},
	}
}

func fixtureModel(_ context.Context, req api.ModelRequest) (api.ModelResponse, error) {
	return api.ModelResponse{
		Message: *api.TextMessage("assistant", "charged "+req.Messages[0].Text()),
		Usage:   api.Usage{Model: fixtureModelID, InputTokens: 11, OutputTokens: 7, ReasoningTokens: 3},
	}, nil
}

func fixtureExecutor(_ context.Context, _ controller.ToolCallContext, tc api.ToolCall) (api.ToolResult, error) {
	return api.ToolResult{ID: tc.ID, Output: map[string]any{"receipt": "r-1", "amount": 12.5}}, nil
}

// fixtureHost serves the Sessions and HarnessRegistry APIs over store, with the static harnesses the
// fixture was written with: echo (agentsessionsd's default) and the gate harness.
type fixtureHost struct {
	sessions v1.SessionsClient
	registry v1.HarnessRegistryClient
}

func newFixtureHost(t *testing.T, store *sqlitelog.Store) fixtureHost {
	t.Helper()
	echo := local.New(echoagent.Harness{})
	gate := local.New(fixtureGateHarness{})
	t.Cleanup(func() { _ = echo.Close(); _ = gate.Close() })
	registry, err := placement.NewRegistry("echo", map[string]*placement.Placer{
		"echo": placement.New(echo, echoagent.Model),
		"gate": placement.New(gate, fixtureModel, placement.WithToolExecutor(fixtureExecutor)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := session.ReserveStaticHarnessNames(store, registry); err != nil {
		t.Fatal(err)
	}
	// As agentsessionsd's registeredPlacer: a remote placement is built without dialing.
	operator, err := session.NewHarnessRegistry(store, registry, func(_ string, spec *v1.HarnessSpec) (*placement.Placer, func(), error) {
		backend := remote.New(spec.GetRemote().GetAddress())
		return placement.New(backend, echoagent.Model, placement.WithDescriptorID(spec.GetDescriptorId())), func() { _ = backend.Close() }, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(operator.Close)
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	v1.RegisterSessionsServer(srv, session.NewService(store, registry, session.WithDefaultProject(fixtureProject)))
	v1.RegisterHarnessRegistryServer(srv, operator)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	conn, err := grpc.NewClient("passthrough:///fixture", grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return fixtureHost{sessions: v1.NewSessionsClient(conn), registry: v1.NewHarnessRegistryClient(conn)}
}

func (h fixtureHost) exec(t *testing.T, uid, input string) *v1.Session {
	t.Helper()
	req := &v1.ExecRequest{Session: uid}
	if input != "" {
		req.Inputs = []*v1.Message{{Role: "user", Parts: []*v1.Part{{Part: &v1.Part_Text{Text: &v1.TextPart{Text: input}}}}}}
	}
	stream, err := h.sessions.Exec(t.Context(), req)
	if err != nil {
		t.Fatalf("exec %s: %v", uid, err)
	}
	var last *v1.Session
	for {
		u, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return last
		}
		if err != nil {
			t.Fatalf("exec %s: %v", uid, err)
		}
		if s := u.GetSession(); s != nil {
			last = s
		}
	}
}

func fixtureSpec() *v1.HarnessSpec {
	return &v1.HarnessSpec{
		Placement:    &v1.HarnessSpec_Remote{Remote: &v1.RemotePlacement{Address: "127.0.0.1:1"}},
		Capabilities: &v1.Capabilities{Resumability: v1.Resumability_RESUMABILITY_STATELESS_REPLAY},
		DescriptorId: "echo",
	}
}

// TestWriteSchemaV4Fixture regenerates the fixture only from a schema v4 checkout.
// It runs only when WRITE_JOURNAL_FIXTURE=1; use hack/gen-journal-fixture.sh.
func TestWriteSchemaV4Fixture(t *testing.T) {
	if os.Getenv("WRITE_JOURNAL_FIXTURE") != "1" {
		t.Skip("set WRITE_JOURNAL_FIXTURE=1 to regenerate " + schemaV4Fixture)
	}
	if sqlitelog.SchemaVersion != 4 {
		t.Fatal("schema-v4.db is frozen; regenerate only from a schema v4 checkout")
	}
	path := filepath.Join(t.TempDir(), "journal.db")
	store, err := sqlitelog.Open(path, sqlitelog.WithDefaultProject(fixtureProject))
	if err != nil {
		t.Fatal(err)
	}
	h := newFixtureHost(t, store)
	ctx := t.Context()

	parent, err := h.sessions.CreateSession(ctx, &v1.CreateSessionRequest{Session: &v1.Session{
		Metadata:    &v1.ResourceMetadata{Project: fixtureProject, Name: "parent"},
		Harness:     "echo",
		Model:       "echo-model",
		Identity:    &v1.IdentityRef{Principal: "agent://fixture", Issuer: "fixture-issuer", Subject: "sub-1"},
		Origin:      &v1.Origin{Source: "fixture", Subject: "compat", Uri: "https://example.invalid/compat", Attributes: map[string]string{"k": "v"}},
		Labels:      map[string]string{"suite": "compat"},
		Annotations: map[string]string{"note": "persisted-format fixture"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	puid := parent.GetMetadata().GetUid()
	h.exec(t, puid, "hello")
	h.exec(t, puid, "again")
	if _, err := h.sessions.Suspend(ctx, &v1.SuspendRequest{Session: puid}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.sessions.Resume(ctx, &v1.ResumeRequest{Session: puid}); err != nil {
		t.Fatal(err)
	}
	fork, err := h.sessions.Fork(ctx, &v1.ForkRequest{Session: puid, Count: 1, ChildNames: []string{"child"}, Labels: map[string]string{"branch": "a"}})
	if err != nil {
		t.Fatal(err)
	}
	h.exec(t, fork.GetChildren()[0].GetMetadata().GetUid(), "from child")

	gate, err := h.sessions.CreateSession(ctx, &v1.CreateSessionRequest{Session: &v1.Session{
		Metadata: &v1.ResourceMetadata{Project: fixtureProject, Name: "gate"}, Harness: "gate",
	}})
	if err != nil {
		t.Fatal(err)
	}
	guid := gate.GetMetadata().GetUid()
	parked := h.exec(t, guid, "card 4242")
	ref := parked.GetPendingApproval()
	if ref == nil {
		t.Fatalf("gate turn did not park: %v", parked)
	}
	if _, err := h.sessions.Approve(ctx, &v1.ApproveRequest{
		Session: guid, ExecutionId: ref.GetExecutionId(), ToolCallId: ref.GetToolCallId(), RequestSeq: ref.GetRequestSeq(),
		Approved: proto.Bool(true), Reason: "fixture approval", Identity: &v1.IdentityRef{Principal: "user://approver"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.sessions.Resume(ctx, &v1.ResumeRequest{Session: guid}); err != nil {
		t.Fatal(err)
	}

	deleted, err := h.sessions.CreateSession(ctx, &v1.CreateSessionRequest{Session: &v1.Session{
		Metadata: &v1.ResourceMetadata{Project: fixtureProject, Name: "deleted"}, Harness: "echo",
	}})
	if err != nil {
		t.Fatal(err)
	}
	duid := deleted.GetMetadata().GetUid()
	h.exec(t, duid, "bye")
	if _, err := h.sessions.DeleteSession(ctx, &v1.DeleteSessionRequest{Uid: duid}); err != nil {
		t.Fatal(err)
	}

	if _, err := h.registry.RegisterHarness(ctx, &v1.RegisterHarnessRequest{Name: fixtureRemote, Spec: fixtureSpec()}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.registry.RetireHarness(ctx, &v1.RetireHarnessRequest{Name: fixtureRemote, Reason: fixtureRetireBy}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	// Fold the WAL into the main file and leave it in rollback mode, so the checked-in file is the
	// whole database and opening it does not need side files.
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{"PRAGMA wal_checkpoint(TRUNCATE)", "PRAGMA journal_mode=DELETE", "VACUUM"} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(schemaV4Fixture, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func copyJournal(t *testing.T, src string) string {
	t.Helper()
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), filepath.Base(src))
	if err := os.WriteFile(dst, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return dst
}

func kindsOf(recs []*v1.LogRecord) []string {
	out := make([]string, 0, len(recs))
	for _, r := range recs {
		out = append(out, strings.TrimPrefix(r.GetEvent().GetKind().String(), "EVENT_"))
	}
	return out
}

func (h fixtureHost) replay(t *testing.T, uid string) []*v1.LogRecord {
	t.Helper()
	stream, err := h.sessions.Replay(t.Context(), &v1.ReplayRequest{Session: uid})
	if err != nil {
		t.Fatal(err)
	}
	var out []*v1.LogRecord
	for {
		r, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatalf("replay %s: %v", uid, err)
		}
		out = append(out, r)
	}
}

// TestSchemaV4FixtureReadsReplaysAndContinues opens the checked-in schema version 4 journal with
// this build, verifies every hash chain, checks what each record decodes to, re-executes every
// session's turns from recorded effects, and appends to it.
func TestSchemaV4FixtureReadsReplaysAndContinues(t *testing.T) {
	path := copyJournal(t, schemaV4Fixture)
	if v := userVersionOf(t, path); v != 4 {
		t.Fatalf("fixture is stamped schema v%d, want v4", v)
	}
	// List hides a deleted session, so find its UID the way an operator auditing the file would.
	duid := sessionUIDOf(t, path, "deleted")
	store := openStore(t, path, sqlitelog.WithDefaultProject(fixtureProject))
	h := newFixtureHost(t, store)
	ctx := t.Context()

	list, err := h.sessions.ListSessions(ctx, &v1.ListSessionsRequest{Project: fixtureProject})
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]*v1.Session{}
	for _, s := range list.GetSessions() {
		byName[s.GetMetadata().GetName()] = s
	}
	if len(byName) != 3 || byName["parent"] == nil || byName["child"] == nil || byName["gate"] == nil {
		t.Fatalf("fixture sessions = %v, want parent, child and gate", list.GetSessions())
	}
	parent, child, gate := byName["parent"], byName["child"], byName["gate"]
	deleted, err := h.sessions.GetSession(ctx, &v1.GetSessionRequest{Uid: duid})
	if err != nil {
		t.Fatal(err)
	}
	if deleted.GetMetadata().GetName() != "deleted" || deleted.GetDeletionState() != v1.DeletionState_DELETION_PENDING ||
		deleted.GetDeletionRequestedTime() == nil {
		t.Fatalf("deleted session = %v", deleted)
	}
	byName["deleted"] = deleted

	// Every column the store round-trips for a session.
	if parent.GetHarness() != "echo" || parent.GetModel() != "echo-model" ||
		parent.GetIdentity().GetPrincipal() != "agent://fixture" || parent.GetIdentity().GetSubject() != "sub-1" ||
		parent.GetOrigin().GetUri() != "https://example.invalid/compat" || parent.GetOrigin().GetAttributes()["k"] != "v" ||
		parent.GetLabels()["suite"] != "compat" || parent.GetAnnotations()["note"] != "persisted-format fixture" ||
		parent.GetComputeState() != v1.ComputeState_COMPUTE_LIVE {
		t.Fatalf("parent metadata = %v", parent)
	}
	if child.GetParentUid() != parent.GetMetadata().GetUid() || child.GetForkSeq() != 12 || child.GetLabels()["branch"] != "a" {
		t.Fatalf("child lineage = %v", child)
	}
	if gate.GetHarness() != "gate" || gate.GetPendingApproval() != nil || gate.GetExecState() != v1.ExecState_EXEC_COMPLETED {
		t.Fatalf("gate session = %v", gate)
	}

	turn := []string{"EXECUTION_START", "INPUT", "MODEL_CALL", "OUTPUT", "END"}
	parentKinds := concat(turn, turn, []string{"LIFECYCLE", "LIFECYCLE"}) // two turns, Suspend, Resume
	want := map[string][]string{
		"parent":  parentKinds,
		"child":   concat(parentKinds, []string{"LIFECYCLE"}, turn), // FORK, then its own turn
		"deleted": concat(turn, []string{"LIFECYCLE"}),              // a turn, then DELETE
		"gate": {
			"EXECUTION_START", "INPUT", "TOOL_CALL", "APPROVAL_REQUEST", "LIFECYCLE",
			"APPROVAL_RESULT", "TOOL_RESULT", "MODEL_CALL", "OUTPUT", "USAGE", "END", "LIFECYCLE",
		},
	}
	records := map[string][]*v1.LogRecord{}
	for name, s := range byName {
		uid := s.GetMetadata().GetUid()
		if err := store.Session(uid).Verify(); err != nil {
			t.Fatalf("verify %s: %v", name, err)
		}
		recs := h.replay(t, uid)
		if got := kindsOf(recs); !reflect.DeepEqual(got, want[name]) {
			t.Errorf("%s journal kinds:\n got %v\nwant %v", name, got, want[name])
		}
		if last := recs[len(recs)-1].GetSeq(); last != s.GetLastSeq() {
			t.Fatalf("%s last_seq = %d, journal ends at %d", name, s.GetLastSeq(), last)
		}
		records[name] = recs
	}
	if t.Failed() {
		t.FailNow()
	}

	// A fork child's chain hangs off its parent's record at fork_seq.
	if records["child"][0].GetPrevHash() != "" ||
		records["child"][child.GetForkSeq()-1].GetContentHash() != records["parent"][child.GetForkSeq()-1].GetContentHash() {
		t.Fatal("child does not share the parent's chain up to fork_seq")
	}

	// The deleted session's last record is the DELETE intent, stamped with the request time Get
	// reports, and the tombstone still refuses a new turn.
	tombstone := records["deleted"][len(records["deleted"])-1].GetEvent()
	if tombstone.GetLifecycle().GetKind() != v1.Lifecycle_LIFECYCLE_DELETE ||
		!proto.Equal(tombstone.GetTs(), deleted.GetDeletionRequestedTime()) {
		t.Fatalf("DELETE record = %v, want LIFECYCLE_DELETE at %v", tombstone, deleted.GetDeletionRequestedTime())
	}
	if code := execCode(t, h, duid, "after upgrade"); code != codes.FailedPrecondition {
		t.Fatalf("exec on the deleted session = %v, want FailedPrecondition", code)
	}

	// The gate turn's tool call, approval and receipt decode to what was written.
	var call *v1.ToolCall
	var decision *v1.ApprovalResult
	var approver *v1.IdentityRef
	var receipt *v1.ToolResult
	var usage *v1.Usage
	for _, r := range records["gate"] {
		ev := r.GetEvent()
		switch ev.GetKind() {
		case v1.EventKind_EVENT_TOOL_CALL:
			call = ev.GetTool()
		case v1.EventKind_EVENT_APPROVAL_RESULT:
			decision, approver = ev.GetApprovalResult(), ev.GetActor()
		case v1.EventKind_EVENT_TOOL_RESULT:
			receipt = ev.GetResult()
		case v1.EventKind_EVENT_USAGE:
			usage = ev.GetUsage()
		}
	}
	if call == nil || !reflect.DeepEqual(call.GetArgs().AsMap(), fixtureCall().Args) || call.GetMediation() != v1.Mediation_MEDIATION_REQUIRES_APPROVAL {
		t.Fatalf("tool call = %v", call)
	}
	if decision == nil || !decision.GetApproved() || decision.GetReason() != "fixture approval" || approver.GetPrincipal() != "user://approver" {
		t.Fatalf("approval decision = %v", decision)
	}
	if receipt == nil || receipt.GetOutput().AsMap()["receipt"] != "r-1" || receipt.GetApprovalRequestSeq() != 4 || receipt.GetApprovalDecisionSeq() != 6 {
		t.Fatalf("tool receipt = %v", receipt)
	}
	if usage == nil || usage.GetModel() != fixtureModelID || usage.GetInputTokens() != 11 || usage.GetReasoningTokens() != 3 {
		t.Fatalf("usage = %v", usage)
	}

	// The registry row decodes, and its digest is still the digest of the canonical spec.
	reg, err := h.registry.GetHarness(ctx, &v1.GetHarnessRequest{Name: fixtureRemote})
	if err != nil {
		t.Fatal(err)
	}
	if reg.GetState() != v1.HarnessState_HARNESS_STATE_RETIRED || reg.GetRetireReason() != fixtureRetireBy || !proto.Equal(reg.GetSpec(), fixtureSpec()) {
		t.Fatalf("registration = %v", reg)
	}
	canonical, err := canon.Proto(fixtureSpec())
	if err != nil {
		t.Fatal(err)
	}
	if sum := sha256.Sum256(canonical); reg.GetSpecDigest() != "sha256:"+hex.EncodeToString(sum[:]) {
		t.Fatalf("spec digest %s no longer matches the canonical spec %s", reg.GetSpecDigest(), canonical)
	}

	// Replay re-executes each live session from its recorded effects: the model and tool executor
	// must not run, and each recorded model-input hash must match what the harness asks for now. A
	// controller takes the writer fence, which a deleted session refuses, so the tombstone is checked
	// through the raw Replay above instead.
	noModel := func(context.Context, api.ModelRequest) (api.ModelResponse, error) {
		return api.ModelResponse{}, errors.New("replay invoked the model")
	}
	noTool := func(context.Context, controller.ToolCallContext, api.ToolCall) (api.ToolResult, error) {
		return api.ToolResult{}, errors.New("replay invoked the tool executor")
	}
	for _, tc := range []struct {
		name    string
		harness api.Harness
		want    []string
	}{
		{"parent", echoagent.Harness{}, []string{"echo:hello", "echo:again"}},
		{"child", echoagent.Harness{}, []string{"echo:hello", "echo:again", "echo:from child"}},
		{"gate", fixtureGateHarness{}, []string{"charged card 4242"}},
	} {
		uid := byName[tc.name].GetMetadata().GetUid()
		c, err := controller.New(store.Session(uid), noModel, controller.WithSessionUID(uid), controller.WithHarness(byName[tc.name].GetHarness()), controller.WithToolExecutor(noTool))
		if err != nil {
			t.Fatal(err)
		}
		got, err := c.Replay(ctx, tc.harness)
		if err != nil || !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("replay %s = %q, %v; want %q", tc.name, got, err, tc.want)
		}
	}

	// This build continues the old chain: a new turn appends and the chain still verifies.
	puid := parent.GetMetadata().GetUid()
	h.exec(t, puid, "after upgrade")
	after, err := h.sessions.GetSession(ctx, &v1.GetSessionRequest{Uid: puid})
	if err != nil {
		t.Fatal(err)
	}
	if after.GetLastSeq() != parent.GetLastSeq()+5 {
		t.Fatalf("new turn ends at %d, want %d", after.GetLastSeq(), parent.GetLastSeq()+5)
	}
	if err := store.Session(puid).Verify(); err != nil {
		t.Fatal(err)
	}
}

func concat(parts ...[]string) []string {
	var out []string
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// sessionUIDOf returns the UID of the session named name in the journal at path.
func sessionUIDOf(t *testing.T, path, name string) string {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var uid string
	if err := db.QueryRow("SELECT session FROM sessions WHERE name = ?", name).Scan(&uid); err != nil {
		t.Fatalf("session %q: %v", name, err)
	}
	return uid
}

// execCode runs a turn on uid and returns the status code it ends with.
func execCode(t *testing.T, h fixtureHost, uid, input string) codes.Code {
	t.Helper()
	stream, err := h.sessions.Exec(t.Context(), &v1.ExecRequest{Session: uid, Inputs: []*v1.Message{
		{Role: "user", Parts: []*v1.Part{{Part: &v1.Part_Text{Text: &v1.TextPart{Text: input}}}}},
	}})
	for err == nil {
		_, err = stream.Recv()
	}
	if errors.Is(err, io.EOF) {
		return codes.OK
	}
	return status.Code(err)
}

func userVersionOf(t *testing.T, path string) int {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var v int
	if err := db.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}
