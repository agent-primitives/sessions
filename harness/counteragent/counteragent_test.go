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

// TestMemoryRestoreDoesNotReinitialize is the harness-side I4 contract: a memory-restored sandbox
// must continue from its in-RAM count, NOT reset to zero. The restore is modeled by reusing the
// SAME instance (its fields = the snapshot-preserved RAM). The turn uses an empty History, the
// shape of a fresh STATELESS start: the host sends the full prior log after a restore (covered by
// TestNonEmptyHistoryIsNotAppliedToState), so this guards a harness that resets on an empty
// History. A naive `if len(History)==0 { count=0 }` would fail this.
func TestMemoryRestoreDoesNotReinitialize(t *testing.T) {
	h := &Harness{}
	sink := &captureSink{}
	_ = h.Run(context.Background(), &api.Start{}, sink) // count -> 1
	_ = h.Run(context.Background(), &api.Start{}, sink) // count -> 2 (state before a suspend)
	// Same instance (RAM preserved), one more turn with an empty History (a fresh-start shape,
	// not what the host sends after a restore). It must not reset the count.
	if err := h.Run(context.Background(), &api.Start{History: nil}, sink); err != nil {
		t.Fatal(err)
	}
	if got := sink.outputs[len(sink.outputs)-1]; got != "3" {
		t.Fatalf("post-restore output=%q want %q (empty History must NOT reset the in-RAM count)", got, "3")
	}
}

// TestNonEmptyHistoryIsNotAppliedToState guards the other failure: the full prior log the host
// sends after a restore must not be replayed into the count — the count comes from RAM, not from
// History. A naive `count += len(History)` would double-apply and fail this.
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
