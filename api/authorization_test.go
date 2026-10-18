package api_test

import (
	"context"
	"errors"
	"testing"

	"github.com/agent-primitives/sessions/api"
)

// This policy must not turn owner access or a future action into approval authority.
func TestOwnerAllowlistAuthorizer(t *testing.T) {
	owner := api.CallerIdentity{Issuer: "issuer", Subject: "owner"}
	other := api.CallerIdentity{Issuer: "issuer", Subject: "other"}
	policy := api.OwnerAllowlistAuthorizer{Allow: map[api.Action][]api.CallerIdentity{
		api.ActionCreate: {owner}, api.ActionList: {owner}, api.ActionApprove: {owner},
		api.ActionRegistryRead: {owner}, api.ActionRegistryList: {owner},
		api.ActionRegistryRegister: {owner}, api.ActionRegistryRetire: {owner},
		api.ActionMint: {owner}, "future": {owner}, api.ActionRead: {other},
	}}
	for _, tc := range []struct {
		name   string
		caller api.CallerIdentity
		action api.Action
		uid    string
		allow  bool
	}{
		{"owner read", owner, api.ActionRead, "s", true},
		{"allowlist cannot bypass ownership", other, api.ActionRead, "s", false},
		{"explicit approve", owner, api.ActionApprove, "s", true},
		{"approve denied", other, api.ActionApprove, "s", false},
		{"issuer exact", api.CallerIdentity{Issuer: "issuer/", Subject: "owner"}, api.ActionCreate, "", false},
		{"incomplete", api.CallerIdentity{Subject: "owner"}, api.ActionCreate, "", false},
		{"create", owner, api.ActionCreate, "", true},
		{"list", owner, api.ActionList, "", true},
		{"registry read", owner, api.ActionRegistryRead, "", true},
		{"registry list", owner, api.ActionRegistryList, "", true},
		{"registry register", owner, api.ActionRegistryRegister, "", true},
		{"registry retire", owner, api.ActionRegistryRetire, "", true},
		{"mint never", owner, api.ActionMint, "s", false},
		{"unknown never", owner, "future", "s", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := policy.Authorize(context.Background(), api.AuthorizationRequest{Caller: tc.caller, Owner: owner, SessionUID: tc.uid, Action: tc.action})
			if tc.allow && err != nil || !tc.allow && !errors.Is(err, api.ErrPermissionDenied) {
				t.Fatalf("Authorize = %v, allow %v", err, tc.allow)
			}
		})
	}
	if err := (api.OwnerAllowlistAuthorizer{}).Authorize(context.Background(), api.AuthorizationRequest{Caller: owner, Owner: owner, SessionUID: "s", Action: api.ActionApprove}); !errors.Is(err, api.ErrPermissionDenied) {
		t.Fatalf("owner alone approved: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := policy.Authorize(ctx, api.AuthorizationRequest{Caller: owner, Action: api.ActionCreate}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled: %v", err)
	}
}

func TestOwnerAuthorizer(t *testing.T) {
	owner := api.CallerIdentity{Issuer: "https://issuer", Subject: "caller"}
	var authorizer api.Authorizer = api.OwnerAuthorizer{}
	for _, action := range []api.Action{api.ActionRead, api.ActionExec, api.ActionFork, api.ActionSuspend, api.ActionResume, api.ActionDelete, api.ActionCancel} {
		t.Run(string(action), func(t *testing.T) {
			if err := authorizer.Authorize(context.Background(), api.AuthorizationRequest{Caller: owner, Owner: owner, SessionUID: "session", Action: action}); err != nil {
				t.Fatal(err)
			}
		})
	}
	for _, tc := range []struct {
		name          string
		caller, owner api.CallerIdentity
		uid           string
		action        api.Action
	}{
		{"different issuer", api.CallerIdentity{Issuer: "https://other", Subject: "caller"}, owner, "s", api.ActionRead},
		{"different subject", api.CallerIdentity{Issuer: owner.Issuer, Subject: "other"}, owner, "s", api.ActionExec},
		{"no issuer normalization", api.CallerIdentity{Issuer: owner.Issuer + "/", Subject: owner.Subject}, owner, "s", api.ActionRead},
		{"no subject normalization", api.CallerIdentity{Issuer: owner.Issuer, Subject: "Caller"}, owner, "s", api.ActionRead},
		{"anonymous", api.CallerIdentity{}, owner, "s", api.ActionRead},
		{"missing issuer", api.CallerIdentity{Subject: "caller"}, owner, "s", api.ActionRead},
		{"missing subject", api.CallerIdentity{Issuer: owner.Issuer}, owner, "s", api.ActionRead},
		{"unbound", owner, api.CallerIdentity{}, "s", api.ActionRead},
		{"partial owner", owner, api.CallerIdentity{Subject: "caller"}, "s", api.ActionRead},
		{"missing session", owner, owner, "", api.ActionRead},
		{"approval separate", owner, owner, "s", api.ActionApprove},
		{"audience separate", owner, owner, "s", api.ActionMint},
		{"create not claim", owner, owner, "s", api.ActionCreate},
		{"empty action", owner, owner, "s", ""},
		{"future action", owner, owner, "s", "future"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := authorizer.Authorize(context.Background(), api.AuthorizationRequest{Caller: tc.caller, Owner: tc.owner, SessionUID: tc.uid, Action: tc.action})
			if !errors.Is(err, api.ErrPermissionDenied) {
				t.Fatalf("Authorize = %v, want ErrPermissionDenied", err)
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := authorizer.Authorize(ctx, api.AuthorizationRequest{Caller: owner, Owner: owner, SessionUID: "s", Action: api.ActionRead}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled Authorize = %v", err)
	}
}
