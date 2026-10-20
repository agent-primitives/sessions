package harnesswire

import (
	"reflect"
	"testing"

	"github.com/agent-primitives/sessions/api"
	v1 "github.com/agent-primitives/sessions/api/genpb"
	"google.golang.org/protobuf/proto"
)

// A transport hop must preserve the host's default rather than selecting a harness default.
func TestStartModelRoundTrip(t *testing.T) {
	for _, model := range []string{"", "host-default"} {
		t.Run(model, func(t *testing.T) {
			want := &api.Start{Model: model, Config: []byte("cfg"), ResumeFromSeq: 7}
			blob, err := proto.Marshal(startToProto(want))
			if err != nil {
				t.Fatal(err)
			}
			var decoded v1.Start
			if err := proto.Unmarshal(blob, &decoded); err != nil {
				t.Fatal(err)
			}
			if got := startFromProto(&decoded); !reflect.DeepEqual(got, want) {
				t.Fatalf("round-trip start = %#v, want %#v", got, want)
			}
		})
	}
}

func TestStartModelLegacyDefaults(t *testing.T) {
	if got := startToProto(nil); !proto.Equal(got, &v1.Start{}) {
		t.Fatalf("nil start = %v, want empty proto", got)
	}
	if got := startFromProto(nil); !reflect.DeepEqual(got, &api.Start{}) {
		t.Fatalf("nil proto = %#v, want empty start", got)
	}
	blob, err := proto.Marshal(startToProto(&api.Start{}))
	if err != nil {
		t.Fatal(err)
	}
	if len(blob) != 0 {
		t.Fatalf("empty start gained wire fields: %x", blob)
	}
}
