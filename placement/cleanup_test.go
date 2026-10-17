package placement

import (
	"errors"
	"reflect"
	"testing"

	"github.com/agent-primitives/sessions/eventlog"
)

func TestRegistryReleaseSessionUsesOnlyExactOptInConfiguration(t *testing.T) {
	var trace []string
	unsupported := &allocationBackend{trace: &trace}
	a := &safeAllocationBackend{allocationBackend: &allocationBackend{trace: &trace}, key: "endpoint-A"}
	b := &safeAllocationBackend{allocationBackend: &allocationBackend{trace: &trace}, key: "endpoint-B"}
	r, err := NewRegistry("default", map[string]*Placer{
		"default": New(unsupported, nil), "a": New(a, nil),
	})
	if err != nil {
		t.Fatal(err)
	}
	p, _ := r.For("a")
	if err := r.Add("alias", p); err != nil {
		t.Fatal(err)
	}
	if err := r.Add("b", New(b, nil)); err != nil {
		t.Fatal(err)
	}
	if err := r.MarkUnavailable("unserved", errors.New("configuration missing")); err != nil {
		t.Fatal(err)
	}

	if err := r.ReleaseSession(t.Context(), "endpoint-B", "deleted"); err != nil {
		t.Fatal(err)
	}
	if err := r.ReleaseSession(t.Context(), "endpoint-A", "deleted"); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(a.released, []string{"deleted"}) || !reflect.DeepEqual(b.released, []string{"deleted"}) {
		t.Fatalf("release routed by harness/default or called aliases twice: A=%v B=%v", a.released, b.released)
	}
	for _, key := range []string{"", "missing", "default", "unserved"} {
		if err := r.ReleaseSession(t.Context(), key, "deleted"); !errors.Is(err, ErrCleanupUnavailable) {
			t.Errorf("key %q = %v, want ErrCleanupUnavailable", key, err)
		}
	}
	if unsupported.describes+a.describes+b.describes != 0 {
		t.Fatal("cleanup attempted harness Describe")
	}
	if unsupported.stops != 0 || a.stops != 0 || b.stops != 0 || len(trace) != 0 {
		t.Fatalf("cleanup reached generic placement: trace=%v Stop=%d/%d/%d", trace, unsupported.stops, a.stops, b.stops)
	}
	if len(a.released) != 1 || len(b.released) != 1 {
		t.Fatal("missing key fell back to served runtime")
	}
}

func TestRegistryReleaseSessionPreservesFailureAndCanRunUnderGuard(t *testing.T) {
	var trace []string
	cause := errors.New("detach failed")
	b := &safeAllocationBackend{allocationBackend: &allocationBackend{trace: &trace}, key: "endpoint", releaseErr: cause}
	r, err := NewRegistry("default", map[string]*Placer{"default": New(b, nil)})
	if err != nil {
		t.Fatal(err)
	}
	err = r.WithSessionGuard("deleted", func() error {
		return r.ReleaseSession(t.Context(), "endpoint", "deleted")
	})
	if !errors.Is(err, cause) || errors.Is(err, ErrCleanupUnavailable) {
		t.Fatalf("release = %v, want backend cause", err)
	}
	if err := r.WithSessionGuard("deleted", func() error { return nil }); err != nil {
		t.Fatalf("release leaked exclusion: %v", err)
	}
}

func TestRegistryReleaseEmptyKeyNeverSelectsAnUnreleasableAllocation(t *testing.T) {
	var trace []string
	b := &safeAllocationBackend{allocationBackend: &allocationBackend{trace: &trace}}
	r, err := NewRegistry("default", map[string]*Placer{"default": New(b, nil)})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.ReleaseSession(t.Context(), "", "s"); !errors.Is(err, ErrCleanupUnavailable) {
		t.Fatalf("empty key = %v", err)
	}
	if len(b.released) != 0 {
		t.Fatal("empty ownership evidence admitted cleanup")
	}
}

func TestRegistryReleaseDuplicateSafeKeysReleaseOnce(t *testing.T) {
	var trace []string
	a := &safeAllocationBackend{allocationBackend: &allocationBackend{trace: &trace}, key: "local"}
	b := &safeAllocationBackend{allocationBackend: &allocationBackend{trace: &trace}, key: "local"}
	r, err := NewRegistry("a", map[string]*Placer{"a": New(a, nil), "b": New(b, nil)})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.ReleaseSession(t.Context(), "local", "deleted"); err != nil {
		t.Fatal(err)
	}
	if len(a.released)+len(b.released) != 1 {
		t.Fatalf("shared no-resource key released %d times", len(a.released)+len(b.released))
	}
}

// Wrapping only Store intentionally selects standalone, non-recording rollback semantics.
// Preserve the optional deletion preflight, but do not claim durable allocation coverage.
type legacyForkLog struct{ eventlog.Store }

func (l legacyForkLog) CheckWritable() error   { return eventlog.CheckWritable(l.Store) }
func (l legacyForkLog) CheckAdmissible() error { return eventlog.CheckAdmissible(l.Store) }
