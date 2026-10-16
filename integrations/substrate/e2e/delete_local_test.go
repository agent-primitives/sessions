package e2e

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/agent-primitives/sessions/api"
	"github.com/agent-primitives/sessions/eventlog"
	"github.com/agent-primitives/sessions/harness/echoagent"
	"github.com/agent-primitives/sessions/runtime/local"
)

// Run the new audit/reopen/refusal fixture locally with real gRPC/SQLite/harness transport. This
// exercises the logic that can be checked offline, not substrate actor/pin or microVM qualification.
func TestDeleteFixtureLocalAuditAndHierarchy(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		name := "modern"
		if legacy {
			name = "markerless"
		}
		t.Run(name, func(t *testing.T) {
			backend := local.New(echoagent.Harness{})
			t.Cleanup(func() { _ = backend.Close() })
			b := &deleteRuntime{Backend: backend}
			a := newDeleteAPI(t, b)
			ctx := t.Context()
			parent := a.create(t, ctx, "local-parent")
			if legacy {
				var records []eventlog.Record
				if err := json.Unmarshal(legacyCrashedOutput, &records); err != nil {
					t.Fatal(err)
				}
				log := a.store.Session(parent)
				fence, err := log.NewFence()
				if err != nil {
					t.Fatal(err)
				}
				for _, r := range records {
					if _, err := log.Append(r.Seq-1, fence, r.Event); err != nil {
						t.Fatal(err)
					}
				}
				if _, err := a.client.Resume(ctx, parent, false); err != nil {
					t.Fatal(err)
				}
			} else {
				a.exec(t, ctx, parent, "before")
			}
			children := a.fork(t, ctx, parent, 2)
			grandchild := a.fork(t, ctx, children[0], 1)[0]
			prefix := a.records(t, grandchild)
			deletionStatus(t, ctx, a, parent, true)
			a.pending(t, ctx, b, parent)
			deletionStatus(t, ctx, a, children[0], false)
			a.pending(t, ctx, b, children[0])
			for _, uid := range []string{children[1], grandchild} {
				if _, err := a.client.Resume(ctx, uid, false); err != nil {
					t.Fatal(err)
				}
				if got := a.exec(t, ctx, uid, "survive"); got != "echo:survive" {
					t.Fatalf("descendant output=%q", got)
				}
			}
			assertDeletePrefix(t, prefix, a.records(t, grandchild))
			own := a.fork(t, ctx, grandchild, 1)[0]
			if got := a.exec(t, ctx, own, "own"); got != "echo:own" {
				t.Fatalf("own fork output=%q", got)
			}
			if err := eventlog.CheckWritable(a.store.Session(parent)); !errors.Is(err, eventlog.ErrDeleted) {
				t.Fatalf("deleted local writable=%v", err)
			}
			for _, r := range a.records(t, grandchild) {
				if r.Event.Kind == api.EventLifecycle && r.Event.Lifecycle.Kind == api.LifecycleDelete {
					t.Fatal("ancestor DELETE cascaded into grandchild")
				}
			}
		})
	}
}
