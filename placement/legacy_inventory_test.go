package placement_test

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/agent-primitives/sessions/api"
	"github.com/agent-primitives/sessions/eventlog"
	"github.com/agent-primitives/sessions/placement"
	"github.com/agent-primitives/sessions/sqlitelog"
)

// Untracked fork paths have no write-ahead clone coverage. Neither the first fence (including
// ForkSession's atomic model metadata), successful finalization nor a later tracked restore
// proves that all allocations in the child's lifetime are known.
func TestLegacyForkInventoryStaysIncomplete(t *testing.T) {
	for _, modern := range []bool{false, true} {
		for _, outcome := range []string{"success", "failed-partial", "interrupted-after-fence"} {
			t.Run(outcome+map[bool]string{false: "/legacy", true: "/model-eligible"}[modern], func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "inventory.db")
				s, err := sqlitelog.Open(path)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { s.Close() })
				parent := s.Session("parent")
				fence, err := parent.NewFence()
				if err != nil {
					t.Fatal(err)
				}
				if _, err := parent.Append(0, fence, api.Event{Kind: api.EventInput, Message: api.TextMessage("user", "hi")}); err != nil {
					t.Fatal(err)
				}
				meta := sqlitelog.SessionMeta{UID: "c1", ParentUID: "parent", ForkSeq: 1, ModelPolicyEligible: modern}
				child := s.Session("c1")
				if modern {
					child = s.ForkSession(meta)
				}
				interrupted := errors.New("process stopped after durable first fence")
				observed := false
				first := &inventoryFenceLog{Store: child, afterFence: func() error {
					observed = true
					info, err := s.SessionInfo("c1")
					if err != nil {
						t.Fatal(err)
					}
					if info.InventoryComplete {
						t.Error("first-row window falsely certified inventory")
					}
					if !modern && info.ParentUID != "" {
						t.Fatal("fixture skipped pre-parent-metadata window")
					}
					if outcome == "interrupted-after-fence" {
						return interrupted
					}
					return nil
				}}
				ctl := &cloningControl{snapshot: "snap-parent-1"}
				if outcome == "failed-partial" {
					ctl.failCloneOf = "c2"
				}
				children := []placement.ForkChild{{UID: "c1", Log: first}, {UID: "c2", Log: inventoryFenceLog{Store: s.Session("c2")}}}
				err = memorySubstratePlacer(ctl).Fork(t.Context(), parent, "parent", children, 1)
				if outcome == "success" && err != nil {
					t.Fatal(err)
				}
				if outcome != "success" && err == nil {
					t.Fatal("expected incomplete fork")
				}
				if !observed {
					t.Fatal("fork did not reach real durable first fence")
				}
				if outcome == "interrupted-after-fence" && !errors.Is(err, interrupted) {
					t.Fatalf("interruption = %v", err)
				}
				if outcome == "success" {
					if err := s.PutSession(meta); err != nil {
						t.Fatal(err)
					}
				}
				if err := s.Close(); err != nil {
					t.Fatal(err)
				}
				s, err = sqlitelog.Open(path)
				if err != nil {
					t.Fatal(err)
				}
				// Later allocation tracking cannot retroactively prove a legacy clone's absence.
				attempt, err := s.Session("c1").BeginAllocation(t.Context(), "route", "restore")
				if err != nil {
					t.Fatal(err)
				}
				if err := s.Session("c1").ConfirmAllocation(t.Context(), attempt, api.Incarnation{ID: "restored"}); err != nil {
					t.Fatal(err)
				}
				info, err := s.SessionInfo("c1")
				if err != nil || info.InventoryComplete {
					t.Fatalf("reopened/later tracked child = %+v, %v", info, err)
				}
			})
		}
	}
}

// Deliberately preserves the old, non-recording clone boundary even after tracked Fork lands.
// NewFence still exercises the real SQLite NewFence/ForkSession transaction.
type inventoryFenceLog struct {
	eventlog.Store
	afterFence func() error
}

func (l inventoryFenceLog) NewFence() (int64, error) {
	fence, err := l.Store.NewFence()
	if err == nil && l.afterFence != nil {
		err = l.afterFence()
	}
	return fence, err
}

func (l inventoryFenceLog) CheckWritable() error { return eventlog.CheckWritable(l.Store) }
