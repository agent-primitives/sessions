package counteragent

import (
	"context"
	"reflect"
	"testing"

	"github.com/agent-primitives/sessions/api"
)

// captureSink records Output deltas; the counter only ever calls Output.
type captureSink struct{ outputs []string }

func (s *captureSink) Model(context.Context, api.ModelRequest) (api.ModelResponse, error) {
	return api.ModelResponse{}, nil
}
func (s *captureSink) Output(_ context.Context, delta string) error {
	s.outputs = append(s.outputs, delta)
	return nil
}
func (s *captureSink) ToolCall(context.Context, api.ToolCall) (api.ToolResult, error) {
	return api.ToolResult{}, nil
}
func (s *captureSink) Report(context.Context, api.ToolResult) error { return nil }
func (s *captureSink) Usage(context.Context, api.Usage) error       { return nil }

func TestCounterIncrementsInRAM(t *testing.T) {
	h := &Harness{}
	sink := &captureSink{}
	for i := 0; i < 3; i++ {
		if err := h.Run(context.Background(), &api.Start{}, sink); err != nil {
			t.Fatal(err)
		}
	}
	if want := []string{"1", "2", "3"}; !reflect.DeepEqual(sink.outputs, want) {
		t.Fatalf("outputs=%v want %v (count must persist across turns in RAM)", sink.outputs, want)
	}
}

// TestEmptyHistoryDoesNotResetCount is a harness-only guard for the I4 rule that the count is never
// initialized from History: on the same instance (RAM preserved), a turn whose History is empty
// must continue the in-RAM count, not reset it. It does not model a memory restore or what the
// host sends; it only catches a harness that keys a reset off History, e.g.
// `if len(History)==0 { count=0 }`.
func TestEmptyHistoryDoesNotResetCount(t *testing.T) {
	h := &Harness{}
	sink := &captureSink{}
	_ = h.Run(context.Background(), &api.Start{}, sink) // count -> 1
	_ = h.Run(context.Background(), &api.Start{}, sink) // count -> 2
	if err := h.Run(context.Background(), &api.Start{History: nil}, sink); err != nil {
		t.Fatal(err)
	}
	if got := sink.outputs[len(sink.outputs)-1]; got != "3" {
		t.Fatalf("output=%q want %q (empty History must NOT reset the in-RAM count)", got, "3")
	}
}

// TestNonEmptyHistoryIsNotAppliedToState is a harness-only guard for the same rule: a non-empty
// History must not be replayed into the count. The History is five zero-value events, not a
// host-built log; this test checks only that the counter ignores History, not what the host
// delivers. It catches a harness that does `count += len(History)`.
func TestNonEmptyHistoryIsNotAppliedToState(t *testing.T) {
	h := &Harness{}
	sink := &captureSink{}
	_ = h.Run(context.Background(), &api.Start{}, sink) // count -> 1
	if err := h.Run(context.Background(), &api.Start{History: make([]api.Event, 5)}, sink); err != nil {
		t.Fatal(err)
	}
	if got := sink.outputs[len(sink.outputs)-1]; got != "2" {
		t.Fatalf("output=%q want %q (History must NOT be applied to the in-RAM count)", got, "2")
	}
}

func TestCounterRequiresMemorySnapshot(t *testing.T) {
	d, err := (&Harness{}).Describe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if d.Capabilities.Resumability != api.ResumabilityRequiresMemorySnapshot {
		t.Fatalf("resumability=%q want REQUIRES_MEMORY_SNAPSHOT", d.Capabilities.Resumability)
	}
}
