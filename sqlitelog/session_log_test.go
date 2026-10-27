package sqlitelog_test

import (
	"context"
	"errors"
	"testing"

	"github.com/agent-primitives/sessions/api"
	"github.com/agent-primitives/sessions/eventlog"
	"github.com/agent-primitives/sessions/sqlitelog"
)

// The optional journal methods placement checks for. SessionLog and ForkSessionLog return the
// same *Log as Session and ForkSession, so they keep them; CheckWritable is in their
// eventlog.SessionJournal return type.
type allocationTracker interface {
	BeginAllocation(ctx context.Context, backend, operation string) (string, error)
	ConfirmAllocation(ctx context.Context, attempt string, in api.Incarnation) error
}

func TestSessionLogKeepsOptionalJournalMethods(t *testing.T) {
	s := mustOpen(t, ":memory:")
	logs := map[string]eventlog.SessionJournal{
		"SessionLog":     s.SessionLog("plain"),
		"ForkSessionLog": s.ForkSessionLog(sqlitelog.SessionMeta{UID: "child", ParentUID: "plain"}),
	}
	for name, log := range logs {
		if _, ok := log.(*sqlitelog.Log); !ok {
			t.Errorf("%s returned %T, want *sqlitelog.Log", name, log)
		}
		if _, ok := log.(allocationTracker); !ok {
			t.Errorf("%s dropped allocation recording", name)
		}
	}
}

func TestForkSessionLogPublishesAtFirstFence(t *testing.T) {
	s := mustOpen(t, ":memory:")
	log := s.ForkSessionLog(sqlitelog.SessionMeta{UID: "child", Harness: "agent", ModelPolicyEligible: true, ParentUID: "parent"})
	if _, err := s.SessionInfo("child"); !errors.Is(err, sqlitelog.ErrSessionNotFound) {
		t.Fatalf("handle published the child: %v", err)
	}
	if _, err := log.NewFence(); err != nil {
		t.Fatal(err)
	}
	info, err := s.SessionInfo("child")
	if err != nil || info.Harness != "agent" || info.ParentUID != "parent" || !info.ModelPolicyEligible {
		t.Fatalf("after first fence: %+v, %v", info, err)
	}
}
