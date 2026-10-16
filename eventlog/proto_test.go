package eventlog_test

import (
	"reflect"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/agent-primitives/sessions/api"
	v1 "github.com/agent-primitives/sessions/api/genpb"
	"github.com/agent-primitives/sessions/eventlog"
)

func TestDeleteRecordProtoPreservesHash(t *testing.T) {
	// This is serialization coverage, not a store deletion primitive: no tombstone is applied.
	log := eventlog.New()
	r, err := log.Append(0, log.NewFence(), api.Event{
		Kind: api.EventLifecycle, SchemaVersion: 1,
		Timestamp: time.Unix(1_700_000_000, 123456789).UTC(),
		Lifecycle: &api.Lifecycle{Kind: api.LifecycleKind("DELETE")},
	})
	if err != nil {
		t.Fatal(err)
	}
	const wantHash = "bfbda987672b922ac194edadeec95fad94093740ab2a6bf9786f09c4a56d3d90"
	if r.Hash != wantHash {
		t.Errorf("DELETE record hash = %s, want %s", r.Hash, wantHash)
	}
	blob, err := proto.Marshal(eventlog.RecordToProto(r))
	if err != nil {
		t.Fatal(err)
	}
	var decoded v1.LogRecord
	if err := proto.Unmarshal(blob, &decoded); err != nil {
		t.Fatal(err)
	}
	got := eventlog.RecordFromProto(&decoded)
	if !reflect.DeepEqual(got, r) {
		t.Fatalf("DELETE record round-trip = %#v (lifecycle %#v), want %#v", got, got.Event.Lifecycle, r)
	}
	if err := log.Verify(); err != nil {
		t.Fatal(err)
	}
}

func TestRecordRoundTrip(t *testing.T) {
	r := eventlog.Record{
		Seq: 7, PrevHash: "sha256:prev", Hash: "sha256:cur", Fence: 3,
		Event: api.Event{
			Kind:    api.EventOutput,
			Message: api.TextMessage("assistant", "hi"),
			Actor:   api.IdentityRef{Principal: "agent://a", Issuer: "entra", Subject: "s"},
		},
	}
	got := eventlog.RecordFromProto(eventlog.RecordToProto(r))
	if !reflect.DeepEqual(r, got) {
		t.Fatalf("record round-trip mismatch\n want: %#v\n got:  %#v", r, got)
	}
}
