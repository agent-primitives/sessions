package local_test

import (
	"testing"

	"github.com/agent-primitives/sessions/api"
	"github.com/agent-primitives/sessions/eventlog"
	"github.com/agent-primitives/sessions/harness/echoagent"
	"github.com/agent-primitives/sessions/placement"
	"github.com/agent-primitives/sessions/runtime/local"
)

func TestReleaseSessionPreservesSharedLocalServer(t *testing.T) {
	b := local.New(echoagent.Harness{})
	t.Cleanup(func() { _ = b.Close() })
	cleanup, ok := any(b).(api.CleanupRuntime)
	if !ok {
		t.Fatal("local backend has not opted into safe release")
	}
	other := newBackend(t)
	otherCleanup, ok := any(other).(api.CleanupRuntime)
	if !ok || cleanup.CleanupKey() != "local" || otherCleanup.CleanupKey() != cleanup.CleanupKey() {
		t.Fatal("local key must be stable across harnesses: there are no per-session resources")
	}
	p := placement.New(b, echoagent.Model)
	log := eventlog.AsStore(eventlog.New())
	first, err := p.Exec(t.Context(), log, "retained", []api.Message{*api.TextMessage("user", "before")}, 0)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := cleanup.ReleaseSession(t.Context(), "deleted"); err != nil {
			t.Fatal(err)
		}
	}
	head, err := log.Head()
	if err != nil {
		t.Fatal(err)
	}
	second, err := p.Exec(t.Context(), log, "retained", []api.Message{*api.TextMessage("user", "after")}, head)
	if err != nil {
		t.Fatalf("retained session lost shared server after release: %v", err)
	}
	if first.Address != second.Address {
		t.Fatal("release restarted the shared local server")
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
