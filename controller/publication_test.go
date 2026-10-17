package controller_test

import (
	"errors"
	"testing"

	"github.com/agent-primitives/sessions/api"
	"github.com/agent-primitives/sessions/controller"
	"github.com/agent-primitives/sessions/eventlog"
	"github.com/agent-primitives/sessions/sqlitelog"
)

func TestUnpublishedForkRefusesDirectControllerAdmission(t *testing.T) {
	for _, operation := range []string{"new", "new-bound", "exec", "resume", "replay", "approve", "fork-parent"} {
		t.Run(operation, func(t *testing.T) {
			store, err := sqlitelog.Open(":memory:")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			if err := store.PutSession(sqlitelog.SessionMeta{UID: "parent"}); err != nil {
				t.Fatal(err)
			}
			log := store.Session("child")
			// An existing controller must recheck admission when its entry point is invoked.
			c, err := controller.New(log, echoModel, controller.WithFence(1))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.BeginFork(t.Context(), "parent", []sqlitelog.SessionMeta{{UID: "child", ParentUID: "parent"}}); err != nil {
				t.Fatal(err)
			}
			switch operation {
			case "new":
				_, err = controller.New(log, echoModel)
			case "new-bound":
				_, err = controller.New(log, echoModel, controller.WithFence(1))
			case "exec":
				err = c.Exec(t.Context(), &echoHarness{}, nil, 0)
			case "resume":
				_, err = c.Resume(t.Context(), &echoHarness{})
			case "replay":
				_, err = c.Replay(t.Context(), &echoHarness{})
			case "approve":
				_, err = controller.Approve(log, api.ApprovalDecision{})
			case "fork-parent":
				err = controller.Fork(log, store.Session("grandchild"), 0)
			}
			if !errors.Is(err, eventlog.ErrUnpublished) {
				t.Fatalf("%s admitted PREPARING journal: %v", operation, err)
			}
		})
	}
}
