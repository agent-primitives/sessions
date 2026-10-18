package api_test

import (
	"context"
	"errors"
	"testing"

	"github.com/agent-primitives/sessions/api"
)

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
