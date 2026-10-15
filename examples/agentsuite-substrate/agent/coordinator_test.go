package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aramase/agentsessions/api"
	"github.com/aramase/agentsessions/examples/agentsuite-substrate/invoke"
)

// scriptedSink answers model calls from a script and tool calls from a function, recording both.
type scriptedSink struct {
	api.EventSink
	replies  []string
	requests []api.ModelRequest
	calls    []api.ToolCall
	tool     func(api.ToolCall) (api.ToolResult, error)
}

func (s *scriptedSink) Model(_ context.Context, req api.ModelRequest) (api.ModelResponse, error) {
	s.requests = append(s.requests, req)
	if len(s.replies) == 0 {
		return api.ModelResponse{}, errors.New("script exhausted")
	}
	r := s.replies[0]
	s.replies = s.replies[1:]
	return api.ModelResponse{Message: *api.TextMessage("assistant", r)}, nil
}

func (s *scriptedSink) ToolCall(_ context.Context, tc api.ToolCall) (api.ToolResult, error) {
	s.calls = append(s.calls, tc)
	return s.tool(tc)
}

func stageCoordinator(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for src, dst := range map[string]string{
		"../suites/coordinator-workers/agents/coordinator.json":     "agent.json",
		"../suites/coordinator-workers/instructions/coordinator.md": "instructions.md",
	} {
		b, err := os.ReadFile(src)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, dst), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestLoadReadsInvokes(t *testing.T) {
	h, err := Load(stageCoordinator(t))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(h.Invokes, ",") != "writer,reviewer" {
		t.Fatalf("invokes = %v, want writer,reviewer", h.Invokes)
	}
	if _, err := h.WithApproval([]string{"publisher"}); err == nil || !strings.Contains(err.Error(), `no invokes edge to it`) {
		t.Fatalf("approval for an unknown target: err = %v", err)
	}
}

// The coordinator turns each delegation reply into an agentsuite.invoke call: a stable id from the
// execution, CONTROLLER_MEDIATED for writer and REQUIRES_APPROVAL for the gated reviewer. Each
// result reaches the model as the next message, and the plain-text reply ends the turn.
func TestCoordinatorDelegates(t *testing.T) {
	h, err := Load(stageCoordinator(t))
	if err != nil {
		t.Fatal(err)
	}
	h, err = h.WithApproval([]string{"reviewer"})
	if err != nil {
		t.Fatal(err)
	}
	sink := &scriptedSink{
		replies: []string{
			`{"invoke": "writer", "task": "write two lines about tides"}`,
			"```json\n{\"invoke\": \"reviewer\", \"task\": \"review: the sea\"}\n```",
			"Final: the sea, reviewed.",
		},
		tool: func(tc api.ToolCall) (api.ToolResult, error) {
			return api.ToolResult{ID: tc.ID, Output: map[string]any{"output": "from " + tc.Args["agent"].(string)}}, nil
		},
	}
	start := &api.Start{ExecutionID: "exec-1", Inputs: []api.Message{*api.TextMessage("user", "a poem about tides, reviewed")}}
	if err := h.Run(t.Context(), start, sink); err != nil {
		t.Fatal(err)
	}
	if len(sink.calls) != 2 {
		t.Fatalf("calls = %+v, want writer then reviewer", sink.calls)
	}
	for i, want := range []struct {
		id, agent string
		mediation api.Mediation
	}{
		{"exec-1-invoke-1", "writer", api.MediationControllerMediated},
		{"exec-1-invoke-2", "reviewer", api.MediationRequiresApproval},
	} {
		c := sink.calls[i]
		if c.ID != want.id || c.IdempotencyKey != want.id || c.Tool != invoke.Tool || c.Args["agent"] != want.agent || c.Mediation != want.mediation {
			t.Fatalf("call %d = %+v, want %+v", i, c, want)
		}
	}
	if len(sink.requests) != 3 {
		t.Fatalf("model calls = %d, want 3", len(sink.requests))
	}
	sys := replyText(sink.requests[0].Messages[0])
	if !strings.HasPrefix(sys, "Delegate writing tasks to the writer and reviews to the reviewer.") || !strings.Contains(sys, "writer, reviewer") {
		t.Fatalf("system prompt = %q", sys)
	}
	last := sink.requests[2].Messages
	if got := replyText(last[len(last)-1]); got != "Reply from reviewer:\nfrom reviewer" {
		t.Fatalf("last message = %q", got)
	}
}

// A denied (or refused) delegation comes back as an error result; the model sees why and the turn
// goes on. A park is returned as it is, so the host can suspend the turn.
func TestCoordinatorDenialAndPark(t *testing.T) {
	h, err := Load(stageCoordinator(t))
	if err != nil {
		t.Fatal(err)
	}
	h, _ = h.WithApproval([]string{"reviewer"})
	sink := &scriptedSink{
		replies: []string{`{"invoke": "reviewer", "task": "review"}`, "Final without review."},
		tool: func(tc api.ToolCall) (api.ToolResult, error) {
			return api.ToolResult{ID: tc.ID, IsError: true, Code: api.ToolResultCodeApprovalDenied, Error: "Tool execution denied by approval decision."}, nil
		},
	}
	if err := h.Run(t.Context(), &api.Start{ExecutionID: "e", Inputs: []api.Message{*api.TextMessage("user", "x")}}, sink); err != nil {
		t.Fatal(err)
	}
	last := sink.requests[1].Messages
	if got := replyText(last[len(last)-1]); got != "The delegation to reviewer did not run: Tool execution denied by approval decision." {
		t.Fatalf("denial message = %q", got)
	}
	park := &api.ApprovalParkedError{Ref: api.ApprovalRef{ExecutionID: "e", ToolCallID: "e-invoke-1", RequestSeq: 7}}
	parking := &scriptedSink{
		replies: []string{`{"invoke": "reviewer", "task": "review"}`},
		tool:    func(api.ToolCall) (api.ToolResult, error) { return api.ToolResult{}, park },
	}
	if err := h.Run(t.Context(), &api.Start{ExecutionID: "e", Inputs: []api.Message{*api.TextMessage("user", "x")}}, parking); !errors.Is(err, park) {
		t.Fatalf("park: err = %v, want the park returned", err)
	}
}

// A model that keeps delegating ends the turn with an error after maxDelegations calls.
func TestCoordinatorBoundsDelegations(t *testing.T) {
	h, err := Load(stageCoordinator(t))
	if err != nil {
		t.Fatal(err)
	}
	var replies []string
	for range maxDelegations + 1 {
		replies = append(replies, `{"invoke": "writer", "task": "again"}`)
	}
	sink := &scriptedSink{replies: replies, tool: func(tc api.ToolCall) (api.ToolResult, error) { return api.ToolResult{ID: tc.ID}, nil }}
	if err := h.Run(t.Context(), &api.Start{ExecutionID: "e", Inputs: []api.Message{*api.TextMessage("user", "x")}}, sink); err == nil || !strings.Contains(err.Error(), "more than 4 delegations") {
		t.Fatalf("err = %v", err)
	}
	if len(sink.calls) != maxDelegations {
		t.Fatalf("calls = %d, want %d", len(sink.calls), maxDelegations)
	}
}

func TestParseDelegation(t *testing.T) {
	for text, want := range map[string]bool{
		`{"invoke": "writer", "task": "t"}`:                   true,
		"```\n{\"invoke\": \"writer\", \"task\": \"t\"}\n```": true,
		`Here you go: {"invoke": "writer"}`:                   false,
		`{"task": "t"}`:                                       false,
		`{"invoke": ""}`:                                      false,
		"plain answer":                                        false,
		`{not json}`:                                          false,
	} {
		if _, got := parseDelegation(text); got != want {
			t.Errorf("parseDelegation(%q) = %v, want %v", text, got, want)
		}
	}
}
