package api_test

import (
	"testing"

	v1 "github.com/agent-primitives/sessions/api/genpb"
)

// Completion is additive: released consumers retain the ACTIVE/PENDING numbers and request time.
func TestDeletionWireCompletionIsAdditive(t *testing.T) {
	values := v1.DeletionState(0).Descriptor().Values()
	for name, want := range map[string]int32{"DELETION_ACTIVE": 1, "DELETION_PENDING": 2, "DELETION_COMPLETE": 3} {
		value := values.ByNumber(v1.DeletionState(want).Number())
		if value == nil || string(value.Name()) != name {
			t.Fatalf("wire value %d=%v, want %s", want, value, name)
		}
	}
	fields := (&v1.Session{}).ProtoReflect().Descriptor().Fields()
	requested := fields.ByName("deletion_requested_time")
	completed := fields.ByName("deletion_completed_time")
	if requested == nil || requested.Number() != 17 {
		t.Fatal("request time field changed")
	}
	if completed == nil || completed.Number() != 18 || completed.Message() == nil || completed.Message().FullName() != "google.protobuf.Timestamp" {
		t.Fatal("completion timestamp is not additive field18")
	}
}
