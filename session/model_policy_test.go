package session_test

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/agent-primitives/sessions/api"
	v1 "github.com/agent-primitives/sessions/api/genpb"
	"github.com/agent-primitives/sessions/controller"
	"github.com/agent-primitives/sessions/harness/echoagent"
	"github.com/agent-primitives/sessions/placement"
	"github.com/agent-primitives/sessions/runtime/local"
	"github.com/agent-primitives/sessions/session"
	"github.com/agent-primitives/sessions/sqlitelog"
)

func TestCreateSessionModelPolicy(t *testing.T) {
	for _, tc := range []struct {
		name, model string
		policy      bool
		want        codes.Code
	}{
		{"explicit", "echo", true, codes.OK},
		{"empty_keeps_metadata_empty", "", true, codes.OK},
		{"invalid", "forbidden", true, codes.InvalidArgument},
		{"no_policy_is_informational", "forbidden", false, codes.OK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := openStore(t, ":memory:")
			b := local.New(echoagent.Harness{})
			t.Cleanup(func() { _ = b.Close() })
			var opts []placement.Option
			if tc.policy {
				opts = append(opts, placement.WithModelPolicy(api.ModelPolicy{Default: "echo", Allowed: []string{"echo"}}))
			}
			client := serveRegistry(t, store, echoRegistry(t, b, opts...))
			sess, err := client.CreateSession(t.Context(), &v1.CreateSessionRequest{Session: &v1.Session{Model: tc.model}})
			if status.Code(err) != tc.want {
				t.Fatalf("create model %q: %v, want %v", tc.model, err, tc.want)
			}
			page, err := store.ListSessions(sqlitelog.ListOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if tc.want != codes.OK {
				if len(page.Sessions) != 0 {
					t.Fatal("refused create stored a session")
				}
				return
			}
			if len(page.Sessions) != 1 || page.Sessions[0].ModelPolicy != tc.policy || sess.GetModel() != tc.model || page.Sessions[0].Model != tc.model {
				t.Fatalf("create changed requested metadata: %v, %+v", sess, page)
			}
		})
	}
}

// The service's real Connect path must preserve the opt-in discriminator, including across
// empty-session reopen and fork. A current policy alone must never opt an old session in.
func TestSessionModelPolicyLifecycle(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		name := "legacy"
		if enabled {
			name = "opted_in"
		}
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "journal.db")
			store := openStore(t, path)
			registry := sessionPolicyRegistry(t, enabled)
			client := serveRegistry(t, store, registry)
			parent, err := client.CreateSession(t.Context(), &v1.CreateSessionRequest{})
			if err != nil {
				t.Fatal(err)
			}
			uid := parent.GetMetadata().GetUid()
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			store = openStore(t, path)
			// Policy activation after a zero-event legacy session cannot change its semantics.
			client = serveRegistry(t, store, sessionPolicyRegistry(t, true))
			forked, err := client.Fork(t.Context(), &v1.ForkRequest{Session: uid, Count: 1})
			if err != nil {
				t.Fatal(err)
			}
			child := forked.GetChildren()[0].GetMetadata().GetUid()
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			store = openStore(t, path)
			client = serveRegistry(t, store, sessionPolicyRegistry(t, true))
			want := "legacy"
			if enabled {
				want = "route-default"
			}
			for _, id := range []string{uid, child} {
				info, err := store.SessionInfo(id)
				if err != nil || info.ModelPolicy != enabled {
					t.Fatalf("stored eligibility %s=%t, %v", id, info.ModelPolicy, err)
				}
				got := execOutputs(t, client, id, "input", info.LastSeq)
				if len(got) != 1 || got[0] != want {
					t.Fatalf("%s output=%v, want %s", id, got, want)
				}
				records, err := store.Session(id).Read(1)
				if err != nil {
					t.Fatal(err)
				}
				for _, rec := range records {
					if rec.Event.ExecutionStart != nil {
						model := ""
						if enabled {
							model = "route-default"
						}
						if rec.Event.ExecutionStart.Model != model {
							t.Fatalf("recorded model=%q, want %q", rec.Event.ExecutionStart.Model, model)
						}
					}
				}
				if err := store.Session(id).Verify(); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

type sessionPolicyHarness struct{ echoagent.Harness }

func (h sessionPolicyHarness) Describe(ctx context.Context) (api.Descriptor, error) {
	d, err := h.Harness.Describe(ctx)
	d.Models = nil
	return d, err
}

func (sessionPolicyHarness) Run(ctx context.Context, start *api.Start, sink api.EventSink) error {
	model := start.Model
	if model == "" {
		model = "legacy"
	}
	_, err := sink.Model(ctx, api.ModelRequest{Model: model})
	return err
}

func sessionPolicyRegistry(t *testing.T, enabled bool) *placement.Registry {
	t.Helper()
	b := local.New(sessionPolicyHarness{})
	t.Cleanup(func() { _ = b.Close() })
	var opts []placement.Option
	if enabled {
		opts = append(opts, placement.WithModelPolicy(api.ModelPolicy{Default: "route-default", Allowed: []string{"route-default", "chosen"}}))
	}
	p := placement.New(b, func(_ context.Context, req api.ModelRequest) (api.ModelResponse, error) {
		return api.ModelResponse{Message: *api.TextMessage("assistant", req.Model)}, nil
	}, opts...)
	r, err := placement.NewRegistry("echo", map[string]*placement.Placer{"echo": p})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// A frozen pre-policy journal must stay legacy even when this host now serves a policy route.
func TestSchemaV4FixtureUnderModelPolicy(t *testing.T) {
	path := copyJournal(t, schemaV4Fixture)
	if got := userVersionOf(t, path); got != 4 {
		t.Fatalf("fixture schema=%d, want 4", got)
	}
	uid := sessionUIDOf(t, path, "parent")
	store := openStore(t, path)
	b := local.New(echoagent.Harness{})
	t.Cleanup(func() { _ = b.Close() })
	policy := api.ModelPolicy{Default: "today", Allowed: []string{"today", "echo"}}
	client := serveRegistry(t, store, echoRegistry(t, b, placement.WithModelPolicy(policy)))
	info, err := store.SessionInfo(uid)
	if err != nil || info.ModelPolicy || info.Model != "echo-model" {
		t.Fatalf("legacy metadata=%+v, %v", info, err)
	}
	before, err := store.Session(uid).Read(1)
	if err != nil {
		t.Fatal(err)
	}
	var providerCalls int
	c, err := controller.New(store.Session(uid), func(context.Context, api.ModelRequest) (api.ModelResponse, error) {
		providerCalls++
		return api.ModelResponse{}, errors.New("replay called provider")
	}, controller.WithSessionUID(uid), controller.WithHarness("echo"), controller.WithModelPolicy(policy))
	if err != nil {
		t.Fatal(err)
	}
	outputs, err := c.Replay(t.Context(), echoagent.Harness{})
	if err != nil || providerCalls != 0 || !reflect.DeepEqual(outputs, []string{"echo:hello", "echo:again"}) {
		t.Fatalf("legacy replay=%v, calls=%d, %v", outputs, providerCalls, err)
	}
	forked, err := client.Fork(t.Context(), &v1.ForkRequest{Session: uid, Count: 1})
	if err != nil {
		t.Fatal(err)
	}
	child := forked.GetChildren()[0].GetMetadata().GetUid()
	for _, id := range []string{uid, child} {
		info, err := store.SessionInfo(id)
		if err != nil || info.ModelPolicy {
			t.Fatalf("legacy fork eligibility=%+v, %v", info, err)
		}
		got := execOutputs(t, client, id, "after upgrade", info.LastSeq)
		if !reflect.DeepEqual(got, []string{"echo:after upgrade"}) {
			t.Fatalf("legacy continuation=%v", got)
		}
		after, err := store.Session(id).Read(1)
		if err != nil || len(after) < len(before) {
			t.Fatalf("old prefix missing: %v", err)
		}
		// Fork re-appends under the child's fence; committed payloads and hashes stay exact.
		for i, old := range before {
			got := after[i]
			if got.Seq != old.Seq || got.PrevHash != old.PrevHash || got.Hash != old.Hash || !reflect.DeepEqual(got.Event, old.Event) {
				t.Fatalf("old prefix changed at seq %d", old.Seq)
			}
		}
		for _, rec := range after {
			if rec.Event.ExecutionStart != nil && rec.Event.ExecutionStart.Model != "" {
				t.Fatal("legacy invocation acquired today's default")
			}
		}
		if err := store.Session(id).Verify(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestExecSessionModelSelectedRoute(t *testing.T) {
	for _, tc := range []struct {
		name, model, route, want string
		refuse                   bool
	}{
		{"explicit", "chosen", "", "chosen", false},
		{"override_default", "", "other", "other-default", false},
		{"override_explicit", "chosen", "other", "chosen", false},
		{"override_refused", "only-a", "other", "", true},
		{"unconfigured_override", "only-a", "legacy", "legacy", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := openStore(t, ":memory:")
			var entries = make(map[string]*placement.Placer)
			var backends = make(map[string]*publicGateBackend)
			var calls atomic.Int32
			for _, name := range []string{"echo", "other", "legacy"} {
				b := &publicGateBackend{Backend: local.New(sessionPolicyHarness{})}
				t.Cleanup(func() { _ = b.Close() })
				backends[name] = b
				var opts []placement.Option
				if name != "legacy" {
					def := "a-default"
					allowed := []string{"a-default", "chosen", "only-a"}
					if name == "other" {
						def, allowed = "other-default", []string{"other-default", "chosen"}
					}
					opts = append(opts, placement.WithModelPolicy(api.ModelPolicy{Default: def, Allowed: allowed}))
				}
				entries[name] = placement.New(b, func(_ context.Context, req api.ModelRequest) (api.ModelResponse, error) {
					calls.Add(1)
					return api.ModelResponse{Message: *api.TextMessage("assistant", req.Model)}, nil
				}, opts...)
			}
			registry, err := placement.NewRegistry("echo", entries)
			if err != nil {
				t.Fatal(err)
			}
			client := serveRegistry(t, store, registry)
			sess, err := client.CreateSession(t.Context(), &v1.CreateSessionRequest{Session: &v1.Session{Model: tc.model}})
			if err != nil {
				t.Fatal(err)
			}
			uid := sess.GetMetadata().GetUid()
			stream, err := client.Exec(t.Context(), &v1.ExecRequest{Session: uid, Harness: tc.route})
			if err == nil {
				err = drainExec(stream)
			}
			if tc.refuse {
				if status.Code(err) != codes.InvalidArgument {
					t.Fatalf("invalid selected model=%v", err)
				}
				head, headErr := store.Session(uid).Head()
				if headErr != nil || head != 0 || calls.Load() != 0 || backends["other"].creates.Load() != 0 {
					t.Fatalf("refused turn had effects: head=%d, calls=%d, creates=%d, %v", head, calls.Load(), backends["other"].creates.Load(), headErr)
				}
				return
			}
			if err != nil && err != io.EOF {
				t.Fatal(err)
			}
			records, err := store.Session(uid).Read(1)
			if err != nil {
				t.Fatal(err)
			}
			var got string
			for _, rec := range records {
				if rec.Event.ModelCall != nil {
					got = rec.Event.ModelCall.Model
				}
			}
			if got != tc.want || calls.Load() != 1 {
				t.Fatalf("selected model=%q calls=%d, want %q once", got, calls.Load(), tc.want)
			}
		})
	}
}

func TestCreateSessionModelPolicyPreservesCause(t *testing.T) {
	store := openStore(t, ":memory:")
	svc := session.NewService(store, sessionPolicyRegistry(t, true))
	_, err := svc.CreateSession(t.Context(), &v1.CreateSessionRequest{Session: &v1.Session{Model: "forbidden"}})
	if status.Code(err) != codes.InvalidArgument || !errors.Is(err, api.ErrModelNotAllowed) {
		t.Fatalf("create cause lost: %v", err)
	}
}

type policyDescriptorHarness struct {
	echoagent.Harness
	models      []string
	describeErr error
	describes   atomic.Int32
}

func (h *policyDescriptorHarness) Describe(ctx context.Context) (api.Descriptor, error) {
	h.describes.Add(1)
	if h.describeErr != nil {
		return api.Descriptor{}, h.describeErr
	}
	d, err := h.Harness.Describe(ctx)
	d.Models = h.models
	return d, err
}

func TestStoredHarnessModelPolicyFailureLeavesOnlyRouteUnavailable(t *testing.T) {
	harness := &policyDescriptorHarness{models: []string{"allowed"}}
	factory := func(_ string, spec *v1.HarnessSpec) (*placement.Placer, func(), error) {
		b := local.New(harness)
		return placement.New(b, echoagent.Model, placement.WithDescriptorID(spec.GetDescriptorId()),
			placement.WithModelPolicy(api.ModelPolicy{Default: "allowed", Allowed: []string{"allowed"}})), func() { _ = b.Close() }, nil
	}
	path := filepath.Join(t.TempDir(), "journal.db")
	first := startRegistryHostWith(t, path, factory)
	registerHarness(t, first, "policy", remoteSpec("127.0.0.1:9000"))
	first.stop()
	harness.models = []string{"forbidden"}
	second := startRegistryHostWith(t, path, factory)
	if _, err := createOn(t.Context(), second.sessions, "policy"); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("failed policy load did not leave route unavailable: %v", err)
	}
	if _, err := createOn(t.Context(), second.sessions, "echo"); err != nil {
		t.Fatalf("policy load failure broke static route: %v", err)
	}
	if _, err := second.store.Harness("policy"); err != nil {
		t.Fatalf("load removed immutable row: %v", err)
	}
	if got := harness.describes.Load(); got != 2 {
		t.Fatalf("registration/load Describe calls=%d, want once each", got)
	}
}

func TestRegisterHarnessModelPolicyChecksBeforeStore(t *testing.T) {
	const private = "unix /private/harness.sock?token=private-payload"
	for _, tc := range []struct {
		name        string
		models      []string
		describeErr error
		want        codes.Code
	}{
		{"allowed", []string{"allowed"}, nil, codes.OK},
		{"forbidden", []string{"forbidden"}, nil, codes.FailedPrecondition},
		{"describe_failed", nil, errors.New(private), codes.Unavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			harness := &policyDescriptorHarness{models: tc.models, describeErr: tc.describeErr}
			factory := func(_ string, spec *v1.HarnessSpec) (*placement.Placer, func(), error) {
				b := local.New(harness)
				return placement.New(b, echoagent.Model, placement.WithDescriptorID(spec.GetDescriptorId()),
					placement.WithModelPolicy(api.ModelPolicy{Default: "allowed", Allowed: []string{"allowed"}})), func() { _ = b.Close() }, nil
			}
			h := startRegistryHostWith(t, filepath.Join(t.TempDir(), "journal.db"), factory)
			_, err := h.reg.RegisterHarness(t.Context(), &v1.RegisterHarnessRequest{Name: "policy", Spec: remoteSpec("127.0.0.1:9000")})
			if status.Code(err) != tc.want {
				t.Fatalf("registration status = %v; want %s", err, tc.want)
			}
			if err != nil && strings.Contains(err.Error(), private) {
				t.Fatalf("registration leaked peer error: %v", err)
			}
			if tc.want != codes.OK {
				if _, err := h.store.Harness("policy"); !errors.Is(err, sqlitelog.ErrHarnessNotFound) {
					t.Fatalf("refused registration stored immutable row: %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if got := harness.describes.Load(); got != 1 {
				t.Fatalf("registration Describe calls=%d, want one cached check", got)
			}
		})
	}
}
