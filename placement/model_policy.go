package placement

import (
	"context"
	"fmt"
	"slices"

	"github.com/agent-primitives/sessions/api"
)

// WithModelPolicy opts this harness route into host-owned model admission. Aliases of the same
// Placer share its policy. The explicit default must belong to Allowed; no default is inferred
// from the harness. Policy registration requires a successful bounded Describe, including at
// startup; a failed stored registration stays unavailable until restart. Without this option,
// registration and execution retain their existing model behavior.
func WithModelPolicy(policy api.ModelPolicy) Option {
	policy.Allowed = slices.Clone(policy.Allowed)
	return func(p *Placer) {
		copy := policy
		copy.Allowed = slices.Clone(policy.Allowed)
		p.modelPolicy = &copy
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
// routes perform no IO. Describe diagnostics omit peer messages, paths and endpoints.
func (p *Placer) ValidateModelPolicy(ctx context.Context) error {
	if p.modelPolicy == nil {
		return nil
	}
	p.policyCheck.Do(func() {
		if _, err := p.modelPolicy.Resolve(""); err != nil {
			p.policyError = err
			return
		}
		dctx, cancel := context.WithTimeout(ctx, describeTimeout)
		defer cancel()
		desc, err := p.Describe(dctx)
		if err != nil {
			// Registration/load errors can reach clients and logs. Classify the boundary without
			// retaining a peer's potentially private transport details or payload in diagnostics.
			if ctx.Err() != nil {
				p.policyError = fmt.Errorf("%w: model policy Describe: %w", ErrAdmissionInterrupted, ctx.Err())
			} else {
				p.policyError = fmt.Errorf("%w: model policy Describe failed", ErrHarnessUnavailable)
			}
			return
		}
		p.policyError = p.modelPolicy.ValidateDescriptor(desc)
	})
	return p.policyError
}

// WithSessionModel opts a new execution into this route's model policy using a session's model.
// The Sessions service supplies it only for sessions created with an explicit policy. Omitting
// the option preserves old sessions, including ones created before their first event existed.
// An empty model uses the explicit policy default; a route without policy remains unchanged.
func WithSessionModel(model string) ExecOption {
	return func(c *execConfig) { c.sessionModel = &model }
}
