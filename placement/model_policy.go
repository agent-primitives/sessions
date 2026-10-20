package placement

import (
	"context"
	"fmt"
	"slices"

	"github.com/agent-primitives/sessions/api"
	v1 "github.com/agent-primitives/sessions/api/genpb"
	"github.com/agent-primitives/sessions/observability"
)

// WithModelPolicy opts this harness route into host-owned model admission. Aliases of the same
// Placer share its policy. The explicit default must belong to Allowed; no default is inferred
// from the harness. Policy registration requires a successful bounded Describe, including at
// startup; a failed stored registration stays unavailable until restart. Without this option,
// registration and execution retain their existing model behavior.
func WithModelPolicy(policy api.ModelPolicy) Option {
	policy.Allowed = slices.Clone(policy.Allowed)
	return func(p *Placer) {
		p.modelPolicy = &policy
	}
}

// ResolveModel resolves a session's requested default against this route's host policy. Without
// a policy it returns ("", false, nil): the session model remains informational as before.
func (p *Placer) ResolveModel(model string) (effective string, configured bool, err error) {
	if p.modelPolicy == nil {
		return "", false, nil
	}
	effective, err = p.modelPolicy.Resolve(model)
	return effective, true, err
}

// ValidateModelPolicy checks policy configuration and Descriptor.Models before registration.
// It provisions nothing, bounds Describe as execution admission does, and caches the result for
// this immutable Placer so pre-commit registration and Registry.Add cannot disagree on a second
// probe. A failed Placer, including a cancelled check, must be rebuilt to retry. Unconfigured
// routes perform no IO. Describe diagnostics retain correlation and classification, never backend
// messages, paths or endpoints, including errors from in-process harnesses.
func (p *Placer) ValidateModelPolicy(ctx context.Context) error {
	if p.modelPolicy == nil {
		return nil
	}
	p.policyCheck.Do(func() {
		if _, err := p.modelPolicy.Resolve(""); err != nil {
			p.policyError = err
			return
		}
		ctx = observability.EnsureRequestID(ctx)
		dctx, cancel := context.WithTimeout(ctx, describeTimeout)
		defer cancel()
		desc, err := p.Describe(dctx)
		if err != nil {
			// Backend errors have unknown origin, even in-process or when wrapping a store
			// sentinel. Keep the diagnostic structured, not the potentially private cause.
			if ctx.Err() != nil {
				p.policyError = fmt.Errorf("%w: model policy Describe: %w", ErrAdmissionInterrupted, ctx.Err())
			} else {
				p.policyError = fmt.Errorf("%w: model policy Describe failed", ErrHarnessUnavailable)
			}
			observability.Logger(ctx, p.logger).ErrorContext(ctx, "model policy Describe failed",
				"method", v1.Harness_Describe_FullMethodName, "error_kind", placementErrorKind(p.policyError), "error_category", "external")
			return
		}
		p.policyError = p.modelPolicy.ValidateDescriptor(desc)
	})
	return p.policyError
}

// WithSessionModel opts a new execution into this route's model policy using a session's model.
// The Sessions service supplies it for eligible modern sessions, including those created on
// unconfigured routes. Omitting the option preserves genuine pre-policy sessions, even empty ones.
// An empty model uses the explicit policy default; a route without policy remains unchanged.
func WithSessionModel(model string) ExecOption {
	return func(c *execConfig) { c.sessionModel = &model }
}
