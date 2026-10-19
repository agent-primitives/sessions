package api

import (
	"errors"
	"slices"
)

// ErrInvalidModelPolicy rejects host policy without an explicit, allowed default or with empty IDs.
var ErrInvalidModelPolicy = errors.New("model policy requires an explicit allowed default and nonempty model IDs")

// ErrModelNotAllowed rejects a session default, declared model, or mediated request outside the
// host's allowed set. A mediated rejection is execution-fatal even if the harness handles it.
var ErrModelNotAllowed = errors.New("model is not allowed by the host policy")

// ModelPolicy is host-owned, opt-in model admission for one harness route. It is configured through
// the embedder Go API, never supplied by a Sessions client or inferred from Descriptor.Models.
// IDs are opaque. Default must be nonempty and present in Allowed; empty Allowed IDs are invalid.
// Without a policy the host retains its existing model behavior.
type ModelPolicy struct {
	Default string
	Allowed []string
}

// Resolve validates the policy and returns model, or the explicit default when model is empty.
func (p ModelPolicy) Resolve(model string) (string, error) {
	if p.Default == "" || !slices.Contains(p.Allowed, p.Default) || slices.Contains(p.Allowed, "") {
		return "", ErrInvalidModelPolicy
	}
	if model == "" {
		model = p.Default
	}
	if !slices.Contains(p.Allowed, model) {
		return "", ErrModelNotAllowed
	}
	return model, nil
}

// ValidateDescriptor requires every declared model to be allowed. An empty declaration is valid;
// it never widens the host policy or supplies its default.
func (p ModelPolicy) ValidateDescriptor(desc Descriptor) error {
	if _, err := p.Resolve(""); err != nil {
		return err
	}
	for _, model := range desc.Models {
		if model == "" || !slices.Contains(p.Allowed, model) {
			return ErrModelNotAllowed
		}
	}
	return nil
}
