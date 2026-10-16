package wire_test

import (
	"testing"

	"github.com/agent-primitives/sessions/api"
	v1 "github.com/agent-primitives/sessions/api/genpb"
	"github.com/agent-primitives/sessions/wire"
)

func TestDeletionStateConversion(t *testing.T) {
	// Literals pin meaningful states; unknown/zero must not silently grant ACTIVE semantics.
	for _, tc := range []struct {
		name   string
		domain api.DeletionState
		wire   v1.DeletionState
	}{
		{"active", "ACTIVE", 1},
		{"pending", "PENDING", 2},
		{"unspecified", "", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := wire.DeletionStateToProto(tc.domain); got != tc.wire {
				t.Errorf("ToProto(%q) = %d, want %d", tc.domain, got, tc.wire)
			}
			if got := wire.DeletionStateFromProto(tc.wire); got != tc.domain {
				t.Errorf("FromProto(%d) = %q, want %q", tc.wire, got, tc.domain)
			}
		})
	}
	for _, unknown := range []api.DeletionState{"UNRECOGNIZED", "UNKNOWN", "DELETED"} {
		if got := wire.DeletionStateToProto(unknown); got != v1.DeletionState_DELETION_STATE_UNSPECIFIED {
			t.Errorf("unknown domain %q = %v, want UNSPECIFIED", unknown, got)
		}
	}
	for _, unknown := range []v1.DeletionState{-1, 3, 99} {
		if got := wire.DeletionStateFromProto(unknown); got != "" {
			t.Errorf("unknown wire %d = %q, want empty", unknown, got)
		}
	}
}
