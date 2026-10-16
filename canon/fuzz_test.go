package canon_test

import (
	"bytes"
	"testing"

	"github.com/gowebpki/jcs"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/agent-primitives/sessions/api"
	v1 "github.com/agent-primitives/sessions/api/genpb"
	"github.com/agent-primitives/sessions/canon"
	"github.com/agent-primitives/sessions/internal/fuzzcheck"
	"github.com/agent-primitives/sessions/wire"
)

// FuzzEventCanonical checks the properties the hash chain rests on, for any Event a journal or a
// peer can hand the host: canonicalization is deterministic, its output is already in JCS form, and
// an independent verifier that parses the canonical JSON back into an Event and canonicalizes it
// again gets the same bytes, so the same content_hash.
func FuzzEventCanonical(f *testing.F) {
	for _, blob := range fuzzcheck.JournalEvents(f) {
		f.Add(blob, int64(1))
	}
	for _, ev := range [][]byte{mustMarshal(f, goldenEvent()), mustMarshal(f, toolCallGoldenEvent())} {
		f.Add(ev, int64(9007199254740993))
	}
	f.Fuzz(func(t *testing.T, data []byte, seq int64) {
		pb := &v1.Event{}
		var first []byte
		var decodeErr, err error
		// The bound covers the protobuf decode as well as conversion and canonicalization.
		fuzzcheck.Allocs(t, len(data), func() {
			if decodeErr = proto.Unmarshal(data, pb); decodeErr != nil {
				return
			}
			first, err = canon.Event(wire.EventFromProto(pb))
		})
		if decodeErr != nil {
			return // not an Event; the host refuses a record it cannot decode
		}
		if err != nil {
			return // not canonicalizable (for example a Struct NaN); the host refuses to append it
		}
		again, err := canon.Event(wire.EventFromProto(pb))
		if err != nil || !bytes.Equal(first, again) {
			t.Fatalf("canonicalization is not deterministic:\n%s\n%s (%v)", first, again, err)
		}
		fixed, err := jcs.Transform(first)
		if err != nil || !bytes.Equal(fixed, first) {
			t.Fatalf("canonical bytes are not a JCS fixed point:\n%s\n%s (%v)", first, fixed, err)
		}

		parsed := &v1.Event{}
		if err := protojson.Unmarshal(first, parsed); err != nil {
			t.Fatalf("canonical bytes do not parse as proto3-JSON: %v\n%s", err, first)
		}
		reparsed, err := canon.Event(wire.EventFromProto(parsed))
		if err != nil || !bytes.Equal(reparsed, first) {
			t.Fatalf("canonical form does not survive a parse:\n%s\n%s (%v)", first, reparsed, err)
		}

		ev := wire.EventFromProto(pb)
		h1, err1 := canon.HashRecord("prev", seq, ev)
		h2, err2 := canon.HashRecord("prev", seq, wire.EventFromProto(parsed))
		if err1 != nil || err2 != nil || h1 != h2 {
			t.Fatalf("content_hash differs across a canonical round trip: %s %s (%v, %v)", h1, h2, err1, err2)
		}
	})
}

func mustMarshal(tb testing.TB, ev api.Event) []byte {
	tb.Helper()
	b, err := proto.Marshal(wire.EventToProto(ev))
	if err != nil {
		tb.Fatal(err)
	}
	return b
}
