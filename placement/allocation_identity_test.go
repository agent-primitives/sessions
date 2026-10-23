package placement

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/agent-primitives/sessions/api"
	"github.com/agent-primitives/sessions/eventlog"
	"github.com/agent-primitives/sessions/harness/echoagent"
	"github.com/agent-primitives/sessions/sqlitelog"
)

// A usable endpoint does not prove an allocation handle was returned.
type emptyIDBackend struct{ *allocationBackend }

func (b emptyIDBackend) Create(context.Context, *api.SessionSpec) (api.Incarnation, error) {
	return api.Incarnation{Address: "shared:1234", Runtime: "test"}, nil
}

func (b emptyIDBackend) Restore(context.Context, api.SnapshotRef) (api.Incarnation, error) {
	return api.Incarnation{Address: "shared:1234", Runtime: "test"}, nil
}

func TestTrackedAllocationRejectsEmptyIDBeforeDial(t *testing.T) {
	for _, operation := range []string{"create", "restore"} {
		for _, tracked := range []bool{false, true} {
			name := operation + "/untracked"
			if tracked {
				name = operation + "/sqlite"
			}
			t.Run(name, func(t *testing.T) {
				log := eventlog.AsStore(eventlog.New())
				var s *sqlitelog.Store
				if tracked {
					var err error
					s, err = sqlitelog.Open(filepath.Join(t.TempDir(), "journal.db"))
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { s.Close() })
					log = s.Session("s")
				}
				var trace []string
				dials := 0
				dialReached := errors.New("dial reached")
				p := New(emptyIDBackend{&allocationBackend{trace: &trace}}, echoagent.Model,
					WithDialer(func(api.Incarnation) (api.Harness, func() error, error) {
						dials++
						return nil, nil, dialReached
					}))
				var err error
				if operation == "create" {
					_, err = p.Exec(t.Context(), log, "s", nil, 0)
				} else {
					err = p.Resume(t.Context(), log, "s")
				}
				if !tracked {
					if !errors.Is(err, dialReached) || dials != 1 {
						t.Fatalf("untracked placement = %v, dials=%d", err, dials)
					}
					return
				}
				if !errors.Is(err, sqlitelog.ErrAllocationConflict) || dials != 0 {
					t.Fatalf("tracked placement = %v, dials=%d; want conflict before dial", err, dials)
				}
				attempts, err := s.Allocations(t.Context(), "s")
				if err != nil || len(attempts) != 1 {
					t.Fatalf("unresolved attempts = %+v, %v", attempts, err)
				}
				if attempts[0].Operation != operation || attempts[0].Confirmed || attempts[0].ResourceID != "" {
					t.Fatalf("empty response acknowledged or lost: %+v", attempts[0])
				}
				head, err := log.Head()
				if err != nil || head != 0 {
					t.Fatalf("journal changed before refusal: head=%d, err=%v", head, err)
				}
			})
		}
	}
}
