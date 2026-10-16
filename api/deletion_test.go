package api_test

import (
	"testing"

	v1 "github.com/agent-primitives/sessions/api/genpb"
)

// Intent-only deletion must not expose a promise of resource cleanup to clients.
func TestDeletionWireHasNoCompletion(t *testing.T) {
	if value := v1.DeletionState(0).Descriptor().Values().ByName("DELETION_COMPLETE"); value != nil {
		t.Error("deletion wire enum exposes completion")
	}
	if field := (&v1.Session{}).ProtoReflect().Descriptor().Fields().ByName("deletion_completed_time"); field != nil {
		t.Error("session wire descriptor exposes completion time")
	}
}
