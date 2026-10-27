package session_test

import (
	"context"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"

	v1 "github.com/agent-primitives/sessions/api/genpb"
	"github.com/agent-primitives/sessions/harness/echoagent"
	"github.com/agent-primitives/sessions/placement"
	"github.com/agent-primitives/sessions/runtime/local"
	"github.com/agent-primitives/sessions/session"
	"github.com/agent-primitives/sessions/sqlitelog"
)

// countingFactory is echoFactory that counts how often the host built a Placer.
func countingFactory(calls *atomic.Int32) func(string, *v1.HarnessSpec) (*placement.Placer, func(), error) {
	return func(name string, spec *v1.HarnessSpec) (*placement.Placer, func(), error) {
		calls.Add(1)
		return echoFactory(name, spec)
	}
}

// A create-only registration on a free name is an ordinary ACTIVE registration that new sessions
// can select at once.
func TestCreateHarnessCreatesUsableRegistration(t *testing.T) {
	ctx := context.Background()
	h := startRegistryHost(t, filepath.Join(t.TempDir(), "journal.db"))
	got, err := h.harnesses.CreateHarness(ctx, &v1.CreateHarnessRequest{Name: "fresh", Spec: remoteSpec("127.0.0.1:9000")})
	if err != nil {
		t.Fatal(err)
	}
	if got.GetState() != v1.HarnessState_HARNESS_STATE_ACTIVE || got.GetSource() != v1.HarnessSource_HARNESS_SOURCE_REGISTERED ||
		got.GetMetadata().GetName() != "fresh" || !strings.HasPrefix(got.GetMetadata().GetUid(), "harness-") ||
		!strings.HasPrefix(got.GetSpecDigest(), "sha256:") || got.GetUnservableReason() != "" || !proto.Equal(got.GetSpec(), remoteSpec("127.0.0.1:9000")) {
		t.Fatalf("created registration = %v", got)
	}
	stored, err := h.harnesses.GetHarness(ctx, &v1.GetHarnessRequest{Name: "fresh"})
	if err != nil || !proto.Equal(stored, got) {
		t.Fatalf("GetHarness = %v, %v; want %v", stored, err, got)
	}
	sess, err := createOn(ctx, h.sessions, "fresh")
	if err != nil {
		t.Fatalf("new session on a created harness: %v", err)
	}
	if err := execOn(ctx, h.sessions, sess, ""); err != nil {
		t.Fatalf("turn on a created harness: %v", err)
	}
}

// Every name that is already taken refuses a create-only registration with ALREADY_EXISTS, whatever
// the spec: an ACTIVE or RETIRED row, a row another host added that this one has not loaded, a
// static name, or a name reserved here or by another host sharing the journal. The refusal changes
// nothing: no row is written or reactivated, nothing is loaded, and no Placer is built for any of
// them.
func TestCreateHarnessRefusesTakenNames(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "journal.db")
	var built atomic.Int32
	h := startRegistryHostWith(t, path, countingFactory(&built))
	spec, other := remoteSpec("127.0.0.1:9000"), remoteSpec("127.0.0.1:9001")
	registerHarness(t, h, "active", spec)
	registerHarness(t, h, "retired", spec)
	if _, err := h.harnesses.RetireHarness(ctx, &v1.RetireHarnessRequest{Name: "retired", Reason: "superseded"}); err != nil {
		t.Fatal(err)
	}
	// Another host sharing the journal registers a name this host never loads, and reserves its
	// own static name.
	late := startRegistryHost(t, path)
	registerHarness(t, late, "late", spec)
	if err := late.store.ReserveHarnessNames([]string{"solo"}); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name string
		spec *v1.HarnessSpec
		row  bool // the journal holds a registration row under the name
	}{
		{"active", spec, true},
		{"active", other, true},
		{"retired", spec, true},
		{"retired", other, true},
		{"late", spec, true},
		{"echo", spec, false}, // static on this host
		{"chat", spec, false}, // reserved on this host
		{"solo", spec, false}, // reserved by another host in the journal
	} {
		t.Run(fmt.Sprintf("%s/%s", tc.name, tc.spec.GetRemote().GetAddress()), func(t *testing.T) {
			before, beforeErr := h.harnesses.GetHarness(ctx, &v1.GetHarnessRequest{Name: tc.name})
			builtBefore := built.Load()
			_, err := h.harnesses.CreateHarness(ctx, &v1.CreateHarnessRequest{Name: tc.name, Spec: tc.spec})
			if status.Code(err) != codes.AlreadyExists {
				t.Fatalf("CreateHarness(%q) = %v, want AlreadyExists", tc.name, err)
			}
			after, afterErr := h.harnesses.GetHarness(ctx, &v1.GetHarnessRequest{Name: tc.name})
			if !proto.Equal(before, after) || status.Code(beforeErr) != status.Code(afterErr) {
				t.Fatalf("refused CreateHarness(%q) changed the registration:\n%v (%v)\n%v (%v)", tc.name, before, beforeErr, after, afterErr)
			}
			if !tc.row {
				if _, err := h.store.Harness(tc.name); !errors.Is(err, sqlitelog.ErrHarnessNotFound) {
					t.Fatalf("refused CreateHarness(%q) stored a row: %v", tc.name, err)
				}
			}
			if n := built.Load() - builtBefore; n != 0 {
				t.Fatalf("CreateHarness(%q) built %d Placers for a taken name", tc.name, n)
			}
		})
	}

	// The retired harness was not reactivated: new sessions are still refused.
	if _, err := createOn(ctx, h.sessions, "retired"); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("new session on retired harness after CreateHarness: %v, want FailedPrecondition", err)
	}
	got, err := h.harnesses.GetHarness(ctx, &v1.GetHarnessRequest{Name: "retired"})
	if err != nil || got.GetState() != v1.HarnessState_HARNESS_STATE_RETIRED || got.GetRetireReason() != "superseded" {
		t.Fatalf("retired harness after CreateHarness = %v, %v", got, err)
	}
	// The row another host added is still not loaded here; only RegisterHarness loads it.
	got, err = h.harnesses.GetHarness(ctx, &v1.GetHarnessRequest{Name: "late"})
	if err != nil || !strings.Contains(got.GetUnservableReason(), "not loaded") {
		t.Fatalf("another host's row after CreateHarness = %v, %v; want still not loaded here", got, err)
	}
}

// CreateHarness validates like RegisterHarness, and a placement this host cannot build stores
// nothing.
func TestCreateHarnessRejectsBadInput(t *testing.T) {
	ctx := context.Background()
	h := startRegistryHost(t, filepath.Join(t.TempDir(), "journal.db"))
	substrate := substrateSpec()
	for _, tc := range []struct {
		name string
		spec *v1.HarnessSpec
		want codes.Code
	}{
		{"Bad_Name", remoteSpec("127.0.0.1:9000"), codes.InvalidArgument},
		{"nospec", nil, codes.InvalidArgument},
		{"noaddr", remoteSpec(""), codes.InvalidArgument},
		{"sub", substrate, codes.FailedPrecondition},
	} {
		if _, err := h.harnesses.CreateHarness(ctx, &v1.CreateHarnessRequest{Name: tc.name, Spec: tc.spec}); status.Code(err) != tc.want {
			t.Errorf("CreateHarness(%q) = %v, want %v", tc.name, err, tc.want)
		}
		if _, err := h.store.Harness(tc.name); !errors.Is(err, sqlitelog.ErrHarnessNotFound) {
			t.Errorf("refused CreateHarness(%q) stored a row: %v", tc.name, err)
		}
	}

	// The spec is validated before registration rows are read (REG-3a), so an invalid spec under a
	// registered name is INVALID_ARGUMENT, while a static name is refused before the spec (REG-1).
	registerHarness(t, h, "taken", remoteSpec("127.0.0.1:9000"))
	for _, tc := range []struct {
		name string
		want codes.Code
	}{
		{"taken", codes.InvalidArgument},
		{"echo", codes.AlreadyExists},
	} {
		for _, spec := range []*v1.HarnessSpec{nil, remoteSpec("")} {
			if _, err := h.harnesses.CreateHarness(ctx, &v1.CreateHarnessRequest{Name: tc.name, Spec: spec}); status.Code(err) != tc.want {
				t.Errorf("CreateHarness(%q, %v) = %v, want %v", tc.name, spec, err, tc.want)
			}
		}
	}
}

// A name that only another host sharing the journal holds, as its static harness or a reserved
// name, is taken for CreateHarness even when this host cannot build the requested placement:
// REG-3a makes a reserved name ALREADY_EXISTS for any request that passes REG-1 and REG-2, so the
// reservation is checked before REG-2a's placement admission, and no Placer is built for it.
func TestCreateHarnessRefusesRemotelyReservedNameBeforePlacement(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "journal.db")
	var built atomic.Int32
	h := startRegistryHostWith(t, path, countingFactory(&built))

	// Host B opens its own store handle on the journal and reserves its static and reserved names
	// at startup; it never registers them, and host A's registry holds neither.
	storeB, err := sqlitelog.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { storeB.Close() })
	backendB := local.New(echoagent.Harness{})
	t.Cleanup(func() { _ = backendB.Close() })
	registryB, err := placement.NewRegistry("b-static", map[string]*placement.Placer{
		"b-static": placement.New(backendB, echoagent.Model),
	}, placement.WithReservedNames("b-reserved"))
	if err != nil {
		t.Fatal(err)
	}
	if err := session.ReserveStaticHarnessNames(storeB, registryB); err != nil {
		t.Fatal(err)
	}

	substrate := substrateSpec()
	for _, name := range []string{"b-static", "b-reserved"} {
		for _, spec := range []*v1.HarnessSpec{substrate, remoteSpec("127.0.0.1:9000")} {
			_, err := h.harnesses.CreateHarness(ctx, &v1.CreateHarnessRequest{Name: name, Spec: spec})
			if status.Code(err) != codes.AlreadyExists {
				t.Errorf("CreateHarness(%q, %s placement) = %v, want AlreadyExists", name, placementKind(spec), err)
			}
			if _, err := h.store.Harness(name); !errors.Is(err, sqlitelog.ErrHarnessNotFound) {
				t.Errorf("refused CreateHarness(%q) stored a row: %v", name, err)
			}
		}
	}
	if n := built.Load(); n != 0 {
		t.Errorf("CreateHarness built %d Placers for names another host reserved", n)
	}

	// Plain RegisterHarness keeps refusing them in the store's transaction.
	for _, name := range []string{"b-static", "b-reserved"} {
		if _, err := h.harnesses.RegisterHarness(ctx, &v1.RegisterHarnessRequest{Name: name, Spec: remoteSpec("127.0.0.1:9000")}); status.Code(err) != codes.AlreadyExists {
			t.Errorf("RegisterHarness(%q) = %v, want AlreadyExists", name, err)
		}
	}
	// A free name with the placement this host cannot build is still FAILED_PRECONDITION (REG-2a).
	if _, err := h.harnesses.CreateHarness(ctx, &v1.CreateHarnessRequest{Name: "free", Spec: substrate}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("CreateHarness(free, substrate placement) = %v, want FailedPrecondition", err)
	}

	// The preflight is not what holds: host B reserves a name after host C's preflight read it as
	// free, while C builds the Placer, and the store's transaction still refuses the insert.
	racer := startRegistryHostWith(t, path, func(name string, spec *v1.HarnessSpec) (*placement.Placer, func(), error) {
		if err := storeB.ReserveHarnessNames([]string{name}); err != nil {
			return nil, nil, err
		}
		return echoFactory(name, spec)
	})
	if _, err := racer.harnesses.CreateHarness(ctx, &v1.CreateHarnessRequest{Name: "raced", Spec: remoteSpec("127.0.0.1:9000")}); status.Code(err) != codes.AlreadyExists {
		t.Errorf("CreateHarness(raced) = %v, want AlreadyExists", err)
	}
	if _, err := racer.store.Harness("raced"); !errors.Is(err, sqlitelog.ErrHarnessNotFound) {
		t.Errorf("CreateHarness(raced) stored a row: %v", err)
	}
	if _, err := createOn(ctx, racer.sessions, "raced"); err == nil {
		t.Error("CreateHarness(raced) left a resolvable harness")
	}
}

// substrateSpec is a valid spec whose placement echoFactory, standing in for a host without a
// substrate backend, cannot build.
func substrateSpec() *v1.HarnessSpec {
	return &v1.HarnessSpec{
		Placement:    &v1.HarnessSpec_Substrate{Substrate: &v1.SubstratePlacement{Atespace: "a", Template: "t"}},
		Capabilities: remoteSpec("x").GetCapabilities(),
	}
}

func placementKind(spec *v1.HarnessSpec) string {
	if spec.GetSubstrate() != nil {
		return "substrate"
	}
	return "remote"
}

// Many callers on two hosts sharing one journal race CreateHarness for the same name. Each host
// serializes its own registrations, but nothing in memory is shared between them, so the store's
// transaction alone has to make exactly one of them win.
func TestCreateHarnessAtMostOneWinnerAcrossHosts(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "journal.db")
	hosts := []*registryHost{startRegistryHost(t, path), startRegistryHost(t, path)}
	for round := range 3 {
		name := fmt.Sprintf("race-%d", round)
		const callers = 16
		var wg sync.WaitGroup
		start := make(chan struct{})
		type result struct {
			host int
			reg  *v1.HarnessRegistration
			err  error
		}
		results := make(chan result, callers)
		for i := range callers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				// One spec for every caller: for plain RegisterHarness the losers would be UNCHANGED,
				// not refused, so only the create-only check in the store's transaction refuses them.
				reg, err := hosts[i%len(hosts)].harnesses.CreateHarness(ctx, &v1.CreateHarnessRequest{Name: name, Spec: remoteSpec("127.0.0.1:9000")})
				results <- result{host: i % len(hosts), reg: reg, err: err}
			}()
		}
		close(start)
		wg.Wait()
		close(results)

		var winners []result
		for r := range results {
			switch {
			case r.err == nil:
				winners = append(winners, r)
			case status.Code(r.err) != codes.AlreadyExists:
				t.Errorf("%s: CreateHarness on host %d = %v, want success or AlreadyExists", name, r.host, r.err)
			}
		}
		if len(winners) != 1 {
			t.Fatalf("%s: %d CreateHarness calls succeeded, want exactly one", name, len(winners))
		}
		win := winners[0]
		for i, h := range hosts {
			got, err := h.harnesses.GetHarness(ctx, &v1.GetHarnessRequest{Name: name})
			if err != nil || got.GetMetadata().GetUid() != win.reg.GetMetadata().GetUid() || got.GetSpecDigest() != win.reg.GetSpecDigest() {
				t.Fatalf("%s: host %d reads %v, %v; winner stored %v", name, i, got, err, win.reg)
			}
		}
		if _, err := createOn(ctx, hosts[win.host].sessions, name); err != nil {
			t.Fatalf("%s: new session on the winning host: %v", name, err)
		}
	}
}

// Plain RegisterHarness keeps its outcomes, including on a row CreateHarness made: CREATED for a
// free name, UNCHANGED for the same spec, ALREADY_EXISTS for another spec, and REACTIVATED for the
// same spec once retired, keeping the uid throughout.
func TestRegisterHarnessUnchangedByCreateOnly(t *testing.T) {
	ctx := context.Background()
	h := startRegistryHost(t, filepath.Join(t.TempDir(), "journal.db"))
	spec, other := remoteSpec("127.0.0.1:9000"), remoteSpec("127.0.0.1:9001")
	created, err := h.harnesses.CreateHarness(ctx, &v1.CreateHarnessRequest{Name: "via-create", Spec: spec})
	if err != nil {
		t.Fatal(err)
	}
	first := registerHarness(t, h, "via-register", spec)
	if first.GetOutcome() != v1.RegisterOutcome_REGISTER_OUTCOME_CREATED {
		t.Fatalf("register a free name: %v, want CREATED", first.GetOutcome())
	}
	for name, uid := range map[string]string{
		"via-create":   created.GetMetadata().GetUid(),
		"via-register": first.GetHarness().GetMetadata().GetUid(),
	} {
		check := func(step string, resp *v1.RegisterHarnessResponse, want v1.RegisterOutcome, wantState v1.HarnessState) {
			t.Helper()
			if resp.GetOutcome() != want || resp.GetHarness().GetMetadata().GetUid() != uid || resp.GetHarness().GetState() != wantState {
				t.Fatalf("%s %s: %v %v (uid %s), want %v %v (uid %s)", name, step, resp.GetOutcome(), resp.GetHarness().GetState(),
					resp.GetHarness().GetMetadata().GetUid(), want, wantState, uid)
			}
		}
		check("repeat", registerHarness(t, h, name, spec), v1.RegisterOutcome_REGISTER_OUTCOME_UNCHANGED, v1.HarnessState_HARNESS_STATE_ACTIVE)
		if _, err := h.harnesses.RegisterHarness(ctx, &v1.RegisterHarnessRequest{Name: name, Spec: other}); status.Code(err) != codes.AlreadyExists {
			t.Fatalf("%s different spec: %v, want AlreadyExists", name, err)
		}
		if _, err := h.harnesses.RetireHarness(ctx, &v1.RetireHarnessRequest{Name: name}); err != nil {
			t.Fatal(err)
		}
		check("after retire", registerHarness(t, h, name, spec), v1.RegisterOutcome_REGISTER_OUTCOME_REACTIVATED, v1.HarnessState_HARNESS_STATE_ACTIVE)
		if _, err := createOn(ctx, h.sessions, name); err != nil {
			t.Fatalf("%s: new session after reactivation: %v", name, err)
		}
	}
	for _, name := range []string{"echo", "chat"} {
		if _, err := h.harnesses.RegisterHarness(ctx, &v1.RegisterHarnessRequest{Name: name, Spec: spec}); status.Code(err) != codes.AlreadyExists {
			t.Errorf("register static/reserved %q: %v, want AlreadyExists", name, err)
		}
	}
}

// A server built before CreateHarness has a HarnessRegistry service without the method. A client
// calling it gets UNIMPLEMENTED and the name stays free, where an ignored create-only field on
// RegisterHarnessRequest would have registered or reactivated it.
func TestCreateHarnessOnOlderServerIsUnimplemented(t *testing.T) {
	ctx := context.Background()
	h := startRegistryHost(t, filepath.Join(t.TempDir(), "journal.db"))
	older := v1.HarnessRegistry_ServiceDesc
	older.Methods = nil
	for _, m := range v1.HarnessRegistry_ServiceDesc.Methods {
		if m.MethodName != "CreateHarness" {
			older.Methods = append(older.Methods, m)
		}
	}
	if len(older.Methods) != len(v1.HarnessRegistry_ServiceDesc.Methods)-1 {
		t.Fatalf("service has no CreateHarness method to remove: %v", v1.HarnessRegistry_ServiceDesc.Methods)
	}
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	srv.RegisterService(&older, h.reg)
	go srv.Serve(lis)
	t.Cleanup(srv.Stop)
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	_, err = v1.NewHarnessRegistryClient(conn).CreateHarness(ctx, &v1.CreateHarnessRequest{Name: "fresh", Spec: remoteSpec("127.0.0.1:9000")})
	if status.Code(err) != codes.Unimplemented {
		t.Fatalf("CreateHarness on an older server = %v, want Unimplemented", err)
	}
	if _, err := h.store.Harness("fresh"); !errors.Is(err, sqlitelog.ErrHarnessNotFound) {
		t.Fatalf("CreateHarness on an older server stored a row: %v", err)
	}
}
