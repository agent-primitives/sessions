package placement

import (
	"reflect"
	"testing"

	"github.com/agent-primitives/sessions/api"
	"github.com/agent-primitives/sessions/eventlog"
)

// RT-4i hands the backend the session's own latest SUSPEND ref as recorded, or none. A SUSPEND with
// no ExternalURI (runtime/local writes {Local: uid}) is still that session's record, while
// lastSuspendRef's fallback, which names the session too, is not a record and must read as none.
func TestRecordedCheckpointIsTheOwnSuspendAsRecorded(t *testing.T) {
	const uid = "child"
	suspend := func(ref api.SnapshotRef) eventlog.Record {
		return eventlog.Record{Event: api.Event{Kind: api.EventLifecycle,
			Lifecycle: &api.Lifecycle{Kind: api.LifecycleSuspend, Snapshot: &ref}}}
	}
	durable := api.SnapshotRef{Local: uid, ExternalURI: "snap://own", Memory: true}
	cases := []struct {
		name string
		recs []eventlog.Record
		want api.SnapshotRef
	}{
		{name: "no suspend", recs: nil, want: api.SnapshotRef{}},
		{name: "only a parent's suspend in the copied prefix",
			recs: []eventlog.Record{suspend(api.SnapshotRef{Local: "parent", ExternalURI: "snap://parent"})},
			want: api.SnapshotRef{}},
		{name: "own suspend with no external uri (runtime/local)",
			recs: []eventlog.Record{suspend(api.SnapshotRef{Local: uid})},
			want: api.SnapshotRef{Local: uid}},
		{name: "own durable suspend",
			recs: []eventlog.Record{suspend(durable)},
			want: durable},
		{name: "latest own suspend after a parent's",
			recs: []eventlog.Record{suspend(durable), suspend(api.SnapshotRef{Local: "parent", ExternalURI: "snap://parent"})},
			want: durable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := recordedCheckpoint(tc.recs, uid); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("recordedCheckpoint = %+v, want %+v", got, tc.want)
			}
		})
	}
	// The fallback is still what Restore gets when nothing is recorded.
	if got, want := lastSuspendRef(nil, uid), (api.SnapshotRef{Local: uid}); !reflect.DeepEqual(got, want) {
		t.Errorf("lastSuspendRef fallback = %+v, want %+v", got, want)
	}
}
