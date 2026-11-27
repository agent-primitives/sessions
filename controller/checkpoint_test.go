package controller

import (
	"errors"
	"testing"

	"github.com/agent-primitives/sessions/api"
	"github.com/agent-primitives/sessions/eventlog"
)

func checkpointRecords(t *testing.T, events ...api.Event) []eventlog.Record {
	t.Helper()
	log := gateLog(t, events...)
	records, err := log.Read(1)
	if err != nil {
		t.Fatal(err)
	}
	return records
}

func TestValidateCheckpointState(t *testing.T) {
	zero, one, two := int64(0), int64(1), int64(2)
	start := func(id string, count *int64) api.Event {
		return api.Event{ExecutionID: id, Kind: api.EventExecutionStart, ExecutionStart: &api.ExecutionStart{InputCount: count}}
	}
	input := func(id string) api.Event {
		return api.Event{ExecutionID: id, Kind: api.EventInput, Message: api.TextMessage("user", "hi")}
	}
	end := func(id string) api.Event {
		return api.Event{ExecutionID: id, Kind: api.EventEnd, End: &api.HarnessEnd{State: "COMPLETED"}}
	}
	failure := api.Event{ExecutionID: "e", Kind: api.EventError, Err: &api.Error{Description: "failed"}}
	lifecycle := api.Event{Kind: api.EventLifecycle, Lifecycle: &api.Lifecycle{Kind: api.LifecycleSuspend}}

	for _, tc := range []struct {
		name   string
		events []api.Event
		want   error
	}{
		{name: "empty"},
		{name: "lifecycle only", events: []api.Event{lifecycle}},
		{name: "completed", events: []api.Event{start("e", &one), input("e"), end("e")}},
		{name: "modern pending", events: []api.Event{start("e", &one), input("e")}, want: ErrCheckpointIncompleteTurn},
		{name: "modern error remains pending", events: []api.Event{start("e", &zero), failure}, want: ErrCheckpointIncompleteTurn},
		{name: "markerless modern pending", events: []api.Event{input("e")}, want: ErrCheckpointIncompleteTurn},
		{name: "partial input prefix", events: []api.Event{start("e", &two), input("e")}},
		{name: "partial input with error is invalid", events: []api.Event{start("e", &two), input("e"), failure}, want: ErrInvalidExecutionLog},
		{name: "legacy pending", events: []api.Event{{Kind: api.EventInput, Message: api.TextMessage("user", "hi")}}, want: ErrCheckpointIncompleteTurn},
		{name: "legacy end", events: []api.Event{{Kind: api.EventInput, Message: api.TextMessage("user", "hi")}, {Kind: api.EventEnd}}},
		{name: "legacy error", events: []api.Event{{Kind: api.EventInput, Message: api.TextMessage("user", "hi")}, {Kind: api.EventError, Err: &api.Error{Description: "failed"}}}},
		{name: "older incomplete superseded", events: []api.Event{start("old", &zero), start("new", &zero), end("new")}},
		{name: "approval call only", events: gateEvents()[:2], want: ErrCheckpointIncompleteTurn},
		{name: "approval pending", events: gateEvents()[:3]},
		{name: "approval decided", events: gateEvents()[:4], want: ErrCheckpointIncompleteTurn},
		{name: "approval decided recovery failed", events: append(append([]api.Event(nil), gateEvents()[:4]...), failure), want: ErrCheckpointIncompleteTurn},
		{name: "approval receipt remains pending", events: gateEvents()[:5], want: ErrCheckpointIncompleteTurn},
		{name: "approval completed", events: gateEvents()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateCheckpointState(checkpointRecords(t, tc.events...))
			if tc.want == nil && err != nil || tc.want != nil && !errors.Is(err, tc.want) {
				t.Fatalf("ValidateCheckpointState = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestValidateCheckpointStateAllowsInheritedOpenApproval(t *testing.T) {
	parent := gateLog(t, gateEvents()[:4]...)
	child := gateLog(t)
	if err := Fork(parent, child, 4); err != nil {
		t.Fatal(err)
	}
	records, err := child.Read(1)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateCheckpointState(records); err != nil {
		t.Fatalf("ValidateCheckpointState = %v", err)
	}
}
