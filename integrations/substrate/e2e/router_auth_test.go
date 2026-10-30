package e2e

import (
	"context"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"

	"github.com/agent-primitives/sessions/api"
	"github.com/agent-primitives/sessions/harness/echoagent"
	"github.com/agent-primitives/sessions/internal/harnessdial"
	"github.com/agent-primitives/sessions/runtime/substrate"
)

// TestRouterRefusesClientWithoutCertificate checks the router's half of the mTLS contract: the
// same TLS config as the suite's router client, minus the client certificate, is refused before
// any actor is named or woken. A positive control with the Job's SVID, to the same address and
// actor, gets past the handshake to routing, so the refusal is the missing certificate and not an
// unreachable router.
func TestRouterRefusesClientWithoutCertificate(t *testing.T) {
	requireCluster(t)
	inc := api.Incarnation{
		Address:      env("ATENET_ROUTER_ADDR", substrate.DefaultRouterAddress),
		CallMetadata: map[string]string{substrate.TargetActorHeader: "agentsessions-e2e/no-such-actor"},
	}
	describe := func(creds credentials.TransportCredentials) error {
		t.Helper()
		har, closeHarness, err := harnessdial.Dial(inc, creds)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = closeHarness() }()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, err = har.Describe(ctx)
		return err
	}

	cfg, err := routerCreds(t).TLSConfig()
	if err != nil {
		t.Fatal(err)
	}
	err = describe(credentials.NewTLS(cfg))
	if err == nil || status.Code(err) == codes.Unavailable {
		t.Fatalf("Describe with the SVID for an absent actor: %v, want a routing error past the handshake", err)
	}
	t.Logf("positive control: router admitted the SVID and refused the absent actor: %v", err)

	cfg.Certificates = nil
	err = describe(credentials.NewTLS(cfg))
	// v0.6.0's envoy sends a "certificate required" alert after the handshake; the client's first
	// write can race it and fail as a broken pipe instead. Either way the call never reaches routing.
	if status.Code(err) != codes.Unavailable || !(strings.Contains(err.Error(), "certificate required") || strings.Contains(err.Error(), "broken pipe")) {
		t.Fatalf("Describe without a client certificate: %v, want UNAVAILABLE from the router's refusal", err)
	}
	t.Logf("router refused a client without a certificate: %v", err)
}

// TestRoutedCallWakesSuspendedActor checks that the router, not Sessions, wakes a suspended actor:
// after the session is suspended, a routed Harness.Describe with the Job's SVID and the actor's
// ate-target-actor, and no Control ResumeActor from the client, reaches the harness and leaves the
// actor live.
func TestRoutedCallWakesSuspendedActor(t *testing.T) {
	f := newFixture(t, env("SUBSTRATE_ATESPACE", "e2e-suspend"))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	desc := api.Descriptor{ID: "echo", Capabilities: api.Capabilities{Resumability: api.ResumabilityStatelessReplay}}
	backend := f.backend(t, echoTemplateSpec, desc)
	session := uniqueUID("router-wake")
	defer stopQuietly(t, backend, session)
	log := journal(t).Session(session)
	p := newPlacer(t, backend, echoagent.Model)

	inc, err := p.Exec(ctx, log, session, []api.Message{*api.TextMessage("user", "before")}, 0)
	if err != nil {
		t.Fatalf("pre-suspend exec: %v", err)
	}
	if _, err := p.Suspend(ctx, log, session); err != nil {
		t.Fatalf("session suspend: %v", err)
	}
	if state, err := backend.Status(ctx, inc); err != nil || state != api.ComputeCold {
		t.Fatalf("actor state after suspend=%s (err %v), want %s", state, err, api.ComputeCold)
	}

	har, closeHarness, err := dialActor(t, inc)
	if err != nil {
		t.Fatalf("dial the suspended actor through the router: %v", err)
	}
	defer func() { _ = closeHarness() }()
	start := time.Now()
	got, err := har.Describe(ctx)
	if err != nil {
		t.Fatalf("routed Describe of the suspended actor: %v", err)
	}
	if got.ID != desc.ID {
		t.Fatalf("routed Describe returned descriptor %q, want %q", got.ID, desc.ID)
	}
	if state, err := backend.Status(ctx, inc); err != nil || state != api.ComputeLive {
		t.Fatalf("actor state after the routed call=%s (err %v), want %s", state, err, api.ComputeLive)
	}
	t.Logf("router woke the suspended actor and served Describe in %s", time.Since(start).Round(time.Millisecond))
}
