package claudecode

import (
	"context"
	"reflect"
	"slices"
	"testing"

	"github.com/agent-primitives/sessions/api"
)

// The recorded fixtures are the stream-json output of the Claude Code version the harness is
// pinned to (testdata/README.md): a turn that runs one Bash tool, then a turn that resumes it.

// TestRecordedTurnAndResume replays the recorded pair through the harness parser.
func TestRecordedTurnAndResume(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t, Config{PermissionMode: "acceptEdits"})
	ctx := context.Background()
	id := sessionID("uid", 0)

	setScenario(t, h, scenario{Fixture: "recorded_tool_turn.jsonl"})
	sink := &recordingSink{t: t}
	if err := h.Run(ctx, startFor("uid", "e1", nil, "list the files"), sink); err != nil {
		t.Fatal(err)
	}
	wantReports := []api.ToolResult{{
		ID: "toolu_01JJHyxBpd853yWTaNCKQ2M1",
		Output: map[string]any{
			"tool":        "Bash",
			"input":       `{"command":"ls","description":"List files in current directory"}`,
			"input_bytes": float64(len(`{"command":"ls","description":"List files in current directory"}`)),
			"truncated":   false,
		},
		Content: []api.Part{{Text: &api.TextPart{Text: "notes.txt"}}},
	}}
	if !reflect.DeepEqual(sink.reports, wantReports) {
		t.Fatalf("reports = %+v, want %+v", sink.reports, wantReports)
	}
	// Usage is the result's: uncached, cache-read and cache-write input.
	if want := []api.Usage{{Model: "claude-haiku-5-5", InputTokens: 4 + 28018 + 6378, OutputTokens: 81}}; !reflect.DeepEqual(sink.usages, want) {
		t.Fatalf("usages = %+v, want %+v", sink.usages, want)
	}
	if !slices.Equal(sink.outputs, []string{"notes.txt"}) {
		t.Fatalf("outputs = %q", sink.outputs)
	}

	setScenario(t, h, scenario{Fixture: "recorded_resume_turn.jsonl"})
	sink = &recordingSink{t: t}
	if err := h.Run(ctx, startFor("uid", "e2", priorTurn("e1"), "say done"), sink); err != nil {
		t.Fatal(err)
	}
	if want := []api.Usage{{Model: "claude-haiku-5-5", InputTokens: 2 + 17266 + 65, OutputTokens: 3}}; !reflect.DeepEqual(sink.usages, want) {
		t.Fatalf("usages = %+v, want %+v", sink.usages, want)
	}
	if !slices.Equal(sink.outputs, []string{"done"}) || len(sink.reports) != 0 {
		t.Fatalf("outputs = %q, reports = %+v", sink.outputs, sink.reports)
	}

	got := starts(t, h)
	if len(got) != 2 {
		t.Fatalf("spawned %d processes, want 2", len(got))
	}
	for i, flag := range []string{"--session-id", "--resume"} {
		args := got[i].Args
		if j := slices.Index(args, flag); j < 0 || args[j+1] != id {
			t.Fatalf("turn %d args = %q, want %s %s", i+1, args, flag, id)
		}
	}
}

// TestRecordedOverWire runs the recorded pair through the controller, as TestControllerOverWire
// does with the documented fixtures.
func TestRecordedOverWire(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t, Config{PermissionMode: "acceptEdits"})
	log, store, exec := wireController(t, h, "haiku")

	recs, err := exec("recorded_tool_turn.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := kinds(recs), []api.EventKind{
		api.EventExecutionStart, api.EventInput, api.EventToolResult, api.EventUsage, api.EventOutput, api.EventEnd,
	}; !slices.Equal(got, want) {
		t.Fatalf("turn 1 kinds = %v, want %v", got, want)
	}
	recs, err = exec("recorded_resume_turn.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := kinds(recs), []api.EventKind{
		api.EventExecutionStart, api.EventInput, api.EventUsage, api.EventOutput, api.EventEnd,
	}; !slices.Equal(got, want) {
		t.Fatalf("turn 2 kinds = %v, want %v", got, want)
	}
	if out := recs[3].Event.Message.Parts[0].Text.Text; out != "done" {
		t.Fatalf("OUTPUT = %q", out)
	}
	if got := starts(t, h); len(got) != 2 || !slices.Contains(got[1].Args, "--resume") {
		t.Fatalf("turn 2 did not resume: %+v", got)
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
