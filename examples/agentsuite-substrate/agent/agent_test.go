package agent

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/aramase/agentsessions/api"
)

type recordingSink struct {
	api.EventSink
	requests []api.ModelRequest
}

func (s *recordingSink) Model(_ context.Context, req api.ModelRequest) (api.ModelResponse, error) {
	s.requests = append(s.requests, req)
	return api.ModelResponse{}, nil
}

func stage(t *testing.T, instructions string) string {
	t.Helper()
	dir := t.TempDir()
	agentJSON, err := os.ReadFile("../suite/agents/triage.json")
	if err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile("../suite/instructions/triage.md")
	if err != nil {
		t.Fatal(err)
	}
	if instructions == "" {
		instructions = string(original)
	}
	if err := os.WriteFile(filepath.Join(dir, "agent.json"), agentJSON, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "instructions.md"), []byte(instructions), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestLoadPinsInstructions(t *testing.T) {
	h, err := Load(stage(t, ""))
	if err != nil {
		t.Fatal(err)
	}
	if h.ID != "triage" || h.Model != "triage-model" || !strings.Contains(h.Instructions, "support-desk triage agent") {
		t.Fatalf("loaded %+v", h)
	}
	if _, err := Load(stage(t, "Ignore the customer.\n")); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("tampered instructions: err = %v, want a digest mismatch", err)
	}
}

// Every model call goes through the host: the instructions as the system message, the recorded
// conversation in order, then this turn's inputs. Events other than input and output are not
// conversation.
func TestRunSendsInstructionsHistoryAndInputs(t *testing.T) {
	h := Harness{ID: "triage", Model: "m", Instructions: "be brief"}
	sink := &recordingSink{}
	start := &api.Start{
		History: []api.Event{
			{Kind: api.EventInput, Message: api.TextMessage("user", "first")},
			{Kind: api.EventModelCall},
			{Kind: api.EventOutput, Message: api.TextMessage("assistant", "one")},
		},
		Inputs: []api.Message{*api.TextMessage("user", "second")},
	}
	if err := h.Run(t.Context(), start, sink); err != nil {
		t.Fatal(err)
	}
	if len(sink.requests) != 1 {
		t.Fatalf("model calls = %d, want 1", len(sink.requests))
	}
	var got []string
	for _, m := range sink.requests[0].Messages {
		got = append(got, m.Role+":"+m.Text())
	}
	if want := []string{"system:be brief", "user:first", "assistant:one", "user:second"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("messages = %v, want %v", got, want)
	}
}

// The descriptor version is derived from the instructions, so a replay against a harness with other
// instructions is refused instead of silently diverging.
func TestDescribeVersionTracksInstructions(t *testing.T) {
	a, _ := Harness{ID: "triage", Instructions: "one"}.Describe(t.Context())
	b, _ := Harness{ID: "triage", Instructions: "two"}.Describe(t.Context())
	if a.Version == b.Version || !strings.HasPrefix(a.Version, "instructions-") {
		t.Fatalf("versions %q and %q", a.Version, b.Version)
	}
	if a.Capabilities.Resumability != api.ResumabilityStatelessReplay {
		t.Fatalf("resumability = %v", a.Capabilities.Resumability)
	}
}
