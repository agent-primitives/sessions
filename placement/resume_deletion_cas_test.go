package placement

import (
	"errors"
	"reflect"
	"testing"

	"github.com/agent-primitives/sessions/api"
	"github.com/agent-primitives/sessions/eventlog"
	"github.com/agent-primitives/sessions/sqlitelog"
)

type resumeAdmissionProbe struct {
	eventlog.Store
	onWritable func()
	onHead     func()
}

func (s resumeAdmissionProbe) CheckWritable() error {
	s.onWritable()
	return eventlog.CheckWritable(s.Store)
}

func (s resumeAdmissionProbe) Head() (int64, error) {
	s.onHead()
	return s.Store.Head()
}

// Dropping either entry check or moving it outside the guard must fail before routing or effects.
func TestRegistryResumeDeletionAndCursorAdmission(t *testing.T) {
	for _, deleted := range []bool{false, true} {
		name := "stale cursor"
		if deleted {
			name = "deleted session"
		}
		t.Run(name, func(t *testing.T) {
			s, db := deletionStore(t)
			if err := s.PutSession(sqlitelog.SessionMeta{UID: "s", Harness: "echo"}); err != nil {
				t.Fatal(err)
			}
			log := s.Session("s")
			expected, want := int64(0), eventlog.ErrConflict
			wantHeads := 1
			if deleted {
				if _, err := s.DeleteSession(t.Context(), "s"); err != nil {
					t.Fatal(err)
				}
				expected, want, wantHeads = 1, eventlog.ErrDeleted, 0
			} else {
				f, err := log.NewFence()
				if err != nil {
					t.Fatal(err)
				}
				zero := int64(0)
				if _, err := log.Append(0, f, api.Event{Kind: api.EventExecutionStart, ExecutionID: "turn", ExecutionStart: &api.ExecutionStart{Harness: "echo", InputCount: &zero}}); err != nil {
					t.Fatal(err)
				}
			}
			p, b := newDeletionPlacer(t, false)
			r, err := NewRegistry("echo", map[string]*Placer{"echo": p})
			if err != nil {
				t.Fatal(err)
			}
			assertGuard := func() {
				if err := r.WithSessionGuard("s", func() error { return nil }); !errors.Is(err, ErrSessionBusy) {
					t.Errorf("admission check outside Resume guard: %v", err)
				}
			}
			writableChecks, headChecks := 0, 0
			probe := resumeAdmissionProbe{Store: log, onWritable: func() {
				writableChecks++
				assertGuard()
			}, onHead: func() {
				headChecks++
				assertGuard()
			}}
			before := rawDeletionState(t, db)
			if err := r.Resume(t.Context(), probe, "s", "echo", WithResumeExpectedLastSeq(expected), WithResolvedHarnessCheck(func(string) error {
				t.Error("refused Resume reached routing")
				return nil
			})); !errors.Is(err, want) {
				t.Fatalf("Resume = %v, want %v", err, want)
			}
			if writableChecks != 1 || headChecks != wantHeads {
				t.Errorf("writable checks=%d, head checks=%d; want 1, %d", writableChecks, headChecks, wantHeads)
			}
			if len(b.calls) != 0 {
				t.Errorf("refused Resume reached runtime/model/dial: %v", b.calls)
			}
			if got := rawDeletionState(t, db); !reflect.DeepEqual(before, got) {
				t.Error("refused Resume mutated durable state")
			}
			if err := r.WithSessionGuard("s", func() error { return nil }); err != nil {
				t.Errorf("refused Resume retained guard: %v", err)
			}
		})
	}
}
