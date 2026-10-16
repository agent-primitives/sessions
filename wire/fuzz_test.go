package wire_test

import (
	"reflect"
	"testing"

	"google.golang.org/protobuf/proto"

	v1 "github.com/agent-primitives/sessions/api/genpb"
	"github.com/agent-primitives/sessions/internal/fuzzcheck"
	"github.com/agent-primitives/sessions/wire"
)

// FuzzEventFromProto decodes arbitrary Events the way the host reads a journal record or a peer's
// event. Decoding and conversion must not panic or allocate out of proportion to the input, and
// conversion must be stable: converting the decoded event back to the wire and decoding it again
// yields the same event, so a record survives being re-sent (Replay, a fork copy) unchanged.
func FuzzEventFromProto(f *testing.F) {
	for _, blob := range fuzzcheck.JournalEvents(f) {
		f.Add(blob)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		// The bound covers decoding the protobuf too: that is where a claimed length or count would
		// size an allocation.
		fuzzcheck.Allocs(t, len(data), func() {
			pb := &v1.Event{}
			if err := proto.Unmarshal(data, pb); err != nil {
				return
			}
			first := wire.EventFromProto(pb)
			again := wire.EventFromProto(wire.EventToProto(first))
			if !reflect.DeepEqual(first, again) {
				t.Fatalf("event changed across a wire round trip:\n%#v\n%#v", first, again)
			}
			// The model-call fields harnesswire decodes beside the event.
			mc := pb.GetModel()
			if choice, err := wire.ToolChoiceFromProto(mc.GetToolChoice()); err == nil {
				back, err := wire.ToolChoiceToProto(choice)
				if err != nil {
					t.Fatalf("decoded tool choice %#v does not encode: %v", choice, err)
				}
				if again, err := wire.ToolChoiceFromProto(back); err != nil || !reflect.DeepEqual(again, choice) {
					t.Fatalf("tool choice changed across a round trip: %#v -> %#v (%v)", choice, again, err)
				}
			}
			defs := wire.ToolDefinitionsFromProto(mc.GetTools())
			if again := wire.ToolDefinitionsFromProto(wire.ToolDefinitionsToProto(defs)); !reflect.DeepEqual(defs, again) {
				t.Fatalf("tool definitions changed across a round trip:\n%#v\n%#v", defs, again)
			}
		})
	})
}
