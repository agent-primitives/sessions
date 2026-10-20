package placement_test

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/agent-primitives/sessions/api"
	"github.com/agent-primitives/sessions/eventlog"
	"github.com/agent-primitives/sessions/placement"
	"github.com/agent-primitives/sessions/runtime/local"
)

type policyHarness struct {
	models     []string
	describes  atomic.Int32
	err        error
	startModel string
}

func (h *policyHarness) Describe(context.Context) (api.Descriptor, error) {
	h.describes.Add(1)
	return api.Descriptor{ID: "h", Models: h.models}, h.err
}
func (h *policyHarness) Run(_ context.Context, start *api.Start, _ api.EventSink) error {
	h.startModel = start.Model
	return nil
}

func TestRegistryModelPolicyAdmission(t *testing.T) {
	for _, tc := range []struct {
		name   string
		models []string
		policy *api.ModelPolicy
		want   error
		calls  int32
	}{
		{name: "legacy does not describe"},
		{name: "empty declaration", policy: &api.ModelPolicy{Default: "m", Allowed: []string{"m"}}, calls: 1},
		{name: "subset", models: []string{"m"}, policy: &api.ModelPolicy{Default: "m", Allowed: []string{"m", "other"}}, calls: 1},
		{name: "outside", models: []string{"outside"}, policy: &api.ModelPolicy{Default: "m", Allowed: []string{"m"}}, want: api.ErrModelNotAllowed, calls: 1},
		{name: "no inferred default", models: []string{"m"}, policy: &api.ModelPolicy{Allowed: []string{"m"}}, want: api.ErrInvalidModelPolicy},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &policyHarness{models: tc.models}
			backend := local.New(h)
			t.Cleanup(func() { _ = backend.Close() })
			var opts []placement.Option
			if tc.policy != nil {
				opts = append(opts, placement.WithModelPolicy(*tc.policy))
			}
			p := placement.New(backend, nil, opts...)
			r, err := placement.NewRegistry("h", map[string]*placement.Placer{"h": p})
			if !errors.Is(err, tc.want) || (tc.want != nil && r != nil) {
				t.Fatalf("NewRegistry = %v, %v; want %v", r, err, tc.want)
			}
			if got := h.describes.Load(); got != tc.calls {
				t.Fatalf("Describe calls = %d; want %d", got, tc.calls)
			}
		})
	}
}

func TestModelPolicyCopiesAndChecksDynamicRegistrationBeforeMutation(t *testing.T) {
	good := local.New(&policyHarness{})
	t.Cleanup(func() { _ = good.Close() })
	r, err := placement.NewRegistry("base", map[string]*placement.Placer{"base": placement.New(good, nil)})
	if err != nil {
		t.Fatal(err)
	}
	allowed := []string{"m"}
	h := &policyHarness{models: []string{"m"}}
	backend := local.New(h)
	t.Cleanup(func() { _ = backend.Close() })
	option := placement.WithModelPolicy(api.ModelPolicy{Default: "m", Allowed: allowed})
	p := placement.New(backend, nil, option)
	allowed[0] = "outside"
	if err := p.ValidateModelPolicy(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := r.Add("added", p); err != nil {
		t.Fatal(err)
	}
	if h.describes.Load() != 1 {
		t.Fatalf("successful admission was not reused: %d", h.describes.Load())
	}
	if got, configured, err := p.ResolveModel(""); err != nil || !configured || got != "m" {
		t.Fatalf("ResolveModel = %q, %v, %v", got, configured, err)
	}
	badBackend := local.New(&policyHarness{models: []string{"outside"}})
	t.Cleanup(func() { _ = badBackend.Close() })
	bad := placement.New(badBackend, nil, option)
	if err := r.Add("refused", bad); !errors.Is(err, api.ErrModelNotAllowed) {
		t.Fatalf("Add = %v", err)
	}
	if _, err := r.For("refused"); !errors.Is(err, placement.ErrUnknownHarness) {
		t.Fatalf("refused route exists: %v", err)
	}
}

func TestModelPolicyDescribeFailureIsPayloadFreeAndRetainsCancellation(t *testing.T) {
	const private = "dial unix /private/harness.sock?token=secret"
	for _, canceled := range []bool{false, true} {
		h := &policyHarness{err: errors.New(private)}
		backend := local.New(h)
		t.Cleanup(func() { _ = backend.Close() })
		p := placement.New(backend, nil, placement.WithModelPolicy(api.ModelPolicy{Default: "m", Allowed: []string{"m"}}))
		ctx, cancel := context.WithCancel(t.Context())
		if canceled {
			cancel()
		}
		err := p.ValidateModelPolicy(ctx)
		cancel()
		want := placement.ErrHarnessUnavailable
		if canceled {
			want = placement.ErrAdmissionInterrupted
		}
		if !errors.Is(err, want) || strings.Contains(err.Error(), private) {
			t.Fatalf("Describe error = %v, want safe %v", err, want)
		}
		if canceled && !errors.Is(err, context.Canceled) {
			t.Fatalf("caller cause lost: %v", err)
		}
		if again := p.ValidateModelPolicy(t.Context()); again != err || h.describes.Load() != 1 {
			t.Fatalf("failed admission retried: %v/%d", again, h.describes.Load())
		}
	}
}

type policyBackend struct {
	*local.Backend
	creates int
}

func (b *policyBackend) Create(ctx context.Context, spec *api.SessionSpec) (api.Incarnation, error) {
	b.creates++
	return b.Backend.Create(ctx, spec)
}

func TestPlacedSessionModelAdmissionBeforeProvisioning(t *testing.T) {
	for _, tc := range []struct {
		name, requested, want string
		opted, policy         bool
		wantErr               error
	}{
		{name: "policy default", opted: true, policy: true, want: "m"},
		{name: "explicit default", opted: true, policy: true, requested: "other", want: "other"},
		{name: "invalid", opted: true, policy: true, requested: "outside", wantErr: api.ErrModelNotAllowed},
		{name: "old session", policy: true},
		{name: "unconfigured route", opted: true, requested: "outside"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &policyHarness{models: []string{"m"}}
			backend := &policyBackend{Backend: local.New(h)}
			t.Cleanup(func() { _ = backend.Close() })
			var options []placement.Option
			if tc.policy {
				options = append(options, placement.WithModelPolicy(api.ModelPolicy{Default: "m", Allowed: []string{"m", "other"}}))
			}
			p := placement.New(backend, nil, options...)
			log := eventlog.AsStore(eventlog.New())
			var execOptions []placement.ExecOption
			if tc.opted {
				execOptions = append(execOptions, placement.WithSessionModel(tc.requested))
			}
			_, err := p.Exec(t.Context(), log, "session", nil, 0, execOptions...)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("Exec = %v; want %v", err, tc.wantErr)
			}
			if tc.wantErr != nil {
				head, _ := log.Head()
				if backend.creates != 0 || head != 0 {
					t.Fatalf("refused model provisioned/journaled: %d/%d", backend.creates, head)
				}
				return
			}
			recs, _ := log.Read(1)
			if h.startModel != tc.want || recs[0].Event.ExecutionStart.Model != tc.want {
				t.Fatalf("defaults Start/journal = %q/%q; want %q", h.startModel, recs[0].Event.ExecutionStart.Model, tc.want)
			}
		})
	}
}
