package toolagent_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/agent-primitives/sessions/api"
	"github.com/agent-primitives/sessions/controller"
	"github.com/agent-primitives/sessions/eventlog"
	"github.com/agent-primitives/sessions/harness/toolagent"
	"github.com/agent-primitives/sessions/sqlitelog"
)

var publish = api.ToolDefinition{
	Name:        "publish",
	Description: "Publish text.",
	InputSchema: map[string]any{"type": "object", "properties": map[string]any{"text": map[string]any{"type": "string"}}},
}

func TestDescribe(t *testing.T) {
	desc, err := (toolagent.Harness{Model: "m", Tools: []api.ToolDefinition{publish}}).Describe(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	want := api.Descriptor{
		ID: "tool", Version: "1", Models: []string{"m"},
		Tools:        []api.ToolSpec{{Name: "publish", Description: "Publish text.", Mediation: api.MediationRequiresApproval}},
		Capabilities: api.Capabilities{Resumability: api.ResumabilityStatelessReplay, ForkSafe: true},
	}
	if !reflect.DeepEqual(desc, want) {
		t.Fatalf("Describe = %+v, want %+v", desc, want)
	}
}

// scriptedModel calls publish("hello") with call ID "call_1" until the request carries its result,
// then answers with text naming the result.
type scriptedModel struct {
	requests []api.ModelRequest
}

func (m *scriptedModel) call(_ context.Context, req api.ModelRequest) (api.ModelResponse, error) {
	m.requests = append(m.requests, req)
	last := req.Messages[len(req.Messages)-1]
	if last.Role == "tool" {
		return api.ModelResponse{Message: *api.TextMessage("assistant", "done: "+last.Parts[0].ToolResult.Content[0].Text.Text)}, nil
	}
	return api.ModelResponse{Message: api.Message{Role: "assistant", Parts: []api.Part{
		{Text: &api.TextPart{Text: "Publishing."}},
		{ToolCall: &api.ToolCall{ID: "call_1", Tool: "publish", Args: map[string]any{"text": "hello"}}},
	}}}, nil
}

func newLog(t *testing.T) eventlog.Store {
	t.Helper()
	store, err := sqlitelog.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store.Session("tool")
}

// parkedTurn runs one turn to its park and returns the pending approval.
func parkedTurn(t *testing.T, log eventlog.Store, model *scriptedModel, h toolagent.Harness, input string) *api.ApprovalParkedError {
	t.Helper()
	c, err := controller.New(log, model.call, controller.WithSessionUID("s"))
	if err != nil {
		t.Fatal(err)
	}
	head, err := log.Head()
	if err != nil {
		t.Fatal(err)
	}
	err = c.Exec(t.Context(), h, []api.Message{*api.TextMessage("user", input)}, head)
	var park *api.ApprovalParkedError
	if !errors.As(err, &park) {
		t.Fatalf("Exec = %v, want a park", err)
	}
	return park
}

func TestApprovedCallRunsOnceAndFeedsTheModel(t *testing.T) {
	log := newLog(t)
	model := &scriptedModel{}
	h := toolagent.Harness{Model: "m", Tools: []api.ToolDefinition{publish}}
	park := parkedTurn(t, log, model, h, "publish hello")
	if park.Ref.ToolCallID != "call_1" {
		t.Fatalf("park = %+v", park.Ref)
	}
	if len(model.requests) != 1 || !reflect.DeepEqual(model.requests[0].Tools, h.Tools) {
		t.Fatalf("first request did not offer the tools: %+v", model.requests)
	}
	if _, err := controller.Approve(log, api.ApprovalDecision{ExecutionID: park.Ref.ExecutionID, ToolCallID: park.Ref.ToolCallID, RequestSeq: park.Ref.RequestSeq, Approved: true}); err != nil {
		t.Fatal(err)
	}
	var calls []api.ToolCall
	executor := func(_ context.Context, scope controller.ToolCallContext, call api.ToolCall) (api.ToolResult, error) {
		calls = append(calls, call)
		return api.ToolResult{Output: map[string]any{"text": "published"}}, nil
	}
	for range 2 { // the second Resume finds the turn complete
		c, err := controller.New(log, model.call, controller.WithSessionUID("s"), controller.WithToolExecutor(executor))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.Resume(t.Context(), h); err != nil {
			t.Fatal(err)
		}
	}
	want := api.ToolCall{ID: "call_1", Tool: "publish", Args: map[string]any{"text": "hello"}, Mediation: api.MediationRequiresApproval, IdempotencyKey: park.Ref.ExecutionID + "/call_1"}
	if len(calls) != 1 || !reflect.DeepEqual(calls[0], want) {
		t.Fatalf("executor calls = %+v, want one %+v", calls, want)
	}
	// Resume served the first completion from the journal and called the model once more.
	if len(model.requests) != 2 {
		t.Fatalf("model requests = %d, want 2", len(model.requests))
	}
	second := model.requests[1].Messages
	result := second[len(second)-1]
	if result.Role != "tool" || result.Parts[0].ToolResult.ID != "call_1" || result.Parts[0].ToolResult.Content[0].Text.Text != `{"text":"published"}` {
		t.Fatalf("tool result message = %+v", result)
	}
	if result.Parts[0].ToolResult.ApprovalRequestSeq != 0 || result.Parts[0].ToolResult.ApprovalDecisionSeq != 0 {
		t.Fatalf("receipt evidence leaked into the model request: %+v", result.Parts[0].ToolResult)
	}
	c, err := controller.New(log, nil, controller.WithSessionUID("s"))
	if err != nil {
		t.Fatal(err)
	}
	outputs, err := c.Replay(t.Context(), h)
	if err != nil {
		t.Fatal(err)
	}
	if got := outputs[len(outputs)-1]; got != `done: {"text":"published"}` {
		t.Fatalf("replayed outputs = %q", outputs)
	}

	// A later turn sees the earlier call and its result. The model reuses call ID call_1; the
	// harness keys it by the new execution, so the executor cannot mistake it for the first call.
	first := park.Ref.ExecutionID
	park = parkedTurn(t, log, model, h, "again")
	context := model.requests[len(model.requests)-1].Messages
	var roles []string
	for _, m := range context {
		roles = append(roles, m.Role)
	}
	if want := []string{"user", "assistant", "tool", "assistant", "user"}; !reflect.DeepEqual(roles, want) {
		t.Fatalf("second turn context roles = %v, want %v", roles, want)
	}
	state, err := controller.InspectApproval(log, 0)
	if err != nil || park.Ref.ExecutionID == first || state.Call.IdempotencyKey != park.Ref.ExecutionID+"/call_1" {
		t.Fatalf("second park = %+v, state %+v, %v", park.Ref, state, err)
	}
}

func TestDeniedCallReachesTheModelAsAnError(t *testing.T) {
	log := newLog(t)
	model := &scriptedModel{}
	h := toolagent.Harness{Model: "m", Tools: []api.ToolDefinition{publish}}
	park := parkedTurn(t, log, model, h, "publish hello")
	if _, err := controller.Approve(log, api.ApprovalDecision{ExecutionID: park.Ref.ExecutionID, ToolCallID: park.Ref.ToolCallID, RequestSeq: park.Ref.RequestSeq}); err != nil {
		t.Fatal(err)
	}
	c, err := controller.New(log, model.call, controller.WithSessionUID("s"), controller.WithToolExecutor(func(context.Context, controller.ToolCallContext, api.ToolCall) (api.ToolResult, error) {
		t.Error("denied call reached the executor")
		return api.ToolResult{}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Resume(t.Context(), h); err != nil {
		t.Fatal(err)
	}
	last := model.requests[len(model.requests)-1].Messages
	result := last[len(last)-1].Parts[0].ToolResult
	if !result.IsError || result.Content[0].Text.Text != "Tool execution denied by approval decision." {
		t.Fatalf("denial message = %+v", result)
	}
}

// A parked turn's unanswered call is not sent to the model on a later turn; a provider refuses an
// assistant tool call with no result.
func TestHistoryDropsUnansweredCalls(t *testing.T) {
	history := []api.Event{
		{ExecutionID: "e1", Kind: api.EventInput, Message: api.TextMessage("user", "q")},
		{ExecutionID: "e1", Kind: api.EventOutput, Message: &api.Message{Role: "assistant", Parts: []api.Part{
			{Text: &api.TextPart{Text: "calling"}},
			{ToolCall: &api.ToolCall{ID: "a", Tool: "publish"}},
			{ToolCall: &api.ToolCall{ID: "b", Tool: "publish"}},
		}}},
		{ExecutionID: "e1", Kind: api.EventToolResult, Result: &api.ToolResult{ID: "a", IsError: true, Error: "nope"}},
		{ExecutionID: "e2", Kind: api.EventOutput, Message: &api.Message{Role: "assistant", Parts: []api.Part{
			{ToolCall: &api.ToolCall{ID: "a", Tool: "publish"}}, // same model ID in another turn, never answered
		}}},
	}
	h := toolagent.Harness{Model: "m", Tools: []api.ToolDefinition{publish}}
	sink := &recordingSink{}
	if err := h.Run(t.Context(), &api.Start{ExecutionID: "e3", History: history}, sink); err != nil {
		t.Fatal(err)
	}
	got, _ := json.Marshal(sink.req.Messages)
	want, _ := json.Marshal([]api.Message{
		*api.TextMessage("user", "q"),
		{Role: "assistant", Parts: []api.Part{{Text: &api.TextPart{Text: "calling"}}, {ToolCall: &api.ToolCall{ID: "a", Tool: "publish"}}}},
		{Role: "tool", Parts: []api.Part{{ToolResult: &api.ToolResult{ID: "a", IsError: true, Content: api.TextMessage("tool", "Tool error: nope").Parts}}}},
	})
	if string(got) != string(want) {
		t.Fatalf("history messages\n got %s\nwant %s", got, want)
	}
}

func TestRefusesUngateableCallsAndEndlessLoops(t *testing.T) {
	h := toolagent.Harness{Model: "m", Tools: []api.ToolDefinition{publish}}
	for name, tc := range map[string]struct {
		parts []api.Part
		want  error
	}{
		"empty ID":     {[]api.Part{{ToolCall: &api.ToolCall{Tool: "publish"}}}, toolagent.ErrInvalidToolCall},
		"duplicate ID": {[]api.Part{{ToolCall: &api.ToolCall{ID: "x", Tool: "publish"}}, {ToolCall: &api.ToolCall{ID: "x", Tool: "publish"}}}, toolagent.ErrInvalidToolCall},
		"endless":      {nil, toolagent.ErrTooManyModelCalls},
	} {
		t.Run(name, func(t *testing.T) {
			sink := &loopSink{parts: tc.parts}
			if err := h.Run(t.Context(), &api.Start{ExecutionID: "e"}, sink); !errors.Is(err, tc.want) {
				t.Fatalf("Run = %v, want %v", err, tc.want)
			}
			if name == "endless" && sink.models != toolagent.MaxModelCalls {
				t.Fatalf("model calls = %d, want %d", sink.models, toolagent.MaxModelCalls)
			}
		})
	}
}

// loopSink returns parts from every model call, or a fresh call each time when parts is nil, and
// answers every tool call.
type loopSink struct {
	api.EventSink
	parts  []api.Part
	models int
	tools  []api.ToolCall
}

func (s *loopSink) Model(context.Context, api.ModelRequest) (api.ModelResponse, error) {
	s.models++
	parts := s.parts
	if parts == nil {
		parts = []api.Part{{ToolCall: &api.ToolCall{ID: "call_" + string(rune('a'+s.models)), Tool: "publish"}}}
	}
	return api.ModelResponse{Message: api.Message{Role: "assistant", Parts: parts}}, nil
}

func (s *loopSink) ToolCall(_ context.Context, call api.ToolCall) (api.ToolResult, error) {
	s.tools = append(s.tools, call)
	return api.ToolResult{ID: call.ID, Output: map[string]any{}}, nil
}

func TestConfig(t *testing.T) {
	h := toolagent.Harness{Model: "m"}
	for config, want := range map[string]string{
		`{"system_prompt":"be brief","other":1}`: "be brief",
		``:                                       "",
	} {
		sink := &recordingSink{}
		if err := h.Run(t.Context(), &api.Start{Config: []byte(config), Inputs: []api.Message{*api.TextMessage("user", "q")}}, sink); err != nil {
			t.Fatal(err)
		}
		first := sink.req.Messages[0]
		if want == "" && first.Role != "user" || want != "" && (first.Role != "system" || first.Parts[0].Text.Text != want) {
			t.Fatalf("config %q: first message %+v", config, first)
		}
	}
	for _, bad := range []string{`[]`, `{"system_prompt":1}`, `{"system_prompt":null}`} {
		if err := h.Run(t.Context(), &api.Start{Config: []byte(bad)}, &recordingSink{}); err == nil {
			t.Fatalf("config %s accepted", bad)
		}
	}
}

type recordingSink struct {
	api.EventSink
	req api.ModelRequest
}

func (s *recordingSink) Model(_ context.Context, req api.ModelRequest) (api.ModelResponse, error) {
	s.req = req
	return api.ModelResponse{Message: *api.TextMessage("assistant", "ok")}, nil
}
