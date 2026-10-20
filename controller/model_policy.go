package controller

import (
	"slices"

	"github.com/agent-primitives/sessions/api"
)

// WithModelPolicy opts new executions into host-owned model admission. Default and Allowed are
// explicit; Descriptor.Models never supplies a default. Replay serves recorded calls independently
// of today's policy. Resume applies it only to live effects of invocations with a recorded Model.
// Without this option, new executions keep the harness's existing model behavior.
func WithModelPolicy(policy api.ModelPolicy) Option {
	policy.Allowed = slices.Clone(policy.Allowed)
	return func(c *Controller) {
		c.modelPolicy = &policy
	}
}

// WithModelDefault supplies a session's requested default for a new policy-enabled execution.
// Empty uses the policy default. This option alone enables nothing; Replay/Resume always restore
// the invocation's recorded default, including empty for pre-policy turns.
func WithModelDefault(model string) Option {
	return func(c *Controller) { c.modelDefault = model }
}

func modelRequest(req api.ModelRequest, model string) api.ModelRequest {
	if req.Model == "" && model != "" {
		req.Model = model
	}
	return req
}

// Only policy-aware invocations validate new effects. Old journal tails must remain resumable
// even if this host now configures a policy, including tails with an empty per-call model.
func (c *Controller) invocationPolicy(model string) *api.ModelPolicy {
	if model == "" {
		return nil
	}
	return c.modelPolicy
}

func (s *liveSink) validateModel(req api.ModelRequest) error {
	if s.modelPolicy == nil {
		return nil
	}
	_, err := s.modelPolicy.Resolve(req.Model)
	if err == nil && s.desc != nil {
		err = s.modelPolicy.ValidateDescriptor(*s.desc)
	}
	if err != nil {
		// Pre-intent refusal cannot be replayed as a handleable effect. Seal the execution even
		// if harness code catches the error, so no successfully completed unreplayable turn exists.
		s.guard.stop = err
	}
	return err
}
