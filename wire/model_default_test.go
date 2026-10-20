package wire_test

import (
	"reflect"
	"testing"

	"github.com/agent-primitives/sessions/api"
	v1 "github.com/agent-primitives/sessions/api/genpb"
	"github.com/agent-primitives/sessions/canon"
	"github.com/agent-primitives/sessions/wire"
	"google.golang.org/protobuf/proto"
)

// Dropping either conversion must not silently replace the recorded invocation default on replay.
func TestExecutionStartModelRoundTrip(t *testing.T) {
	for _, model := range []string{"", "host-default"} {
		t.Run(model, func(t *testing.T) {
			want := api.Event{
				ExecutionID: "run", Kind: api.EventExecutionStart,
				ExecutionStart: &api.ExecutionStart{InputCount: proto.Int64(0), Model: model},
			}
			blob, err := proto.Marshal(wire.EventToProto(want))
			if err != nil {
				t.Fatal(err)
			}
			var decoded v1.Event
			if err := proto.Unmarshal(blob, &decoded); err != nil {
				t.Fatal(err)
			}
			if got := wire.EventFromProto(&decoded).ExecutionStart; !reflect.DeepEqual(got, want.ExecutionStart) {
				t.Fatalf("round-trip start = %#v, want %#v", got, want.ExecutionStart)
			}
		})
	}
}

// Changing the effective default must invalidate the chain, even if no model call was made.
func TestExecutionStartModelBoundIntoHash(t *testing.T) {
	start := &api.ExecutionStart{InputCount: proto.Int64(0)}
	event := api.Event{ExecutionID: "run", Kind: api.EventExecutionStart, ExecutionStart: start}
	seen := make(map[string]string)
	for _, model := range []string{"", "host-default", "other-default"} {
		start.Model = model
		hash, err := canon.HashRecord("", 1, event)
		if err != nil {
			t.Fatal(err)
		}
		if previous, ok := seen[hash]; ok {
			t.Fatalf("models %q and %q have the same record hash", previous, model)
		}
		seen[hash] = model
	}
}
