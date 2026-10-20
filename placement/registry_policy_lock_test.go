package placement_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/agent-primitives/sessions/api"
	"github.com/agent-primitives/sessions/placement"
	"github.com/agent-primitives/sessions/runtime/local"
)

type registryPolicyHarness struct {
	describe func() (api.Descriptor, error)
}

func (h registryPolicyHarness) Describe(context.Context) (api.Descriptor, error) {
	return h.describe()
}

func (registryPolicyHarness) Run(context.Context, *api.Start, api.EventSink) error {
	return nil
}

func registryPolicyPlacer(t *testing.T, describe func() (api.Descriptor, error)) *placement.Placer {
	t.Helper()
	b := local.New(registryPolicyHarness{describe: describe})
	t.Cleanup(func() { _ = b.Close() })
	return placement.New(b, nil, placement.WithModelPolicy(api.ModelPolicy{
		Default: "m", Allowed: []string{"m"},
	}))
}

// Holding the publication lock during Describe would block an unrelated existing route.
func TestRegistryAddPolicyDescribeDoesNotBlockLookup(t *testing.T) {
	base := newPlacer(t)
	r, err := placement.NewRegistry("base", map[string]*placement.Placer{"base": base})
	if err != nil {
		t.Fatal(err)
	}
	started, release := make(chan struct{}), make(chan struct{})
	p := registryPolicyPlacer(t, func() (api.Descriptor, error) {
		close(started)
		<-release
		return api.Descriptor{ID: "slow", Models: []string{"m"}}, nil
	})
	added := make(chan error, 1)
	go func() { added <- r.Add("slow", p) }()
	<-started

	var got *placement.Placer
	var lookupErr error
	lookedUp := make(chan struct{})
	go func() {
		got, lookupErr = r.For("base")
		close(lookedUp)
	}()
	defer func() {
		close(release)
		if err := <-added; err != nil {
			t.Errorf("Add(slow) = %v", err)
		}
		<-lookedUp
		if got, err := r.For("slow"); err != nil || got != p {
			t.Errorf("For(slow) = %p, %v; want added placer", got, err)
		}
	}()

	// Channels establish the blocked Describe; this bound asserts lookup liveness only.
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	select {
	case <-lookedUp:
		if lookupErr != nil || got != base {
			t.Fatalf("For(base) = %p, %v; want existing placer", got, lookupErr)
		}
	case <-ctx.Done():
		t.Fatal("For(base) blocked while policy Describe was in flight")
	}
}

// A name claimed during policy IO must win without attaching the refused Placer.
func TestRegistryAddPolicyRechecksNameBeforePublish(t *testing.T) {
	for _, unavailable := range []bool{false, true} {
		name := "served"
		if unavailable {
			name = "unavailable"
		}
		t.Run(name, func(t *testing.T) {
			base, winner := newPlacer(t), newPlacer(t)
			r, err := placement.NewRegistry("base", map[string]*placement.Placer{"base": base})
			if err != nil {
				t.Fatal(err)
			}
			cause := errors.New("cannot load harness")
			var claimErr error
			p := registryPolicyPlacer(t, func() (api.Descriptor, error) {
				if unavailable {
					claimErr = r.MarkUnavailable("contested", cause)
				} else {
					claimErr = r.Add("contested", winner)
				}
				return api.Descriptor{ID: "candidate", Models: []string{"m"}}, nil
			})
			if err := r.Add("contested", p); !errors.Is(err, placement.ErrHarnessExists) {
				t.Fatalf("Add(contested) = %v; want ErrHarnessExists", err)
			}
			if claimErr != nil {
				t.Fatalf("claim during Describe = %v", claimErr)
			}
			got, err := r.For("contested")
			if unavailable {
				if got != nil || !errors.Is(err, placement.ErrHarnessUnservable) || !errors.Is(err, cause) {
					t.Fatalf("unavailable route changed: %p, %v", got, err)
				}
			} else if err != nil || got != winner {
				t.Fatalf("winning route changed: %p, %v", got, err)
			}
			if _, err := placement.NewRegistry("candidate", map[string]*placement.Placer{"candidate": p}); err != nil {
				t.Fatalf("refused placer was mutated: %v", err)
			}
		})
	}
}

func TestRegistryAddPolicySkipsDescribeForExistingNames(t *testing.T) {
	r, err := placement.NewRegistry("base", map[string]*placement.Placer{"base": newPlacer(t)},
		placement.WithReservedNames("reserved"))
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Add("added", newPlacer(t)); err != nil {
		t.Fatal(err)
	}
	if err := r.MarkUnavailable("unavailable", errors.New("cannot load harness")); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"base", "reserved", "added", "unavailable"} {
		t.Run(name, func(t *testing.T) {
			p := registryPolicyPlacer(t, func() (api.Descriptor, error) {
				t.Error("Describe called for an already conflicting name")
				return api.Descriptor{}, errors.New("unexpected Describe")
			})
			if err := r.Add(name, p); !errors.Is(err, placement.ErrHarnessExists) {
				t.Fatalf("Add(%q) = %v; want ErrHarnessExists", name, err)
			}
		})
	}
}

func TestRegistryAddPolicyRefusalDoesNotPublishOrAttach(t *testing.T) {
	r, err := placement.NewRegistry("base", map[string]*placement.Placer{"base": newPlacer(t)})
	if err != nil {
		t.Fatal(err)
	}
	p := registryPolicyPlacer(t, func() (api.Descriptor, error) {
		return api.Descriptor{ID: "refused", Models: []string{"outside"}}, nil
	})
	if err := r.Add("refused", p); !errors.Is(err, api.ErrModelNotAllowed) {
		t.Fatalf("Add(refused) = %v; want ErrModelNotAllowed", err)
	}
	if _, err := r.For("refused"); !errors.Is(err, placement.ErrUnknownHarness) {
		t.Fatalf("refused route was published: %v", err)
	}
	if _, err := placement.NewRegistry("refused", map[string]*placement.Placer{"refused": p}); !errors.Is(err, api.ErrModelNotAllowed) {
		t.Fatalf("refused placer was attached before policy refusal: %v", err)
	}
}
