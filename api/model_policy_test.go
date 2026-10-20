package api_test

import (
	"errors"
	"testing"

	"github.com/agent-primitives/sessions/api"
)

func TestModelPolicyResolvesOnlyExplicitlyAllowedModels(t *testing.T) {
	policy := api.ModelPolicy{Default: "primary", Allowed: []string{"alternate", "primary"}}
	for _, tc := range []struct{ requested, want string }{{"", "primary"}, {"primary", "primary"}, {"alternate", "alternate"}} {
		got, err := policy.Resolve(tc.requested)
		if err != nil || got != tc.want {
			t.Fatalf("Resolve(%q) = %q, %v; want %q", tc.requested, got, err, tc.want)
		}
	}
	for _, model := range []string{"unknown", " primary "} {
		if _, err := policy.Resolve(model); !errors.Is(err, api.ErrModelNotAllowed) {
			t.Fatalf("Resolve(%q) = %v; want ErrModelNotAllowed", model, err)
		}
	}
}

func TestModelPolicyRequiresExplicitAllowedDefault(t *testing.T) {
	for _, policy := range []api.ModelPolicy{
		{}, {Allowed: []string{"first"}}, {Default: "m"},
		{Default: "m", Allowed: []string{"other"}},
		{Default: "m", Allowed: []string{"m", ""}},
	} {
		if _, err := policy.Resolve(""); !errors.Is(err, api.ErrInvalidModelPolicy) {
			t.Fatalf("Resolve under %#v = %v; want ErrInvalidModelPolicy", policy, err)
		}
	}
}

func TestModelPolicyDescriptorMustBeSubset(t *testing.T) {
	policy := api.ModelPolicy{Default: "primary", Allowed: []string{"primary", "alternate"}}
	for _, models := range [][]string{nil, {"alternate"}, {"alternate", "primary"}} {
		if err := policy.ValidateDescriptor(api.Descriptor{Models: models}); err != nil {
			t.Fatalf("valid subset %v: %v", models, err)
		}
	}
	for _, models := range [][]string{{"outside"}, {"primary", ""}} {
		if err := policy.ValidateDescriptor(api.Descriptor{Models: models}); !errors.Is(err, api.ErrModelNotAllowed) {
			t.Fatalf("invalid subset %v = %v; want ErrModelNotAllowed", models, err)
		}
	}
}
