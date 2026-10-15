package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/agent-primitives/sessions/api"
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

// The harness requests the override when one is set, and the manifest's model otherwise.
func TestWithModel(t *testing.T) {
	h, err := Load(stage(t, ""))
	if err != nil {
		t.Fatal(err)
	}
	if got := h.WithModel("").Model; got != "triage-model" {
		t.Fatalf("no override: model = %q, want the manifest's triage-model", got)
	}
	o := h.WithModel("gpt-x")
	if o.Model != "gpt-x" || h.Model != "triage-model" {
		t.Fatalf("override: model = %q (original %q), want gpt-x and the original unchanged", o.Model, h.Model)
	}
	sink := &recordingSink{}
	if err := o.Run(t.Context(), &api.Start{Inputs: []api.Message{*api.TextMessage("user", "hi")}}, sink); err != nil {
		t.Fatal(err)
	}
	if len(sink.requests) != 1 || sink.requests[0].Model != "gpt-x" {
		t.Fatalf("requests = %+v, want one for gpt-x", sink.requests)
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

// The memory tier builds each request from its own RAM, not the history the host sends, keeps a
// turn only when the model call succeeds, and says so in its descriptor.
func TestMemoryTierUsesRAM(t *testing.T) {
	h := Harness{ID: "writer", Model: "m", Instructions: "write"}.WithMemory()
	d, _ := h.Describe(t.Context())
	if d.Capabilities.Resumability != api.ResumabilityRequiresMemorySnapshot {
		t.Fatalf("resumability = %v, want REQUIRES_MEMORY_SNAPSHOT", d.Capabilities.Resumability)
	}
	sink := &replySink{reply: "one"}
	history := []api.Event{{Kind: api.EventInput, Message: api.TextMessage("user", "from the journal")}}
	turn := func(input string) {
		t.Helper()
		if err := h.Run(t.Context(), &api.Start{History: history, Inputs: []api.Message{*api.TextMessage("user", input)}}, sink); err != nil {
			t.Fatal(err)
		}
	}
	turn("first")
	sink.reply = "two"
	turn("second")
	sink.fail = true
	if err := h.Run(t.Context(), &api.Start{Inputs: []api.Message{*api.TextMessage("user", "lost")}}, sink); err == nil {
		t.Fatal("failed model call: err = nil")
	}
	sink.fail = false
	turn("third")
	var got []string
	for _, m := range sink.requests[len(sink.requests)-1].Messages {
		got = append(got, m.Role+":"+m.Text())
	}
	want := []string{"system:write", "user:first", "assistant:one", "user:second", "assistant:two", "user:third"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("messages = %v, want %v", got, want)
	}
	// Each WithMemory starts an empty transcript.
	if fresh := (Harness{ID: "writer"}).WithMemory(); len(fresh.memory.messages) != 0 {
		t.Fatalf("fresh transcript holds %d messages", len(fresh.memory.messages))
	}
}

type replySink struct {
	api.EventSink
	reply    string
	fail     bool
	requests []api.ModelRequest
}

func (s *replySink) Model(_ context.Context, req api.ModelRequest) (api.ModelResponse, error) {
	s.requests = append(s.requests, req)
	if s.fail {
		return api.ModelResponse{}, errors.New("provider down")
	}
	return api.ModelResponse{Message: *api.TextMessage("assistant", s.reply)}, nil
}

// usageSink answers every model call with usage and records what the harness reports.
type usageSink struct {
	api.EventSink
	usage    api.Usage
	failUse  bool
	reported []api.Usage
}

func (s *usageSink) Model(context.Context, api.ModelRequest) (api.ModelResponse, error) {
	return api.ModelResponse{Message: *api.TextMessage("assistant", "ok"), Usage: s.usage}, nil
}

func (s *usageSink) Usage(_ context.Context, u api.Usage) error {
	if s.failUse {
		return errors.New("journal down")
	}
	s.reported = append(s.reported, u)
	return nil
}

// Both tiers journal the usage the provider returned, once per model call, and record nothing when
// the provider returns none. A memory turn whose usage record fails keeps RAM as it was.
func TestRunReportsProviderUsage(t *testing.T) {
	usage := api.Usage{Model: "m", InputTokens: 40, OutputTokens: 9}
	for name, h := range map[string]Harness{
		"stateless": {ID: "writer", Model: "m"},
		"memory":    Harness{ID: "writer", Model: "m"}.WithMemory(),
	} {
		t.Run(name, func(t *testing.T) {
			sink := &usageSink{usage: usage}
			start := &api.Start{Inputs: []api.Message{*api.TextMessage("user", "hi")}}
			if err := h.Run(t.Context(), start, sink); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(sink.reported, []api.Usage{usage}) {
				t.Fatalf("reported %+v, want exactly %+v", sink.reported, usage)
			}
			none := &usageSink{}
			if err := h.Run(t.Context(), start, none); err != nil {
				t.Fatal(err)
			}
			if len(none.reported) != 0 {
				t.Fatalf("zero usage reported %+v, want nothing", none.reported)
			}
			if err := h.Run(t.Context(), start, &usageSink{usage: usage, failUse: true}); err == nil {
				t.Fatal("failed usage record: err = nil")
			}
			if h.memory != nil && h.memory.turns != 2 {
				t.Fatalf("memory tier kept %d turns, want 2 (the failed usage record must not count)", h.memory.turns)
			}
		})
	}
}
