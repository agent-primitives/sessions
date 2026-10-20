package session_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/agent-primitives/sessions/api"
	v1 "github.com/agent-primitives/sessions/api/genpb"
	"github.com/agent-primitives/sessions/placement"
	"github.com/agent-primitives/sessions/runtime/local"
	"github.com/agent-primitives/sessions/sqlitelog"
)

type registrationBarrierHarness struct {
	entered chan struct{}
	release chan struct{}
}

func (h registrationBarrierHarness) Describe(ctx context.Context) (api.Descriptor, error) {
	close(h.entered)
	select {
	case <-h.release:
		return api.Descriptor{Models: []string{"m"}}, nil
	case <-ctx.Done():
		return api.Descriptor{}, ctx.Err()
	}
}
func (registrationBarrierHarness) Run(context.Context, *api.Start, api.EventSink) error { return nil }

// A factory or Describe can succeed after the caller cancels; do not commit its immutable row.
func TestRegisterHarnessCancellationBeforeCommit(t *testing.T) {
	for _, policy := range []bool{false, true} {
		t.Run(fmt.Sprintf("policy=%t", policy), func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var releases atomic.Int32
			factory := func(_ string, spec *v1.HarnessSpec) (*placement.Placer, func(), error) {
				cancel()
				// This local harness returns a successful Describe even with an expired context.
				b := local.New(sessionPolicyHarness{})
				opts := []placement.Option{placement.WithDescriptorID(spec.GetDescriptorId())}
				if policy {
					opts = append(opts, placement.WithModelPolicy(api.ModelPolicy{Default: "m", Allowed: []string{"m"}}))
				}
				return placement.New(b, nil, opts...), func() { releases.Add(1); _ = b.Close() }, nil
			}
			h := startRegistryHostWith(t, ":memory:", factory)
			_, err := h.reg.RegisterHarness(ctx, &v1.RegisterHarnessRequest{Name: "cancelled", Spec: remoteSpec("127.0.0.1:9000")})
			if status.Code(err) != codes.Canceled || !errors.Is(err, context.Canceled) || !errors.Is(err, placement.ErrAdmissionInterrupted) {
				t.Errorf("registration ignored cancellation before commit: %v", err)
			}
			if releases.Load() != 1 {
				t.Errorf("uncommitted factory resource not released: %d", releases.Load())
			}
			if _, err := h.store.Harness("cancelled"); !errors.Is(err, sqlitelog.ErrHarnessNotFound) {
				t.Errorf("cancelled registration persisted: %v", err)
			}
		})
	}
}

// A blocked policy Describe must not trap a cancelled registration behind serialization, nor may
// that cancelled call later build a Placer or persist a row (including a route with no policy).
func TestRegisterHarnessCancellationWhilePolicyDescribeBlocked(t *testing.T) {
	for _, secondPolicy := range []bool{false, true} {
		for _, deadline := range []bool{false, true} {
			t.Run(fmt.Sprintf("secondPolicy=%t/deadline=%t", secondPolicy, deadline), func(t *testing.T) {
				barrier := registrationBarrierHarness{entered: make(chan struct{}), release: make(chan struct{})}
				var releaseOnce sync.Once
				release := func() { releaseOnce.Do(func() { close(barrier.release) }) }
				defer release()
				var secondFactories atomic.Int32
				factory := func(name string, spec *v1.HarnessSpec) (*placement.Placer, func(), error) {
					var harness api.Harness = sessionPolicyHarness{}
					switch name {
					case "first":
						harness = barrier
					case "second":
						secondFactories.Add(1)
					}
					b := local.New(harness)
					opts := []placement.Option{placement.WithDescriptorID(spec.GetDescriptorId())}
					if name == "first" || secondPolicy {
						opts = append(opts, placement.WithModelPolicy(api.ModelPolicy{Default: "m", Allowed: []string{"m"}}))
					}
					return placement.New(b, nil, opts...), func() { _ = b.Close() }, nil
				}
				h := startRegistryHostWith(t, ":memory:", factory)
				firstDone := make(chan error, 1)
				go func() {
					_, err := h.reg.RegisterHarness(t.Context(), &v1.RegisterHarnessRequest{Name: "first", Spec: remoteSpec("127.0.0.1:9000")})
					firstDone <- err
				}()
				select {
				case <-barrier.entered:
				case <-time.After(5 * time.Second):
					t.Fatal("first registration did not enter policy Describe")
				}
				ctx, cancel := context.WithCancel(t.Context())
				want, cause := codes.Canceled, context.Canceled
				if deadline {
					cancel()
					ctx, cancel = context.WithTimeout(t.Context(), 20*time.Millisecond)
					want, cause = codes.DeadlineExceeded, context.DeadlineExceeded
				}
				defer cancel()
				secondDone, started := make(chan error, 1), make(chan struct{})
				go func() {
					close(started)
					_, err := h.reg.RegisterHarness(ctx, &v1.RegisterHarnessRequest{Name: "second", Spec: remoteSpec("127.0.0.1:9000")})
					secondDone <- err
				}()
				<-started
				if !deadline {
					cancel()
				}
				<-ctx.Done()
				var secondErr error
				returned := false
				select {
				case secondErr = <-secondDone:
					returned = true
				case <-time.After(time.Second):
					t.Error("cancelled registration blocked behind another registration's Describe")
				}
				release()
				select {
				case err := <-firstDone:
					if err != nil {
						t.Fatalf("owner registration failed: %v", err)
					}
				case <-time.After(5 * time.Second):
					t.Fatal("owner registration did not finish after release")
				}
				if !returned {
					select {
					case secondErr = <-secondDone:
					case <-time.After(5 * time.Second):
						t.Fatal("cancelled registration did not drain after release")
					}
				}
				if status.Code(secondErr) != want || !errors.Is(secondErr, cause) || !errors.Is(secondErr, placement.ErrAdmissionInterrupted) {
					t.Errorf("cancelled registration = %v, want %s retaining %v and admission class", secondErr, want, cause)
				}
				if secondFactories.Load() != 0 {
					t.Errorf("cancelled registration called factory %d times", secondFactories.Load())
				}
				if _, err := h.store.Harness("second"); !errors.Is(err, sqlitelog.ErrHarnessNotFound) {
					t.Errorf("cancelled registration persisted: %v", err)
				}
				if _, err := h.reg.RegisterHarness(t.Context(), &v1.RegisterHarnessRequest{Name: "third", Spec: remoteSpec("127.0.0.1:9000")}); err != nil {
					t.Fatalf("cancelled waiter stranded serialization: %v", err)
				}
			})
		}
	}
}
