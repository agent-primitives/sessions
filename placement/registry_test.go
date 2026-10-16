package placement_test

import (
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/agent-primitives/sessions/harness/echoagent"
	"github.com/agent-primitives/sessions/placement"
	"github.com/agent-primitives/sessions/runtime/local"
)

func newPlacer(t *testing.T) *placement.Placer {
	t.Helper()
	b := local.New(echoagent.Harness{})
	t.Cleanup(func() { _ = b.Close() })
	return placement.New(b, echoagent.Model)
}

// A registry must resolve each name to its own placer, since that mapping is the whole reason
// Session.harness means anything.
func TestRegistryResolvesByName(t *testing.T) {
	echo, counter := newPlacer(t), newPlacer(t)
	r, err := placement.NewRegistry("echo", map[string]*placement.Placer{
		"echo": echo, "counter": counter,
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := r.For("counter")
	if err != nil {
		t.Fatal(err)
	}
	if got != counter {
		t.Fatal("For(counter) returned the wrong placer")
	}
	if got, err := r.For("echo"); err != nil || got != echo {
		t.Fatalf("For(echo) = %v, %v", got, err)
	}
}

// An empty name selects the default, which is what keeps a caller that does not care about
// harness selection working unchanged.
func TestRegistryEmptyNameUsesDefault(t *testing.T) {
	echo := newPlacer(t)
	r, err := placement.NewRegistry("echo", map[string]*placement.Placer{"echo": echo, "counter": newPlacer(t)})
	if err != nil {
		t.Fatal(err)
	}
	got, err := r.For("")
	if err != nil {
		t.Fatal(err)
	}
	if got != echo {
		t.Fatal("empty name did not resolve to the default harness")
	}
	if r.Default() != "echo" {
		t.Fatalf("Default() = %q, want echo", r.Default())
	}
}

// An unknown harness must fail rather than fall back. Substituting a different harness would run
// the session on something other than what was asked for, and the log would record it as intended.
func TestRegistryUnknownHarnessIsAnError(t *testing.T) {
	r, err := placement.NewRegistry("echo", map[string]*placement.Placer{"echo": newPlacer(t)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.For("nope"); !errors.Is(err, placement.ErrUnknownHarness) {
		t.Fatalf("For(nope) = %v, want ErrUnknownHarness", err)
	}
}

// A default that is not itself registered must fail at construction. Deferring it to call time
// would turn every unqualified request into a runtime error on a host that started up clean.
func TestNewRegistryRejectsUnregisteredDefault(t *testing.T) {
	_, err := placement.NewRegistry("missing", map[string]*placement.Placer{"echo": newPlacer(t)})
	if err == nil {
		t.Fatal("NewRegistry accepted a default that is not registered")
	}
}

func TestNewRegistryRejectsEmptyRegistry(t *testing.T) {
	if _, err := placement.NewRegistry("echo", nil); err == nil {
		t.Fatal("NewRegistry accepted an empty registry")
	}
}

// Names is what the daemon logs at startup and what an error message lists, so it must be sorted
// rather than map-ordered.
func TestRegistryNamesAreSorted(t *testing.T) {
	r, err := placement.NewRegistry("echo", map[string]*placement.Placer{
		"echo": newPlacer(t), "counter": newPlacer(t), "alpha": newPlacer(t),
	})
	if err != nil {
		t.Fatal(err)
	}
	got := r.Names()
	want := []string{"alpha", "counter", "echo"}
	if len(got) != len(want) {
		t.Fatalf("Names() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Names() = %v, want %v", got, want)
		}
	}
}

// Add makes a registered harness resolvable without touching the static entries, and never
// replaces an entry or takes a reserved name.
func TestRegistryAdd(t *testing.T) {
	echo := newPlacer(t)
	// echo is both static and reserved; ReservedNames lists it once.
	r, err := placement.NewRegistry("echo", map[string]*placement.Placer{"echo": echo}, placement.WithReservedNames("chat", "echo"))
	if err != nil {
		t.Fatal(err)
	}
	reg := newPlacer(t)
	if err := r.Add("reg", reg); err != nil {
		t.Fatal(err)
	}
	if got, err := r.For("reg"); err != nil || got != reg {
		t.Fatalf("For(reg) = %p, %v; want the added placer", got, err)
	}
	for _, name := range []string{"echo", "reg", "chat"} {
		if err := r.Add(name, newPlacer(t)); !errors.Is(err, placement.ErrHarnessExists) {
			t.Errorf("Add(%q) = %v, want ErrHarnessExists", name, err)
		}
	}
	if got, _ := r.For("echo"); got != echo {
		t.Fatal("Add replaced a static entry")
	}
	if !r.Static("echo") || r.Static("reg") || r.Static("chat") {
		t.Fatal("Static must report only the entries given to NewRegistry")
	}
	if !r.Reserved("echo") || !r.Reserved("chat") || r.Reserved("reg") {
		t.Fatal("Reserved must report static and reserved names only")
	}
	if got, want := r.ReservedNames(), []string{"chat", "echo"}; !slices.Equal(got, want) {
		t.Fatalf("ReservedNames = %v, want %v", got, want)
	}
	if !r.Has("reg") || r.Has("chat") {
		t.Fatal("Has must report resolvable names only")
	}
}

// An incompatible stored registration remains a known, reserved name; it must never fall through
// to the unknown-name path or be replaced by a newly constructed Placer.
func TestRegistryUnavailableName(t *testing.T) {
	r, err := placement.NewRegistry("echo", map[string]*placement.Placer{"echo": newPlacer(t)})
	if err != nil {
		t.Fatal(err)
	}
	cause := errors.New("unsupported placement")
	if err := r.MarkUnavailable("old", cause); err != nil {
		t.Fatal(err)
	}
	if !r.Has("old") || !slices.Equal(r.Names(), []string{"echo", "old"}) {
		t.Fatalf("unavailable name not reserved: Has=%t Names=%v", r.Has("old"), r.Names())
	}
	if _, err := r.For("old"); !errors.Is(err, placement.ErrHarnessUnservable) || !errors.Is(err, cause) || errors.Is(err, placement.ErrUnknownHarness) {
		t.Fatalf("For(old)=%v, want unservable wrapping cause (not unknown)", err)
	} else if err.Error() != `placement: registered harness is unservable on this host: harness "old": unsupported placement` {
		t.Fatalf("For(old)=%v, want explicit harness name and load cause", err)
	}
	if err := r.Add("old", newPlacer(t)); !errors.Is(err, placement.ErrHarnessExists) {
		t.Fatalf("Add(old)=%v, want ErrHarnessExists", err)
	}
	if _, err := r.For("missing"); !errors.Is(err, placement.ErrUnknownHarness) {
		t.Fatalf("For(missing)=%v, want ErrUnknownHarness", err)
	}
}

// Refused MarkUnavailable calls must not replace static or added Placers, overwrite an earlier
// failure, or reserve a name without the information needed to diagnose it.
func TestRegistryMarkUnavailableRefusals(t *testing.T) {
	cause := errors.New("unsupported placement")
	for _, tc := range []struct {
		name       string
		harness    string
		cause      error
		wantExists bool
	}{
		{"static", "echo", cause, true},
		{"reserved", "chat", cause, true},
		{"added", "reg", cause, true},
		{"repeat", "old", errors.New("replacement failure"), true},
		{"empty name", "", cause, false},
		{"nil cause", "new", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			echo, reg := newPlacer(t), newPlacer(t)
			r, err := placement.NewRegistry("echo", map[string]*placement.Placer{"echo": echo}, placement.WithReservedNames("chat"))
			if err != nil {
				t.Fatal(err)
			}
			if err := r.Add("reg", reg); err != nil {
				t.Fatal(err)
			}
			if err := r.MarkUnavailable("old", cause); err != nil {
				t.Fatal(err)
			}
			err = r.MarkUnavailable(tc.harness, tc.cause)
			if tc.wantExists {
				if !errors.Is(err, placement.ErrHarnessExists) {
					t.Fatalf("MarkUnavailable(%q)=%v, want ErrHarnessExists", tc.harness, err)
				}
			} else if err == nil || err.Error() != "placement: MarkUnavailable needs a name and a cause" {
				t.Fatalf("MarkUnavailable(%q)=%v, want invalid name/cause refusal", tc.harness, err)
			}
			if got, err := r.For("echo"); err != nil || got != echo {
				t.Fatalf("static Placer changed: %v, %v", got, err)
			}
			if got, err := r.For("reg"); err != nil || got != reg {
				t.Fatalf("added Placer changed: %v, %v", got, err)
			}
			if _, err := r.For("old"); !errors.Is(err, placement.ErrHarnessUnservable) || !errors.Is(err, cause) {
				t.Fatalf("original load failure changed: %v", err)
			}
			if !slices.Equal(r.Names(), []string{"echo", "old", "reg"}) {
				t.Fatalf("refused call reserved a new name: %v", r.Names())
			}
		})
	}
}

// Lookups run concurrently with Add on a live host; -race checks the locking.
func TestRegistryAddIsSafeAlongsideLookups(t *testing.T) {
	r, err := placement.NewRegistry("echo", map[string]*placement.Placer{"echo": newPlacer(t)})
	if err != nil {
		t.Fatal(err)
	}
	p := newPlacer(t)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 100; i++ {
			_ = r.Add(fmt.Sprintf("h%d", i), p)
		}
	}()
	for i := 0; i < 100; i++ {
		_, _ = r.For(fmt.Sprintf("h%d", i))
		_ = r.Names()
		_ = r.Has("h0")
	}
	<-done
	if len(r.Names()) != 101 {
		t.Fatalf("names = %d, want 101", len(r.Names()))
	}
}

// A registry shares one set of per-session checkpoint state across its placers. A placer moved into
// a second registry would split that set, so a checkpoint through one registry would miss turns run
// through the other; NewRegistry refuses it instead.
func TestNewRegistryRejectsAPlacerAlreadyInAnotherRegistry(t *testing.T) {
	echo := newPlacer(t)
	if _, err := placement.NewRegistry("echo", map[string]*placement.Placer{"echo": echo}); err != nil {
		t.Fatal(err)
	}
	if _, err := placement.NewRegistry("echo", map[string]*placement.Placer{"echo": echo}); err == nil {
		t.Fatal("NewRegistry accepted a placer that already belongs to another registry")
	}
}

// The same placer under two names is one placer, so it shares the set with itself.
func TestNewRegistryAcceptsOnePlacerUnderTwoNames(t *testing.T) {
	p := newPlacer(t)
	if _, err := placement.NewRegistry("a", map[string]*placement.Placer{"a": p, "b": p}); err != nil {
		t.Fatal(err)
	}
}
