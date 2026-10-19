package session_test

import (
	"context"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/agent-primitives/sessions/api"
	v1 "github.com/agent-primitives/sessions/api/genpb"
	"github.com/agent-primitives/sessions/controller"
	"github.com/agent-primitives/sessions/placement"
	"github.com/agent-primitives/sessions/runtime/local"
)

// dedupAllTools declares durable dedup for every tool, keeping the Resume re-drive that tests
// written before WithDurableDedup exercise.
var dedupAllTools = placement.WithDurableDedup(func(string) bool { return true })

// A trailing intent whose executor declares no durable dedup is a caller recovery precondition:
// Resume is FAILED_PRECONDITION with the refusal and its guidance, runs no executor and leaves the
// journal unchanged, and a new Exec still runs its own first attempt.
func TestResumeUndeclaredToolIntentFailedPrecondition(t *testing.T) {
	store := openStore(t, ":memory:")
	backend := local.New(registryToolHarness{key: "original-service-key"})
	t.Cleanup(func() { _ = backend.Close() })
	var effects atomic.Int32
	client := serveRegistry(t, store, echoRegistry(t, backend, placement.WithToolExecutor(
		func(_ context.Context, scope controller.ToolCallContext, _ api.ToolCall) (api.ToolResult, error) {
			effects.Add(1)
			return api.ToolResult{Output: map[string]any{"receipt": scope.SessionUID}}, nil
		},
	)))
	uid := mustCreate(t, client)
	log := store.Session(uid)
	fence, err := log.NewFence()
	if err != nil {
		t.Fatal(err)
	}
	call := api.ToolCall{ID: "service-call", Tool: "charge", Args: map[string]any{"account": "a1"}, Mediation: api.MediationControllerMediated, IdempotencyKey: "original-service-key"}
	for i, ev := range []api.Event{
		{Kind: api.EventExecutionStart, ExecutionStart: &api.ExecutionStart{InputCount: proto.Int64(1)}},
		{Kind: api.EventInput, Message: api.TextMessage("user", "charge")},
		{Kind: api.EventToolCall, ToolCall: &call},
	} {
		ev.ExecutionID = "service-interrupted"
		if _, err := log.Append(int64(i), fence, ev); err != nil {
			t.Fatal(err)
		}
	}
	before, err := log.Read(1)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Resume(t.Context(), &v1.ResumeRequest{Session: uid})
	if status.Code(err) != codes.FailedPrecondition {
		t.Errorf("undeclared intent Resume = %v, want FailedPrecondition", err)
	}
	message := status.Convert(err).Message()
	for _, want := range []string{"tool call has no recorded result; repeating it could repeat the action", "declares durable dedup", "Exec a new turn"} {
		if !strings.Contains(message, want) {
			t.Errorf("Resume message %q lacks %q", message, want)
		}
	}
	after, err := log.Read(1)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("refused Resume changed the journal: %v", err)
	}
	if got := effects.Load(); got != 0 {
		t.Fatalf("refused Resume ran the executor %d times", got)
	}
	if outputs := execOutputs(t, client, uid, "new charge", int64(len(before))); !reflect.DeepEqual(outputs, []string{"service-call:" + uid}) {
		t.Fatalf("new Exec outputs = %v", outputs)
	}
	if got := effects.Load(); got != 1 {
		t.Fatalf("new Exec effects=%d, want its own first attempt", got)
	}
}

// The journal check (CHAIN-12) runs before the durable dedup decision (TOOL-10): a trailing intent
// whose TOOL_CALL changed in storage is DATA_LOSS on Resume whether or not its tool is declared,
// with no executor call and no append, rather than a re-drive of the changed call or the
// undeclared-tool FAILED_PRECONDITION. The untampered journal is re-driven or refused as usual.
func TestResumeVerifiesJournalBeforeToolIntentRedrive(t *testing.T) {
	for _, tc := range []struct {
		name               string
		declared, tampered bool
		want               codes.Code
		wantEffects        int32
	}{
		{name: "declared", declared: true, want: codes.OK, wantEffects: 1},
		{name: "undeclared", want: codes.FailedPrecondition},
		{name: "declared tampered", declared: true, tampered: true, want: codes.DataLoss},
		{name: "undeclared tampered", tampered: true, want: codes.DataLoss},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "journal.db")
			store := openStore(t, path)
			db := deletionSQL(t, path)
			backend := local.New(registryToolHarness{key: "original-service-key"})
			t.Cleanup(func() { _ = backend.Close() })
			var effects atomic.Int32
			opts := []placement.Option{placement.WithToolExecutor(
				func(_ context.Context, scope controller.ToolCallContext, call api.ToolCall) (api.ToolResult, error) {
					effects.Add(1)
					if !reflect.DeepEqual(call.Args, map[string]any{"account": "a1"}) {
						t.Errorf("executor received args %v, want the call as first recorded", call.Args)
					}
					return api.ToolResult{Output: map[string]any{"receipt": scope.SessionUID}}, nil
				},
			)}
			if tc.declared {
				opts = append(opts, dedupAllTools)
			}
			client := serveRegistry(t, store, echoRegistry(t, backend, opts...))
			uid := mustCreate(t, client)
			log := store.Session(uid)
			fence, err := log.NewFence()
			if err != nil {
				t.Fatal(err)
			}
			call := api.ToolCall{ID: "service-call", Tool: "charge", Args: map[string]any{"account": "a1"}, Mediation: api.MediationControllerMediated, IdempotencyKey: "original-service-key"}
			for i, ev := range []api.Event{
				{Kind: api.EventExecutionStart, ExecutionStart: &api.ExecutionStart{InputCount: proto.Int64(1)}},
				{Kind: api.EventInput, Message: api.TextMessage("user", "charge")},
				{Kind: api.EventToolCall, ToolCall: &call},
			} {
				ev.ExecutionID = "service-interrupted"
				if _, err := log.Append(int64(i), fence, ev); err != nil {
					t.Fatal(err)
				}
			}
			if tc.tampered {
				tamperEvent(t, db, uid, 3, func(ev *api.Event) {
					if ev.Kind != api.EventToolCall {
						t.Fatalf("seq 3 is %v, want TOOL_CALL", ev.Kind)
					}
					ev.ToolCall.Args = map[string]any{"account": "attacker"}
				})
			}
			before := journalRows(t, db, uid)
			_, err = client.Resume(t.Context(), &v1.ResumeRequest{Session: uid})
			if status.Code(err) != tc.want {
				t.Fatalf("Resume = %v, want %v", err, tc.want)
			}
			if got := effects.Load(); got != tc.wantEffects {
				t.Fatalf("Resume ran the executor %d times, want %d", got, tc.wantEffects)
			}
			if after := journalRows(t, db, uid); tc.want != codes.OK && !reflect.DeepEqual(after, before) {
				t.Fatalf("refused Resume changed the journal: %d records -> %d", len(before), len(after))
			}
		})
	}
}
