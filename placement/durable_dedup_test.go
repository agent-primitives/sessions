package placement_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/agent-primitives/sessions/api"
	"github.com/agent-primitives/sessions/controller"
	"github.com/agent-primitives/sessions/placement"
	"github.com/agent-primitives/sessions/sqlitelog"
)

// declareAllTools declares durable dedup for every tool, keeping the Resume re-drive that tests
// written before WithDurableDedup exercise.
func declareAllTools(string) bool { return true }

var dedupAllTools = placement.WithDurableDedup(declareAllTools)

// A Placer passes its declaration to the controller: without one (or with the tool undeclared) a
// trailing intent is refused before the executor runs and without the RESUME marker, and the
// declared tool is re-driven under its original key.
func TestPlacerResumeRedrivesTrailingIntentOnlyForDeclaredTools(t *testing.T) {
	for _, tc := range []struct {
		name    string
		opts    []placement.Option
		refused bool
	}{
		{"undeclared", nil, true},
		{"other tool declared", []placement.Option{placement.WithDurableDedup(func(tool string) bool { return tool == "refund" })}, true},
		{"declared", []placement.Option{placement.WithDurableDedup(func(tool string) bool { return tool == "charge" })}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, err := sqlitelog.Open(":memory:")
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			log := store.Session("terminal-intent")
			seedToolPrefix(t, log, false)
			before, err := log.Read(1)
			if err != nil {
				t.Fatal(err)
			}
			var calls []api.ToolCall
			opts := append([]placement.Option{placement.WithToolExecutor(func(_ context.Context, _ controller.ToolCallContext, call api.ToolCall) (api.ToolResult, error) {
				calls = append(calls, call)
				return api.ToolResult{Output: map[string]any{"receipt": "recovered"}}, nil
			})}, tc.opts...)
			p := newLocalPlacer(t, hostToolHarness{key: "original-key"}, opts...)
			err = p.Resume(context.Background(), log, "terminal-intent")
			if tc.refused {
				if !errors.Is(err, controller.ErrUnansweredToolCall) || len(calls) != 0 {
					t.Fatalf("Resume = %v with %d executor calls; want ErrUnansweredToolCall and none", err, len(calls))
				}
				after, err := log.Read(1)
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(before, after) {
					t.Fatalf("refused Resume changed the journal: %d records before, %d after", len(before), len(after))
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(calls) != 1 || !reflect.DeepEqual(calls[0], hostToolCall("original-key")) {
				t.Fatalf("executor calls = %+v, want the recorded call once", calls)
			}
			assertToolKinds(t, log, api.EventExecutionStart, api.EventInput, api.EventToolCall, api.EventToolResult, api.EventOutput, api.EventEnd, api.EventLifecycle)
		})
	}
}
