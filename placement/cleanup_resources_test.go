package placement

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/agent-primitives/sessions/api"
	"github.com/agent-primitives/sessions/eventlog"
	"github.com/agent-primitives/sessions/harness/echoagent"
	"github.com/agent-primitives/sessions/sqlitelog"
)

// ownedBackend opts into resource plans. Its runtime calls return evidence with their error.
type ownedBackend struct {
	*allocationBackend
	key      string
	plan     []api.CleanupResource
	evidence []api.CleanupResource
	err      error
}

func (b *ownedBackend) CleanupKey() string { return b.key }
func (b *ownedBackend) CleanupResources(api.CleanupOperation, string) []api.CleanupResource {
	return b.plan
}
func (b *ownedBackend) Fork(_ context.Context, _ api.SnapshotRef, opts api.ForkOpts) (api.Incarnation, error) {
	b.forks++
	evidence, _ := api.NewCleanupEvidence(b.evidence)
	return api.Incarnation{ID: opts.ChildSessionUID, Cleanup: evidence}, b.err
}

// recordingLog keeps the plan BeginAllocation received.
type recordingLog struct {
	allocationLog
	plan []api.CleanupResource
}

func (l *recordingLog) BeginAllocation(ctx context.Context, backend, operation string, plan ...api.CleanupResource) (string, error) {
	l.plan = plan
	return l.allocationLog.BeginAllocation(ctx, backend, operation)
}

// The plan is recorded under the backend's key before I/O, and a UID the runtime returns with an
// error is still confirmed. A keyless backend keeps the legacy path; an invalid plan runs nothing.
func TestAllocateRecordsOwnedPlanAndErrorEvidence(t *testing.T) {
	actor := api.CleanupResource{Reference: "actor"}
	proven := api.CleanupResource{Reference: "actor", UID: "uid"}
	runtimeErr, confirmErr := errors.New("runtime"), errors.New("confirm")
	for _, tc := range []struct {
		name        string
		key         string
		plan        []api.CleanupResource
		confirmFail bool
		wantTrace   []string
		wantPlan    []api.CleanupResource
	}{
		{name: "owned", key: "k", plan: []api.CleanupResource{actor}, wantTrace: []string{"begin", "effect", "confirm"}, wantPlan: []api.CleanupResource{actor}},
		{name: "confirm fails", key: "k", plan: []api.CleanupResource{actor}, confirmFail: true, wantTrace: []string{"begin", "effect", "confirm"}, wantPlan: []api.CleanupResource{actor}},
		{name: "keyless", plan: []api.CleanupResource{actor}, wantTrace: []string{"begin", "effect"}},
		{name: "invalid plan", key: "k", plan: []api.CleanupResource{proven}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var trace []string
			log := &recordingLog{allocationLog: allocationLog{Store: eventlog.AsStore(eventlog.New()), trace: &trace}}
			if tc.confirmFail {
				log.fail, log.cause = "confirm", confirmErr
			}
			backend := &ownedBackend{allocationBackend: &allocationBackend{trace: &trace}, key: tc.key, plan: tc.plan}
			evidence, _ := api.NewCleanupEvidence([]api.CleanupResource{proven})
			_, err := New(backend, nil).allocate(t.Context(), log, "s", "create", func() (api.Incarnation, error) {
				trace = append(trace, "effect")
				return api.Incarnation{ID: "s", Cleanup: evidence}, runtimeErr
			})
			if !reflect.DeepEqual(trace, tc.wantTrace) || !reflect.DeepEqual(log.plan, tc.wantPlan) {
				t.Fatalf("trace=%v plan=%+v, want %v %+v", trace, log.plan, tc.wantTrace, tc.wantPlan)
			}
			if tc.name == "invalid plan" {
				if !errors.Is(err, api.ErrCleanupResource) {
					t.Fatalf("err=%v, want ErrCleanupResource", err)
				}
				return
			}
			if !errors.Is(err, runtimeErr) || tc.confirmFail != errors.Is(err, confirmErr) {
				t.Fatalf("err=%v, want the runtime error, joined with the confirm error only when it failed", err)
			}
			if tc.key != "" && !reflect.DeepEqual(log.confirmed.Cleanup.Resources(), []api.CleanupResource{proven}) {
				t.Fatalf("confirmed %+v, want the evidence returned with the error", log.confirmed.Cleanup.Resources())
			}
		})
	}
}

// Every child's plan is recorded unpublished before any runtime Fork, and a failed fan-out still
// records the UIDs the runtime proved. A child log that cannot record unpublished is refused first.
func TestForkRecordsUnpublishedPlanAndPartialEvidence(t *testing.T) {
	s, err := sqlitelog.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var trace []string
	forkErr := errors.New("child create response lost")
	backend := &ownedBackend{
		allocationBackend: &allocationBackend{trace: &trace},
		key:               "k",
		plan:              []api.CleanupResource{{Reference: "tag", Borrower: "actor"}, {Reference: "actor"}},
		evidence:          []api.CleanupResource{{Reference: "tag", UID: "tag-uid", Borrower: "actor"}},
		err:               forkErr,
	}
	p := New(backend, echoagent.Model)

	plain := []ForkChild{{UID: "child", Log: eventlog.AsStore(eventlog.New())}}
	if err := p.Fork(t.Context(), s.Session("parent"), "parent", plain, 0); !errors.Is(err, ErrUnplaceable) || backend.forks != 0 {
		t.Fatalf("plain child log: err=%v forks=%d, want ErrUnplaceable before any runtime fork", err, backend.forks)
	}

	child := s.ForkSession(sqlitelog.SessionMeta{UID: "child", ParentUID: "parent"})
	if err := p.Fork(t.Context(), s.Session("parent"), "parent", []ForkChild{{UID: "child", Log: child}}, 0); !errors.Is(err, forkErr) {
		t.Fatalf("fork err=%v, want %v", err, forkErr)
	}
	if _, err := s.SessionInfo("child"); !errors.Is(err, sqlitelog.ErrSessionNotFound) {
		t.Fatalf("failed fork published its child: %v", err)
	}
	allocations, err := s.Allocations(t.Context(), "child")
	if err != nil || len(allocations) != 1 || allocations[0].Confirmed {
		t.Fatalf("allocations=%+v err=%v, want one attempt still pending its actor", allocations, err)
	}
	// The tag UID was recorded: confirming another UID for it conflicts.
	again, _ := api.NewCleanupEvidence([]api.CleanupResource{{Reference: "tag", UID: "other", Borrower: "actor"}})
	if err := child.ConfirmAllocation(t.Context(), allocations[0].ID, api.Incarnation{Cleanup: again}); !errors.Is(err, sqlitelog.ErrAllocationConflict) {
		t.Fatalf("tag UID was not recorded: confirm with another UID err=%v", err)
	}
}
