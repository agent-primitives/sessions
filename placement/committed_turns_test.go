package placement_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/aramase/agentsessions/api"
	"github.com/aramase/agentsessions/eventlog"
	"github.com/aramase/agentsessions/harness/echoagent"
	"github.com/aramase/agentsessions/placement"
	"github.com/aramase/agentsessions/runtime/local"
)

// specRecorder records the SessionSpec of every Create it forwards.
type specRecorder struct {
	*local.Backend
	specs []api.SessionSpec
}

func (b *specRecorder) Create(ctx context.Context, s *api.SessionSpec) (api.Incarnation, error) {
	b.specs = append(b.specs, *s)
	return b.Backend.Create(ctx, s)
}

// failingRead fails every journal read, so the committed-turn count cannot be taken.
type failingRead struct{ eventlog.Store }

var errRead = errors.New("read failed")

func (failingRead) Read(int64) ([]eventlog.Record, error) { return nil, errRead }

// The backend tells a new session from one whose compute is gone by the committed-turn count the
// Placer passes to Create: 0 before the first turn, then one more per completed turn.
func TestExecPassesCommittedTurnsToCreate(t *testing.T) {
	inner := local.New(echoagent.Harness{})
	t.Cleanup(func() { _ = inner.Close() })
	b := &specRecorder{Backend: inner}
	p := placement.New(b, echoagent.Model)
	log := eventlog.AsStore(eventlog.New())
	for i := 0; i < 3; i++ {
		head, err := log.Head()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := p.Exec(context.Background(), log, "sess", []api.Message{*api.TextMessage("user", "hi")}, head); err != nil {
			t.Fatalf("turn %d: %v", i+1, err)
		}
	}
	var got []int
	for _, s := range b.specs {
		got = append(got, s.CommittedTurns)
	}
	if want := []int{0, 1, 2}; !slices.Equal(got, want) {
		t.Fatalf("CommittedTurns per Create = %v, want %v", got, want)
	}
}

// A journal the Placer cannot read gives no turn count, so it must not guess zero (which would let a
// memory-tier session be cold-created): Exec fails before Create.
func TestExecRefusesWhenTheJournalCannotBeRead(t *testing.T) {
	inner := local.New(echoagent.Harness{})
	t.Cleanup(func() { _ = inner.Close() })
	b := &specRecorder{Backend: inner}
	p := placement.New(b, echoagent.Model)
	log := failingRead{eventlog.AsStore(eventlog.New())}
	_, err := p.Exec(context.Background(), log, "sess", []api.Message{*api.TextMessage("user", "hi")}, 0)
	if !errors.Is(err, errRead) {
		t.Fatalf("Exec error = %v, want the read error", err)
	}
	if len(b.specs) != 0 {
		t.Fatalf("Create was called %d times after a failed journal read", len(b.specs))
	}
}

// createCounts returns the CommittedTurns of every Create b has seen, in order.
func createCounts(b *specRecorder) []int {
	var got []int
	for _, s := range b.specs {
		got = append(got, s.CommittedTurns)
	}
	return got
}

// execAs runs one turn on p, recorded under the harness name, the way session.Service routes it.
func execAs(t *testing.T, p *placement.Placer, log eventlog.Store, name string) {
	t.Helper()
	head, err := log.Head()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Exec(context.Background(), log, "sess", []api.Message{*api.TextMessage("user", "hi")}, head, placement.WithHarness(name)); err != nil {
		t.Fatalf("turn on %q: %v", name, err)
	}
}

// A turn an ExecRequest.harness override sent to another harness left no state in this harness's
// compute, so it must not count toward this harness's turns: otherwise a memory-tier default that
// never ran for the session would be refused as if it had lost state. A name that aliases this
// Placer in the registry ran on the same compute, so its turns do count, and so does a turn recorded
// under a name this host does not serve, since nothing proves it ran elsewhere.
func TestCommittedTurnsCountOnlyTurnsThisPlacerRan(t *testing.T) {
	newBackend := func() *specRecorder {
		inner := local.New(echoagent.Harness{})
		t.Cleanup(func() { _ = inner.Close() })
		return &specRecorder{Backend: inner}
	}
	memBackend, otherBackend := newBackend(), newBackend()
	mem := placement.New(memBackend, echoagent.Model)
	other := placement.New(otherBackend, echoagent.Model)
	if _, err := placement.NewRegistry("mem", map[string]*placement.Placer{"mem": mem, "alias": mem, "other": other}); err != nil {
		t.Fatal(err)
	}
	log := eventlog.AsStore(eventlog.New())

	execAs(t, other, log, "other")   // mem: 0
	execAs(t, mem, log, "mem")       // mem: 0 before, 1 after
	execAs(t, mem, log, "alias")     // mem: 1 before, 2 after
	execAs(t, other, log, "other")   // mem: 2
	execAs(t, mem, log, "elsewhere") // mem: 2 before, 3 after
	execAs(t, mem, log, "mem")       // mem: 3 before
	if got, want := createCounts(memBackend), []int{0, 1, 2, 3}; !slices.Equal(got, want) {
		t.Errorf("CommittedTurns per Create on mem = %v, want %v", got, want)
	}
	// other's own two turns, not mem's or the alias's: the unknown name counts for every Placer.
	if got, want := createCounts(otherBackend), []int{0, 1}; !slices.Equal(got, want) {
		t.Errorf("CommittedTurns per Create on other = %v, want %v", got, want)
	}
}

// A Placer outside a registry knows only the name it is running under, so it counts the turns
// recorded under that name; run with no name, it cannot attribute anything and counts every turn.
func TestCommittedTurnsOnAStandalonePlacer(t *testing.T) {
	inner := local.New(echoagent.Harness{})
	t.Cleanup(func() { _ = inner.Close() })
	b := &specRecorder{Backend: inner}
	p := placement.New(b, echoagent.Model)
	log := eventlog.AsStore(eventlog.New())
	execAs(t, p, log, "a")
	execAs(t, p, log, "b")
	execAs(t, p, log, "a")
	if got, want := createCounts(b), []int{0, 0, 1}; !slices.Equal(got, want) {
		t.Errorf("CommittedTurns per Create = %v, want %v", got, want)
	}
	if n, err := p.CommittedTurns(log, ""); err != nil || n != 3 {
		t.Errorf("CommittedTurns with no name = %d, %v; want 3", n, err)
	}
}

// A turn whose EXECUTION_START recorded no harness name (written before names were recorded), and a
// legacy END with no execution ID at all, cannot be attributed, so they count for every Placer:
// undercounting would let a memory-tier harness be cold-created over the state it held. A turn
// recorded under another harness the registry serves does not count.
func TestCommittedTurnsCountsTurnsWithNoRecordedHarness(t *testing.T) {
	for _, tc := range []struct {
		name   string
		events []api.Event
		want   int
	}{
		{name: "legacy END with no execution ID", events: []api.Event{
			{Kind: api.EventEnd},
		}, want: 1},
		{name: "EXECUTION_START with no harness name", events: []api.Event{
			{Kind: api.EventExecutionStart, ExecutionID: "x1", ExecutionStart: &api.ExecutionStart{}},
			{Kind: api.EventEnd, ExecutionID: "x1"},
		}, want: 1},
		{name: "another harness's turn is not counted", events: []api.Event{
			{Kind: api.EventExecutionStart, ExecutionID: "x1", ExecutionStart: &api.ExecutionStart{Harness: "other"}},
			{Kind: api.EventEnd, ExecutionID: "x1"},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inner := local.New(echoagent.Harness{})
			t.Cleanup(func() { _ = inner.Close() })
			mem := placement.New(inner, echoagent.Model)
			other := placement.New(inner, echoagent.Model)
			if _, err := placement.NewRegistry("mem", map[string]*placement.Placer{"mem": mem, "other": other}); err != nil {
				t.Fatal(err)
			}
			log := eventlog.AsStore(eventlog.New())
			for i, ev := range tc.events {
				if _, err := log.Append(int64(i), 0, ev); err != nil {
					t.Fatal(err)
				}
			}
			if n, err := mem.CommittedTurns(log, "mem"); err != nil || n != tc.want {
				t.Errorf("CommittedTurns = %d, %v; want %d", n, err, tc.want)
			}
		})
	}
}
