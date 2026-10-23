package remote_test

import (
	"testing"

	"github.com/agent-primitives/sessions/api"
	"github.com/agent-primitives/sessions/eventlog"
	"github.com/agent-primitives/sessions/harness/echoagent"
	"github.com/agent-primitives/sessions/placement"
	"github.com/agent-primitives/sessions/runtime/remote"
)

func TestReleaseSessionPreservesSharedRemoteServerAndConnection(t *testing.T) {
	addr := serveHarness(t, echoagent.Harness{})
	b := remote.New(addr)
	t.Cleanup(func() { _ = b.Close() })
	cleanup, ok := any(b).(api.CleanupRuntime)
	if !ok {
		t.Fatal("remote backend has not opted into safe release")
	}
	p := placement.New(b, echoagent.Model)
	log := eventlog.AsStore(eventlog.New())
	if _, err := p.Exec(t.Context(), log, "retained", []api.Message{*api.TextMessage("user", "before")}, 0); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := cleanup.ReleaseSession(t.Context(), "deleted"); err != nil {
			t.Fatal(err)
		}
	}
	if desc, err := b.Describe(t.Context()); err != nil || desc.ID != "echo" {
		t.Fatalf("release closed shared Describe connection: desc=%+v error=%v", desc, err)
	}
	head, err := log.Head()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Exec(t.Context(), log, "retained", []api.Message{*api.TextMessage("user", "after")}, head); err != nil {
		t.Fatalf("shared remote server lost after release: %v", err)
	}
	records, err := log.Read(head + 1)
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range records {
		if record.Event.Kind == api.EventOutput && record.Event.Message.Text() == "echo:after" {
			return
		}
	}
	t.Fatal("retained session did not produce output after release")
}

func TestCleanupKeyIsStableRemoteConfigurationFingerprint(t *testing.T) {
	first, same, other := remote.New("harness.example:1234"), remote.New("harness.example:1234"), remote.New("harness.example:5678")
	for _, b := range []*remote.Backend{first, same, other} {
		t.Cleanup(func() { _ = b.Close() })
	}
	a, ok := any(first).(api.CleanupRuntime)
	if !ok {
		t.Fatal("remote backend has not opted into safe release")
	}
	b := any(same).(api.CleanupRuntime)
	c := any(other).(api.CleanupRuntime)
	if a.CleanupKey() == "" || a.CleanupKey() != b.CleanupKey() || a.CleanupKey() == c.CleanupKey() {
		t.Fatal("cleanup key does not identify endpoint configuration across restarts")
	}
}
