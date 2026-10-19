package controller_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/agent-primitives/sessions/api"
	v1 "github.com/agent-primitives/sessions/api/genpb"
	"github.com/agent-primitives/sessions/controller"
	"github.com/agent-primitives/sessions/eventlog"
	"github.com/agent-primitives/sessions/sqlitelog"
	"github.com/agent-primitives/sessions/wire"
)

// dedupAll declares durable dedup for every tool, keeping the Resume re-drive that tests written
// before WithDurableDedup exercise.
var dedupAll = controller.WithDurableDedup(func(string) bool { return true })

// receiptCrashStore simulates a host that dies after the executor applied a call's effect but
// before its TOOL_RESULT became durable: that append and every later one fail, so the journal ends
// at the TOOL_CALL, without even the best-effort ERROR.
type receiptCrashStore struct {
	eventlog.Store
	callID  string
	crashed bool
}

func (s *receiptCrashStore) Append(expectedLastSeq, fence int64, ev api.Event) (eventlog.Record, error) {
	if !s.crashed && ev.Kind == api.EventToolResult && ev.Result != nil && ev.Result.ID == s.callID {
		s.crashed = true
	}
	if s.crashed {
		return eventlog.Record{}, errCrashed
	}
	return s.Store.Append(expectedLastSeq, fence, ev)
}

// countingCallHarness counts how often the controller reaches the harness.
type countingCallHarness struct {
	callHarness
	describes, runs int
}

func (h *countingCallHarness) Describe(ctx context.Context) (api.Descriptor, error) {
	h.describes++
	return h.callHarness.Describe(ctx)
}

func (h *countingCallHarness) Run(ctx context.Context, start *api.Start, sink api.EventSink) error {
	h.runs++
	return h.callHarness.Run(ctx, start, sink)
}

func dedupCalls() []api.ToolCall {
	return []api.ToolCall{
		{ID: "call-lookup", Tool: "lookup", Mediation: api.MediationControllerMediated, IdempotencyKey: "key-lookup"},
		{ID: "call-send", Tool: "send", Mediation: api.MediationControllerMediated, IdempotencyKey: "key-send"},
	}
}

// crashBetweenCallAndResult runs a turn that calls lookup then send, with no durable dedup
// declaration, and crashes after crashCallID's executor ran but before its result was journaled.
// The executor applied that effect once; the journal ends with its unanswered TOOL_CALL.
func crashBetweenCallAndResult(t *testing.T, crashCallID string, effects map[string]int) eventlog.Store {
	t.Helper()
	log := memStore(t)
	executor := func(_ context.Context, _ controller.ToolCallContext, call api.ToolCall) (api.ToolResult, error) {
		effects[call.Tool]++
		return api.ToolResult{Output: map[string]any{"text": call.Tool}}, nil
	}
	crash := &receiptCrashStore{Store: log, callID: crashCallID}
	c, err := controller.New(crash, echoModel, controller.WithSessionUID("s"), controller.WithToolExecutor(executor))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Exec(t.Context(), &callHarness{calls: dedupCalls()}, []api.Message{msg("go")}, 0); !errors.Is(err, errCrashed) {
		t.Fatalf("Exec = %v, want the simulated crash", err)
	}
	recs, err := log.Read(1)
	if err != nil {
		t.Fatal(err)
	}
	last := recs[len(recs)-1].Event
	if last.Kind != api.EventToolCall || last.ToolCall.ID != crashCallID {
		t.Fatalf("journal ends with %s, want the unanswered TOOL_CALL %q", last.Kind, crashCallID)
	}
	return log
}

// Resume after a crash between TOOL_CALL and TOOL_RESULT re-runs the call only when the executor
// declares durable dedup for that tool. Otherwise it refuses before Describe, Run or any append,
// and the untouched journal can still be recovered later by a declaring executor. A first attempt
// of an undeclared tool, including one Resume reaches after a re-drive, is unaffected.
func TestResumeRedrivesTrailingToolCallOnlyWhenDeclared(t *testing.T) {
	declare := func(tools ...string) []controller.Option {
		return []controller.Option{controller.WithDurableDedup(func(tool string) bool {
			for _, declared := range tools {
				if tool == declared {
					return true
				}
			}
			return false
		})}
	}
	for _, tc := range []struct {
		name        string
		crashCallID string
		declaration []controller.Option
		refused     bool
		wantEffects map[string]int // after Resume, including the original attempt
	}{
		{"declared", "call-send", []controller.Option{dedupAll}, false, map[string]int{"lookup": 1, "send": 2}},
		{"undeclared", "call-send", nil, true, map[string]int{"lookup": 1, "send": 1}},
		{"false", "call-send", []controller.Option{controller.WithDurableDedup(func(string) bool { return false })}, true, map[string]int{"lookup": 1, "send": 1}},
		{"nil predicate", "call-send", []controller.Option{controller.WithDurableDedup(nil)}, true, map[string]int{"lookup": 1, "send": 1}},
		{"mixed, undeclared tool trailing", "call-send", declare("lookup"), true, map[string]int{"lookup": 1, "send": 1}},
		{"mixed, declared tool trailing", "call-lookup", declare("lookup"), false, map[string]int{"lookup": 2, "send": 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			effects := map[string]int{}
			log := crashBetweenCallAndResult(t, tc.crashCallID, effects)
			before, err := log.Read(1)
			if err != nil {
				t.Fatal(err)
			}
			var redriven []controller.ToolCallContext
			executor := func(_ context.Context, scope controller.ToolCallContext, call api.ToolCall) (api.ToolResult, error) {
				effects[call.Tool]++
				redriven = append(redriven, scope)
				return api.ToolResult{Output: map[string]any{"text": call.Tool}}, nil
			}
			opts := append([]controller.Option{controller.WithSessionUID("s"), controller.WithToolExecutor(executor)}, tc.declaration...)
			c, err := controller.New(log, echoModel, opts...)
			if err != nil {
				t.Fatal(err)
			}
			h := &countingCallHarness{callHarness: callHarness{calls: dedupCalls()}}
			resumed, err := c.Resume(t.Context(), h)
			if !reflect.DeepEqual(effects, tc.wantEffects) {
				t.Fatalf("executor effects = %v, want %v", effects, tc.wantEffects)
			}
			after, readErr := log.Read(1)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if tc.refused {
				if resumed || !errors.Is(err, controller.ErrUnansweredToolCall) {
					t.Fatalf("Resume = %v, %v; want refusal with ErrUnansweredToolCall", resumed, err)
				}
				const want = "tool call has no recorded result; repeating it could repeat the action"
				if !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), `"send"`) {
					t.Fatalf("refusal %q must state %q and name the tool", err, want)
				}
				if h.describes != 0 || h.runs != 0 {
					t.Fatalf("refusal reached the harness: describes=%d runs=%d", h.describes, h.runs)
				}
				if !reflect.DeepEqual(before, after) {
					t.Fatalf("refusal changed the journal: %d records before, %d after", len(before), len(after))
				}
				// The refusal leaves the intent recoverable: declaring the tool re-drives it once.
				declared, err := controller.New(log, echoModel, controller.WithSessionUID("s"), controller.WithToolExecutor(executor), dedupAll)
				if err != nil {
					t.Fatal(err)
				}
				if resumed, err := declared.Resume(t.Context(), &callHarness{calls: dedupCalls()}); !resumed || err != nil {
					t.Fatalf("declared Resume after refusal = %v, %v", resumed, err)
				}
				if effects["send"] != 2 || effects["lookup"] != 1 {
					t.Fatalf("declared Resume effects = %v, want send re-driven once and lookup served", effects)
				}
				return
			}
			if !resumed || err != nil {
				t.Fatalf("Resume = %v, %v; want a completed re-drive", resumed, err)
			}
			for _, scope := range redriven {
				if scope.SessionUID != "s" {
					t.Fatalf("re-drive scope = %+v, want the recorded session", scope)
				}
			}
			if got := after[len(after)-1].Event; got.Kind != api.EventEnd {
				t.Fatalf("journal ends with %s, want END", got.Kind)
			}
			if err := log.Verify(); err != nil {
				t.Fatal(err)
			}
			if resumed, err := c.Resume(t.Context(), h); resumed || err != nil {
				t.Fatalf("completed Resume = %v, %v; want a no-op", resumed, err)
			}
		})
	}
}

// Without a tool executor there is no action to repeat, so Resume keeps its previous behavior: the
// re-issued call fails as on the live path, not with ErrUnansweredToolCall.
func TestResumeWithoutToolExecutorDoesNotRefuseTrailingToolCall(t *testing.T) {
	log := crashBetweenCallAndResult(t, "call-send", map[string]int{})
	c, err := controller.New(log, echoModel, controller.WithSessionUID("s"))
	if err != nil {
		t.Fatal(err)
	}
	resumed, err := c.Resume(t.Context(), &callHarness{calls: dedupCalls()})
	if !resumed || err == nil || errors.Is(err, controller.ErrUnansweredToolCall) || !strings.Contains(err.Error(), "no tool executor configured") {
		t.Fatalf("Resume = %v, %v; want the live missing-executor failure", resumed, err)
	}
}

// Resume verifies the journal's hash chain (CHAIN-12) before it decides whether to re-drive a
// trailing tool intent (TOOL-10): when the unanswered TOOL_CALL changed in storage, a declaring
// executor is never called with the changed call and an undeclared tool gets ErrJournalTampered
// rather than ErrUnansweredToolCall, both before Describe, Run or any append. The same journal
// untampered is re-driven under the declaration and refused without it.
func TestResumeVerifiesJournalBeforeToolIntentRedrive(t *testing.T) {
	for _, tc := range []struct {
		name               string
		declared, tampered bool
		wantErr            error
		wantEffects        int
	}{
		{name: "declared", declared: true, wantEffects: 1},
		{name: "undeclared", wantErr: controller.ErrUnansweredToolCall},
		{name: "declared tampered", declared: true, tampered: true, wantErr: controller.ErrJournalTampered},
		{name: "undeclared tampered", tampered: true, wantErr: controller.ErrJournalTampered},
	} {
		t.Run(tc.name, func(t *testing.T) {
			records, err := crashBetweenCallAndResult(t, "call-send", map[string]int{}).Read(1)
			if err != nil {
				t.Fatal(err)
			}
			// The harness re-issues what the journal now says, as one rebuilding its turn from the
			// changed History would, so only the chain check stands between the changed call and the
			// executor.
			calls := dedupCalls()
			if tc.tampered {
				calls[1].Args = map[string]any{"to": "attacker"}
			}
			log := sqliteJournal(t, records, func(seq int64, ev *api.Event) {
				if tc.tampered && seq == records[len(records)-1].Seq {
					ev.ToolCall.Args = calls[1].Args
				}
			})
			before, err := log.Read(1)
			if err != nil {
				t.Fatal(err)
			}
			effects := 0
			executor := func(_ context.Context, _ controller.ToolCallContext, call api.ToolCall) (api.ToolResult, error) {
				effects++
				if call.Args != nil {
					t.Errorf("executor received args %v, want the call as first recorded", call.Args)
				}
				return api.ToolResult{Output: map[string]any{"text": call.Tool}}, nil
			}
			opts := []controller.Option{controller.WithSessionUID("s"), controller.WithToolExecutor(executor)}
			if tc.declared {
				opts = append(opts, dedupAll)
			}
			c, err := controller.New(log, echoModel, opts...)
			if err != nil {
				t.Fatal(err)
			}
			h := &countingCallHarness{callHarness: callHarness{calls: calls}}
			resumed, err := c.Resume(t.Context(), h)
			if effects != tc.wantEffects {
				t.Fatalf("executor ran %d times, want %d", effects, tc.wantEffects)
			}
			if tc.wantErr == nil {
				if !resumed || err != nil {
					t.Fatalf("Resume = %v, %v; want a completed re-drive", resumed, err)
				}
				return
			}
			if resumed || !errors.Is(err, tc.wantErr) {
				t.Fatalf("Resume = %v, %v; want refusal with %v", resumed, err, tc.wantErr)
			}
			if h.describes != 0 || h.runs != 0 {
				t.Fatalf("refusal reached the harness: describes=%d runs=%d", h.describes, h.runs)
			}
			after, err := log.Read(1)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(after, before) {
				t.Fatalf("refusal changed the journal: %d records -> %d", len(before), len(after))
			}
		})
	}
}

// sqliteJournal copies records into a SQLite journal, applies change to each stored event without
// touching its stored hashes (as a bad manual edit or a corrupting migration would), and reopens it.
func sqliteJournal(t *testing.T, records []eventlog.Record, change func(seq int64, ev *api.Event)) eventlog.Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "journal.db")
	store, err := sqlitelog.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	log := store.Session("session")
	var fence int64
	for _, record := range records {
		for fence < record.Fence {
			if fence, err = log.NewFence(); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := log.Append(record.Seq-1, fence, record.Event); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, record := range records {
		var blob []byte
		if err := db.QueryRow(`SELECT event FROM events WHERE session = ? AND seq = ?`, "session", record.Seq).Scan(&blob); err != nil {
			t.Fatal(err)
		}
		pb := &v1.Event{}
		if err := proto.Unmarshal(blob, pb); err != nil {
			t.Fatal(err)
		}
		ev := wire.EventFromProto(pb)
		change(record.Seq, &ev)
		if blob, err = proto.Marshal(wire.EventToProto(ev)); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`UPDATE events SET event = ? WHERE session = ? AND seq = ?`, blob, "session", record.Seq); err != nil {
			t.Fatal(err)
		}
	}
	store, err = sqlitelog.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store.Session("session")
}
