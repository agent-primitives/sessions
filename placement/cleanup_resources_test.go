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
	// refuse makes Describe fail, so fork admission refuses before any runtime Fork.
	refuse error
}

func (b *ownedBackend) Describe(ctx context.Context) (api.Descriptor, error) {
	if b.refuse != nil {
		return api.Descriptor{}, b.refuse
	}
	return b.allocationBackend.Describe(ctx)
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

// recordingLog keeps the plan BeginPlannedAllocation received.
type recordingLog struct {
	allocationLog
	plan []api.CleanupResource
}

func (l *recordingLog) BeginPlannedAllocation(ctx context.Context, backend, operation string, plan ...api.CleanupResource) (string, error) {
	l.plan = plan
	return l.BeginAllocation(ctx, backend, operation)
}

// The plan is recorded under the backend's key before I/O, and a UID the runtime returns with an
// error is still confirmed, without the handle that would mark a success. A keyless backend, or a
// journal with only the documented BeginAllocation, keeps the legacy path; an invalid plan runs nothing.
func TestAllocateRecordsOwnedPlanAndErrorEvidence(t *testing.T) {
	actor := api.CleanupResource{Reference: "actor"}
	proven := api.CleanupResource{Reference: "actor", UID: "uid"}
	runtimeErr, confirmErr := errors.New("runtime"), errors.New("confirm")
	for _, tc := range []struct {
		name        string
		key         string
		plan        []api.CleanupResource
		confirmFail bool
		documented  bool
		wantTrace   []string
		wantPlan    []api.CleanupResource
	}{
		{name: "owned", key: "k", plan: []api.CleanupResource{actor}, wantTrace: []string{"begin", "effect", "confirm"}, wantPlan: []api.CleanupResource{actor}},
		{name: "confirm fails", key: "k", plan: []api.CleanupResource{actor}, confirmFail: true, wantTrace: []string{"begin", "effect", "confirm"}, wantPlan: []api.CleanupResource{actor}},
		{name: "keyless", plan: []api.CleanupResource{actor}, wantTrace: []string{"begin", "effect"}},
		{name: "documented journal", key: "k", plan: []api.CleanupResource{actor}, documented: true, wantTrace: []string{"begin", "effect"}},
		{name: "invalid plan", key: "k", plan: []api.CleanupResource{proven}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var trace []string
			log := &recordingLog{allocationLog: allocationLog{Store: eventlog.AsStore(eventlog.New()), trace: &trace}}
			if tc.confirmFail {
				log.fail, log.cause = "confirm", confirmErr
			}
			var journal eventlog.Store = log
			if tc.documented {
				journal = &log.allocationLog
			}
			backend := &ownedBackend{allocationBackend: &allocationBackend{trace: &trace}, key: tc.key, plan: tc.plan}
			evidence, _ := api.NewCleanupEvidence([]api.CleanupResource{proven})
			_, err := New(backend, nil).allocate(t.Context(), journal, "s", "create", func() (api.Incarnation, error) {
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
			if tc.key != "" && !tc.documented && (log.confirmed.ID != "" || !reflect.DeepEqual(log.confirmed.Cleanup.Resources(), []api.CleanupResource{proven})) {
				t.Fatalf("confirmed %q %+v, want only the evidence returned with the error", log.confirmed.ID, log.confirmed.Cleanup.Resources())
			}
		})
	}
}

// Each child's plan is recorded unpublished just before its runtime Fork, and a failed fan-out still
// records the UIDs the runtime proved. A refused fork, and a child the fan-out never reached, leave no
// attempt. A child log that cannot record unpublished forks untracked (STORE-7).
func TestForkRecordsUnpublishedPlanAndPartialEvidence(t *testing.T) {
	s, err := sqlitelog.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var trace []string
	forkErr, refused := errors.New("child create response lost"), errors.New("refused")
	backend := &ownedBackend{
		allocationBackend: &allocationBackend{trace: &trace},
		key:               "k",
		plan:              []api.CleanupResource{{Reference: "tag", Borrower: "actor"}, {Reference: "actor"}},
		evidence:          []api.CleanupResource{{Reference: "tag", UID: "tag-uid", Borrower: "actor"}},
		err:               forkErr,
		refuse:            refused,
	}
	p := New(backend, echoagent.Model)
	children := func(uids ...string) []ForkChild {
		var out []ForkChild
		for _, uid := range uids {
			out = append(out, ForkChild{UID: uid, Log: s.ForkSession(sqlitelog.SessionMeta{UID: uid, ParentUID: "parent"})})
		}
		return out
	}
	attempts := func(uid string) []sqlitelog.Allocation {
		t.Helper()
		got, err := s.Allocations(t.Context(), uid)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}

	if err := p.Fork(t.Context(), s.Session("parent"), "parent", children("refused"), 0); !errors.Is(err, refused) || len(attempts("refused")) != 0 {
		t.Fatalf("refused fork: err=%v attempts=%+v, want no attempt", err, attempts("refused"))
	}
	backend.refuse = nil

	plain := []ForkChild{{UID: "plain", Log: eventlog.AsStore(eventlog.New())}}
	if err := p.Fork(t.Context(), s.Session("parent"), "parent", plain, 0); !errors.Is(err, forkErr) || backend.forks != 1 {
		t.Fatalf("plain child log: err=%v forks=%d, want an untracked runtime fork", err, backend.forks)
	}

	if err := p.Fork(t.Context(), s.Session("parent"), "parent", children("child", "unreached"), 0); !errors.Is(err, forkErr) {
		t.Fatalf("fork err=%v, want %v", err, forkErr)
	}
	if _, err := s.SessionInfo("child"); !errors.Is(err, sqlitelog.ErrSessionNotFound) {
		t.Fatalf("failed fork published its child: %v", err)
	}
	if got := attempts("unreached"); len(got) != 0 {
		t.Fatalf("child the fan-out never reached has attempts %+v", got)
	}
	allocations := attempts("child")
	if len(allocations) != 1 || allocations[0].Confirmed {
		t.Fatalf("allocations=%+v, want one attempt still pending its actor", allocations)
	}
	// The tag UID was recorded: confirming another UID for it conflicts.
	child := s.ForkSession(sqlitelog.SessionMeta{UID: "child", ParentUID: "parent"})
	again, _ := api.NewCleanupEvidence([]api.CleanupResource{{Reference: "tag", UID: "other", Borrower: "actor"}})
	if err := child.ConfirmAllocation(t.Context(), allocations[0].ID, api.Incarnation{Cleanup: again}); !errors.Is(err, sqlitelog.ErrAllocationConflict) {
		t.Fatalf("tag UID was not recorded: confirm with another UID err=%v", err)
	}
}
