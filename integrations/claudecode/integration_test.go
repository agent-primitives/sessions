package claudecode

import (
	"context"
	"net"
	"slices"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	"github.com/agent-primitives/sessions/api"
	v1 "github.com/agent-primitives/sessions/api/genpb"
	"github.com/agent-primitives/sessions/controller"
	"github.com/agent-primitives/sessions/eventlog"
	"github.com/agent-primitives/sessions/harnesswire"
)

// TestControllerOverWire drives the harness the way a deployment does: the controller runs each
// turn over Harness.Connect against the harness served in a gRPC server. It stands in for the
// conformance suite, which exercises replay determinism and mediated harnesses; this harness has
// no replay and the local runtime refuses the memory-snapshot tier.
//
// It shows the host accepts an unmediated harness's journal as is: OUTPUT, TOOL_RESULT and USAGE
// with no MODEL_CALL before them, and a failed turn's fixed error text as ERROR.
func TestControllerOverWire(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t, Config{})
	log, store, exec := wireController(t, h, "claude-test-model-1")

	// First turn: tools, output and usage.
	recs, err := exec("tool_turn.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := kinds(recs), []api.EventKind{
		api.EventExecutionStart, api.EventInput,
		api.EventToolResult, api.EventToolResult, api.EventToolResult, api.EventToolResult, api.EventToolResult,
		api.EventUsage, api.EventOutput, api.EventEnd,
	}; !slices.Equal(got, want) {
		t.Fatalf("turn 1 kinds = %v, want %v", got, want)
	}
	if m := recs[0].Event.ExecutionStart.Model; m != "claude-test-model-1" {
		t.Fatalf("EXECUTION_START.Model = %q", m)
	}

	// Second turn resumes the same Claude Code session.
	recs, err = exec("text_turn.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := kinds(recs), []api.EventKind{
		api.EventExecutionStart, api.EventInput, api.EventUsage, api.EventOutput, api.EventEnd,
	}; !slices.Equal(got, want) {
		t.Fatalf("turn 2 kinds = %v, want %v", got, want)
	}
	if out := recs[3].Event.Message.Parts[0].Text.Text; out != "Hello, world." {
		t.Fatalf("OUTPUT = %q", out)
	}
	got := starts(t, h)
	if len(got) != 2 || !slices.Contains(got[1].Args, "--resume") || !slices.Contains(got[1].Args, sessionID("uid", 0)) {
		t.Fatalf("turn 2 did not resume the session: %+v", got)
	}
	if !slices.Contains(got[0].Args, "claude-test-model-1") {
		t.Fatalf("turn 1 args = %q, want the policy model", got[0].Args)
	}

	// A failed turn is journaled as ERROR with the harness's fixed text.
	recs, err = exec("error_result.jsonl")
	if err == nil {
		t.Fatal("failed turn returned nil")
	}
	last := recs[len(recs)-1].Event
	if last.Kind != api.EventError || last.Err.Description != "harnesswire: remote harness ended FAILED: claudecode: claude reported an error result (subtype error_max_turns)" {
		t.Fatalf("last event = %s %+v", last.Kind, last.Err)
	}

	for _, r := range log.Read(1) {
		if r.Event.Kind == api.EventModelCall || r.Event.Kind == api.EventToolCall {
			t.Fatalf("journal has %s from an unmediated harness", r.Event.Kind)
		}
	}
	if err := store.Verify(); err != nil {
		t.Fatal(err)
	}
}

// wireController serves h over gRPC and returns a controller-backed exec for session "uid": each
// call sets the fake's scenario to fixture and runs one Exec through Harness.Connect. model is the
// policy's only model.
func wireController(t *testing.T, h *Harness, model string) (*eventlog.Log, eventlog.Store, func(fixture string) ([]eventlog.Record, error)) {
	t.Helper()
	log := eventlog.New()
	store := eventlog.AsStore(log)
	return log, store, wireExec(t, h, serveWire(t, h), log, "uid", model)
}

// serveWire serves h in a gRPC server and returns the host's client for it.
func serveWire(t *testing.T, h *Harness) api.Harness {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	v1.RegisterHarnessServer(srv, harnesswire.NewServer(h))
	go srv.Serve(lis)
	t.Cleanup(srv.Stop)
	conn, err := grpc.NewClient(
		"passthrough:///harness",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return harnesswire.NewClientHarness(v1.NewHarnessClient(conn))
}

// wireExec returns an exec for session uid's journal log, run over remote by a controller.
func wireExec(t *testing.T, h *Harness, remote api.Harness, log *eventlog.Log, uid, model string) func(fixture string) ([]eventlog.Record, error) {
	t.Helper()
	c, err := controller.New(eventlog.AsStore(log), nil,
		controller.WithSessionUID(uid),
		controller.WithModelPolicy(api.ModelPolicy{Default: model, Allowed: []string{model}}),
	)
	if err != nil {
		t.Fatal(err)
	}
	return func(fixture string) ([]eventlog.Record, error) {
		setScenario(t, h, scenario{Fixture: fixture})
		head := log.Head()
		err := c.Exec(context.Background(), remote, []api.Message{*api.TextMessage("user", "go")}, head)
		return log.Read(head + 1), err
	}
}

// TestForkOverWire: a child forked from a real parent journal (controller.Fork) and driven over
// harnesswire under its own UID gets the FORK marker in its Start.History, so its first turn forks
// the parent's Claude Code session into its own; the parent still resumes its own session.
func TestForkOverWire(t *testing.T) {
	t.Parallel()
	const model = "claude-test-model-1"
	h := newTestHarness(t, Config{})
	remote := serveWire(t, h)
	parentLog := eventlog.New()
	parent := wireExec(t, h, remote, parentLog, "parent", model)
	if _, err := parent("tool_turn.jsonl"); err != nil {
		t.Fatal(err)
	}

	childLog := eventlog.New()
	if err := controller.Fork(eventlog.AsStore(parentLog), eventlog.AsStore(childLog), parentLog.Head()); err != nil {
		t.Fatal(err)
	}
	child := wireExec(t, h, remote, childLog, "child", model)
	for i, want := range [][]string{
		{"--resume", sessionID("parent", 0), "--fork-session", "--session-id", sessionID("child", 0)},
		{"--resume", sessionID("child", 0)},
	} {
		recs, err := child("text_turn.jsonl")
		if err != nil {
			t.Fatalf("child turn %d: %v", i+1, err)
		}
		if got, wantKinds := kinds(recs), []api.EventKind{
			api.EventExecutionStart, api.EventInput, api.EventUsage, api.EventOutput, api.EventEnd,
		}; !slices.Equal(got, wantKinds) {
			t.Fatalf("child turn %d kinds = %v, want %v", i+1, got, wantKinds)
		}
		got := starts(t, h)
		args := got[len(got)-1].Args
		if j := slices.Index(args, want[0]); j < 0 || !slices.Equal(args[j:j+len(want)], want) {
			t.Fatalf("child turn %d args = %q, want %q", i+1, args, want)
		}
	}

	if _, err := parent("text_turn.jsonl"); err != nil {
		t.Fatal(err)
	}
	got := starts(t, h)
	if args := got[len(got)-1].Args; slices.Contains(args, "--fork-session") || !slices.Contains(args, sessionID("parent", 0)) {
		t.Fatalf("parent turn args = %q, want --resume %s", args, sessionID("parent", 0))
	}
	for _, log := range []*eventlog.Log{parentLog, childLog} {
		if err := eventlog.AsStore(log).Verify(); err != nil {
			t.Fatal(err)
		}
	}
}

func kinds(recs []eventlog.Record) []api.EventKind {
	out := make([]api.EventKind, len(recs))
	for i, r := range recs {
		out[i] = r.Event.Kind
	}
	return out
}
