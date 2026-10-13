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
