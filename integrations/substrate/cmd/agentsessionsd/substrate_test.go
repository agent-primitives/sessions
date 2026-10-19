package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"flag"
	"log/slog"
	"maps"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/agent-primitives/sessions/api"
	v1 "github.com/agent-primitives/sessions/api/genpb"
	"github.com/agent-primitives/sessions/client"
	"github.com/agent-primitives/sessions/harness/echoagent"
	"github.com/agent-primitives/sessions/harnesswire"
	"github.com/agent-primitives/sessions/internal/agentsessionsd"
	"github.com/agent-primitives/sessions/placement"
	"github.com/agent-primitives/sessions/runtime/substrate"
	"github.com/agent-primitives/sessions/session"
	"github.com/agent-primitives/sessions/sqlitelog"
)

func TestSubstrateHarnessFlagParsing(t *testing.T) {
	for _, tt := range []struct {
		name    string
		values  []string
		want    substrateHarnesses
		wantErr string
	}{
		{
			name:   "stateless",
			values: []string{"writer=agentsuite/minimal-writer-1a2b3c4d"},
			want:   substrateHarnesses{"writer": {template: substrate.ObjectRef{Atespace: "agentsuite", Name: "minimal-writer-1a2b3c4d"}}},
		},
		{
			name:   "memory",
			values: []string{" counter = tmpl/counter,memory "},
			want:   substrateHarnesses{"counter": {template: substrate.ObjectRef{Atespace: "tmpl", Name: "counter"}, memory: true}},
		},
		{name: "no equals", values: []string{"writer"}, wantErr: "want name=atespace/template"},
		{name: "empty name", values: []string{"=t/a"}, wantErr: "want name=atespace/template"},
		{name: "no atespace", values: []string{"a=template"}, wantErr: "is not atespace/template"},
		{name: "empty template", values: []string{"a=t/"}, wantErr: "is not atespace/template"},
		{name: "nested path", values: []string{"a=t/b/c"}, wantErr: "is not atespace/template"},
		{name: "uppercase", values: []string{"a=T/b"}, wantErr: "is not atespace/template"},
		{name: "unknown option", values: []string{"a=t/b,snapshot"}, wantErr: `unknown option "snapshot"`},
		{name: "a second harness", values: []string{"a=t/a", "b=t/b,memory"}, wantErr: `given more than once (already "a"): only one substrate harness is supported`},
		{name: "the same harness twice", values: []string{"a=t/b", "a=t/b"}, wantErr: `given more than once (already "a")`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := substrateHarnesses{}
			var err error
			for _, v := range tt.values {
				if err = got.Set(v); err != nil {
					break
				}
			}
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("Set(%q) error = %v, want %q", tt.values, err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("harnesses = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// The neutral server, without this build's hook, does not register the substrate flags, so it
// refuses them as unknown instead of accepting a backend it cannot serve.
func TestNeutralServerRefusesSubstrateFlags(t *testing.T) {
	journal := filepath.Join(t.TempDir(), "journal.db")
	err := agentsessionsd.Run([]string{"-journal", journal, "-model", "m", "-substrate-harness", "w=t/w"})
	if !errors.Is(err, agentsessionsd.ErrUsage) || !strings.Contains(err.Error(), "flag provided but not defined: -substrate-harness") {
		t.Fatalf("neutral Run error = %v, want an undefined-flag usage error", err)
	}
	if err := agentsessionsd.Run([]string{"-h"}); !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("-h error = %v, want flag.ErrHelp", err)
	}
}

// run runs the daemon as this build does, with newControl standing in for the ate-api adapter.
func run(args []string, newControl controlFunc) error {
	return agentsessionsd.Run(args, agentsessionsd.WithHarnessBackends(newSubstrateFlags(newControl)))
}

// parseFlags registers this build's flags on a fresh flag set, parses args and runs the startup
// checks, as agentsessionsd.Run does.
func parseFlags(t *testing.T, newControl controlFunc, args ...string) *substrateFlags {
	t.Helper()
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	f := newSubstrateFlags(newControl)
	f.RegisterFlags(fs)
	if err := fs.Parse(args); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Harnesses(); err != nil {
		t.Fatal(err)
	}
	return f
}

// A misconfigured substrate harness is refused before Run opens the journal or dials anything.
func TestSubstrateStartupChecks(t *testing.T) {
	dir := t.TempDir()
	token := filepath.Join(dir, "token")
	if err := os.WriteFile(token, []byte("tok\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	empty := filepath.Join(dir, "empty")
	if err := os.WriteFile(empty, []byte(" \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dialed := false
	newControl := func(grpc.ClientConnInterface) substrate.ControlClient { dialed = true; return nil }
	base := []string{"-model", "m", "-substrate-harness", "w=t/w", "-substrate-atespace", "sessions"}
	for _, tt := range []struct {
		name string
		args []string
		want string
	}{
		{name: "no model", args: []string{"-substrate-harness", "w=t/w", "-substrate-atespace", "s"}, want: "harness w requires -model"},
		{name: "no atespace", args: []string{"-model", "m", "-substrate-harness", "w=t/w"}, want: "-substrate-atespace"},
		{name: "bad atespace", args: []string{"-model", "m", "-substrate-harness", "w=t/w", "-substrate-atespace", "Bad_Space"}, want: "-substrate-atespace"},
		{name: "built-in name", args: []string{"-model", "m", "-substrate-harness", "chat=t/c", "-substrate-atespace", "s"}, want: `harness "chat" is already served`},
		{name: "also a remote", args: append(slices.Clone(base), "-harness", "w=127.0.0.1:9000"), want: `harness "w" is given with -harness and is also configured by this build's flags`},
		{name: "bad ate-api address", args: append(slices.Clone(base), "-ateapi-addr", "https://api:443"), want: "-ateapi-addr"},
		{name: "bad router address", args: append(slices.Clone(base), "-atenet-router", "router"), want: "-atenet-router"},
		{name: "ca and insecure", args: append(slices.Clone(base), "-ateapi-ca-file", token, "-ateapi-insecure-skip-verify"), want: "mutually exclusive"},
		{name: "missing token", args: append(slices.Clone(base), "-ateapi-token-file", filepath.Join(dir, "absent")), want: "-ateapi-token-file"},
		{name: "empty token", args: append(slices.Clone(base), "-ateapi-token-file", empty), want: "is empty"},
		{name: "substrate flags without a harness", args: []string{"-model", "m", "-substrate-atespace", "s", "-atenet-router", "router:80"}, want: "-atenet-router, -substrate-atespace given without -substrate-harness: these flags only configure substrate harnesses"},
		{name: "a boolean substrate flag without a harness", args: []string{"-ateapi-insecure-skip-verify"}, want: "-ateapi-insecure-skip-verify given without -substrate-harness: these flags only configure substrate harnesses"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			journal := filepath.Join(t.TempDir(), "journal.db")
			err := run(append([]string{"-journal", journal, "-addr", "127.0.0.1:0"}, tt.args...), newControl)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Run error = %v, want %q", err, tt.want)
			}
			if _, statErr := os.Stat(journal); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("a refused start left a journal behind: %v", statErr)
			}
			if dialed {
				t.Fatal("a refused start built a control client")
			}
		})
	}
}

// Substrate harnesses would share one actor per session, so the daemon refuses a second
// -substrate-harness as a command-line error, which exits 2 (agentsessionsd prints the flag
// package's reason and the usage), before it opens the journal or dials the ate-api.
func TestSecondSubstrateHarnessIsRefused(t *testing.T) {
	journal := filepath.Join(t.TempDir(), "journal.db")
	dialed := false
	newControl := func(grpc.ClientConnInterface) substrate.ControlClient { dialed = true; return nil }
	err := run([]string{"-journal", journal, "-addr", "127.0.0.1:0", "-model", "m", "-substrate-atespace", "s",
		"-substrate-harness", "a=t/a", "-substrate-harness", "b=t/b,memory"}, newControl)
	if !errors.Is(err, agentsessionsd.ErrUsage) {
		t.Fatalf("Run error = %v, want ErrUsage (exit status 2)", err)
	}
	const want = `invalid value "b=t/b,memory" for flag -substrate-harness: given more than once (already "a"): only one substrate harness is supported, because substrate harnesses would share one actor per session`
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("Run error = %v, want %q", err, want)
	}
	if _, statErr := os.Stat(journal); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("a refused start left a journal behind: %v", statErr)
	}
	if dialed {
		t.Fatal("a refused start built a control client")
	}
}

func TestFileTokenCreds(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	creds := fileTokenCreds{path: path}
	if !creds.RequireTransportSecurity() {
		t.Fatal("a bearer token must never be sent over a cleartext connection")
	}
	if _, err := creds.GetRequestMetadata(t.Context()); !errors.Is(err, errTokenUnavailable) {
		t.Fatalf("missing token error = %v, want %v: a missing token file must fail the call, not send it unauthenticated", err, errTokenUnavailable)
	}
	for _, tok := range []string{"first", "rotated"} {
		if err := os.WriteFile(path, []byte(tok+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		md, err := creds.GetRequestMetadata(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if got, want := md["authorization"], "Bearer "+tok; got != want {
			t.Fatalf("authorization = %q, want %q (the file is re-read on every call)", got, want)
		}
	}
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := creds.GetRequestMetadata(t.Context()); !errors.Is(err, errTokenUnavailable) {
		t.Fatalf("empty token error = %v, want %v", err, errTokenUnavailable)
	}
}

// The ate-api connection verifies the server against -ateapi-ca-file and carries the token file as
// a bearer on every call. The test server accepts any method, so it observes exactly what the
// ControlClient's calls would carry.
func TestSubstrateDialsATEAPIWithTLSAndToken(t *testing.T) {
	dir := t.TempDir()
	certPEM, keyPEM := selfSignedCert(t, "api.ate-system.svc")
	caFile := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(caFile, certPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	tokenFile := filepath.Join(dir, "token")
	if err := os.WriteFile(tokenFile, []byte("sa-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan metadata.MD, 1)
	srv := grpc.NewServer(
		grpc.Creds(credentials.NewTLS(&tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})),
		grpc.UnknownServiceHandler(func(_ any, stream grpc.ServerStream) error {
			md, _ := metadata.FromIncomingContext(stream.Context())
			got <- md
			return nil
		}),
	)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	for _, tt := range []struct {
		name    string
		flags   []string
		wantErr bool
	}{
		{name: "ca file", flags: []string{"-ateapi-ca-file", caFile, "-ateapi-server-name", "api.ate-system.svc"}},
		{name: "insecure skip verify", flags: []string{"-ateapi-insecure-skip-verify"}},
		{name: "system roots refuse a private CA", flags: []string{"-ateapi-server-name", "api.ate-system.svc"}, wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var conn grpc.ClientConnInterface
			args := append([]string{"-substrate-harness", "w=t/w", "-substrate-atespace", "s", "-ateapi-addr", lis.Addr().String(), "-ateapi-token-file", tokenFile}, tt.flags...)
			f := parseFlags(t, func(c grpc.ClientConnInterface) substrate.ControlClient { conn = c; return nil }, args...)
			_, closeConn, err := f.Backends(nil)
			if err != nil {
				t.Fatal(err)
			}
			defer closeConn()
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			err = conn.Invoke(ctx, "/ateapi.Control/GetActor", &emptypb.Empty{}, &emptypb.Empty{})
			if tt.wantErr {
				if err == nil || !strings.Contains(err.Error(), "certificate") {
					t.Fatalf("call error = %v, want a certificate verification failure", err)
				}
				return
			}
			select {
			case md := <-got:
				if a := md.Get("authorization"); len(a) != 1 || a[0] != "Bearer sa-token" {
					t.Fatalf("authorization = %q, want the token file as a bearer", a)
				}
			case <-ctx.Done():
				t.Fatalf("server saw no call: %v", err)
			}
		})
	}
}

// Every ate-api call carries a deadline of at most ateapiCallTimeout, so a hung ate-api fails the
// call when the caller's context has none, and a caller's earlier deadline is kept.
func TestSubstrateBoundsEachATEAPICall(t *testing.T) {
	old := ateapiCallTimeout
	ateapiCallTimeout = 200 * time.Millisecond
	t.Cleanup(func() { ateapiCallTimeout = old })

	dir := t.TempDir()
	certPEM, keyPEM := selfSignedCert(t, "api.ate-system.svc")
	caFile := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(caFile, certPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	// The server hangs until the call's context ends and reports the deadline it saw.
	deadlines := make(chan time.Duration, 2)
	srv := grpc.NewServer(
		grpc.Creds(credentials.NewTLS(&tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})),
		grpc.UnknownServiceHandler(func(_ any, stream grpc.ServerStream) error {
			d, ok := stream.Context().Deadline()
			if !ok {
				deadlines <- -1
			} else {
				deadlines <- time.Until(d)
			}
			<-stream.Context().Done()
			return stream.Context().Err()
		}),
	)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	var conn grpc.ClientConnInterface
	f := parseFlags(t, func(c grpc.ClientConnInterface) substrate.ControlClient { conn = c; return nil },
		"-substrate-harness", "w=t/w", "-substrate-atespace", "s", "-ateapi-addr", lis.Addr().String(),
		"-ateapi-ca-file", caFile, "-ateapi-server-name", "api.ate-system.svc")
	_, closeConn, err := f.Backends(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer closeConn()

	// No caller deadline: the call fails with DeadlineExceeded after ateapiCallTimeout.
	done := make(chan error, 1)
	go func() {
		done <- conn.Invoke(context.Background(), "/ateapi.Control/GetActor", &emptypb.Empty{}, &emptypb.Empty{})
	}()
	select {
	case err = <-done:
	case <-time.After(5 * time.Second):
		srv.Stop() // ends the hung call
		t.Fatalf("call with no deadline still running after 5s, want it ended after %v", ateapiCallTimeout)
	}
	if status.Code(err) != codes.DeadlineExceeded {
		t.Fatalf("call with no deadline: err = %v, want DeadlineExceeded", err)
	}
	if d := <-deadlines; d < 0 || d > ateapiCallTimeout {
		t.Fatalf("server saw deadline in %v, want at most %v", d, ateapiCallTimeout)
	}

	// An earlier caller deadline is kept.
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	err = conn.Invoke(ctx, "/ateapi.Control/GetActor", &emptypb.Empty{}, &emptypb.Empty{})
	if status.Code(err) != codes.DeadlineExceeded {
		t.Fatalf("call with an earlier deadline: err = %v, want DeadlineExceeded", err)
	}
	if d := <-deadlines; d < 0 || d > 50*time.Millisecond {
		t.Fatalf("server saw deadline in %v, want the caller's 50ms at most", d)
	}
}

// A session on the -substrate-harness backend runs through the substrate wiring: the session's
// actor is created from the configured template in -substrate-atespace and resumed, its turns reach
// the harness through -atenet-router with the ate-target-actor metadata naming that actor, and
// suspend suspends it. How agentsessionsd registers and reserves a HarnessBackends harness is
// tested in its own module.
func TestSubstrateHarnessRunsThroughTheRouter(t *testing.T) {
	routed := make(chan string, 16)
	router := serveRouter(t, echoagent.Harness{}, routed)
	ctl := &fakeControl{actors: map[string]substrate.ActorStatus{}}

	f := parseFlags(t, func(grpc.ClientConnInterface) substrate.ControlClient { return ctl },
		"-substrate-harness", "writer=agentsuite/minimal-writer-1a2b3c4d", "-substrate-atespace", "sessions", "-atenet-router", router, "-ateapi-insecure-skip-verify")
	backends, closeConn, err := f.Backends(nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(closeConn)
	if got, want := slices.Sorted(maps.Keys(backends)), []string{"writer"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("backends = %v, want %v", got, want)
	}
	registry, err := placement.NewRegistry("writer", map[string]*placement.Placer{"writer": placement.New(backends["writer"], echoagent.Model)})
	if err != nil {
		t.Fatal(err)
	}
	placer, err := registry.For("writer")
	if err != nil {
		t.Fatal(err)
	}
	desc, err := placer.Describe(t.Context())
	if err != nil || desc.Capabilities.Resumability != api.ResumabilityStatelessReplay {
		t.Fatalf("writer descriptor = %+v, %v; want the declared STATELESS_REPLAY tier", desc, err)
	}

	c := newTestClient(t, registry)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	turn, err := c.Exec(ctx, client.ExecOptions{Harness: "writer", Inputs: []string{"hello"}})
	if err != nil {
		t.Fatalf("exec on substrate harness: %v", err)
	}
	uid := turn.Session.GetMetadata().GetUid()
	if turn.Output != "echo:hello" {
		t.Fatalf("output = %q, want echo:hello", turn.Output)
	}
	if want := "create:sessions/" + uid + ":tmpl=agentsuite/minimal-writer-1a2b3c4d"; !slices.Contains(ctl.log(), want) {
		t.Fatalf("control calls = %v, want %q", ctl.log(), want)
	}
	select {
	case target := <-routed:
		if target != "sessions/"+uid {
			t.Fatalf("router saw ate-target-actor %q, want sessions/%s", target, uid)
		}
	default:
		t.Fatal("the turn did not reach the harness through the router")
	}
	if _, err := c.Suspend(ctx, uid); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	if !slices.Contains(ctl.log(), "suspend:sessions/"+uid) {
		t.Fatalf("control calls = %v, want the actor suspended", ctl.log())
	}
	if _, err := c.Resume(ctx, uid, false); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if _, err := c.Exec(ctx, client.ExecOptions{Session: uid, Inputs: []string{"again"}}); err != nil {
		t.Fatalf("exec after resume: %v", err)
	}
}

// newTestClient serves the Sessions API for registry over an in-memory journal and connection.
func newTestClient(t *testing.T, registry *placement.Registry) *client.Client {
	t.Helper()
	store, err := sqlitelog.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	v1.RegisterSessionsServer(srv, session.NewService(store, registry))
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	c, err := client.Dial("passthrough:///bufnet", client.WithDialOptions(
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
	))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// serveRouter stands in for the atenet-router: a harnesswire server that reports the
// ate-target-actor metadata of every stream it accepts.
func serveRouter(t *testing.T, h api.Harness, routed chan<- string) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	record := func(ctx context.Context) {
		md, _ := metadata.FromIncomingContext(ctx)
		if v := md.Get(substrate.TargetActorHeader); len(v) == 1 {
			select {
			case routed <- v[0]:
			default:
			}
		}
	}
	srv := grpc.NewServer(
		grpc.ChainStreamInterceptor(func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
			if strings.HasSuffix(info.FullMethod, "/Connect") {
				record(ss.Context())
			}
			return handler(srv, ss)
		}),
	)
	v1.RegisterHarnessServer(srv, harnesswire.NewServer(h))
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return lis.Addr().String()
}

// fakeControl is a minimal lifecycle-faithful substrate.ControlClient.
type fakeControl struct {
	mu     sync.Mutex
	actors map[string]substrate.ActorStatus
	calls  []string
}

func (f *fakeControl) log() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

func (f *fakeControl) CreateActor(_ context.Context, a substrate.ActorRef, tmpl substrate.ObjectRef) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "create:"+a.Atespace+"/"+a.Name+":tmpl="+tmpl.Atespace+"/"+tmpl.Name)
	f.actors[a.Name] = substrate.StatusSuspended
	return nil
}

func (f *fakeControl) ResumeActor(_ context.Context, a substrate.ActorRef) (substrate.ActorInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "resume:"+a.Atespace+"/"+a.Name)
	if _, ok := f.actors[a.Name]; !ok {
		return substrate.ActorInfo{}, substrate.ErrActorNotFound
	}
	f.actors[a.Name] = substrate.StatusRunning
	return substrate.ActorInfo{Status: substrate.StatusRunning, Worker: "worker-0", UID: "uid-" + a.Name}, nil
}

func (f *fakeControl) SuspendActor(_ context.Context, a substrate.ActorRef) (substrate.ActorInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "suspend:"+a.Atespace+"/"+a.Name)
	f.actors[a.Name] = substrate.StatusSuspended
	return substrate.ActorInfo{Status: substrate.StatusSuspended, Snapshot: "mem://" + a.Name, UID: "uid-" + a.Name}, nil
}

func (f *fakeControl) DeleteActor(_ context.Context, a substrate.ActorRef) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "delete:"+a.Atespace+"/"+a.Name)
	delete(f.actors, a.Name)
	return nil
}

func (f *fakeControl) GetActor(_ context.Context, a substrate.ActorRef) (substrate.ActorInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	st, ok := f.actors[a.Name]
	if !ok {
		return substrate.ActorInfo{}, substrate.ErrActorNotFound
	}
	return substrate.ActorInfo{Status: st, UID: "uid-" + a.Name}, nil
}

func selfSignedCert(t *testing.T, host string) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: host},
		DNSNames:              []string{host},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
}

// After an ate-api failure the connection retries within about a second rather than on gRPC's
// default backoff, which waits 1s, then 1.6s, 2.56s and so on up to 120s. The server here accepts
// and closes every connection, so each accept is one reconnect attempt: in 3s the bounded backoff
// makes six or seven of them, even with the most jitter, and the default at most three. The bar is
// five to leave room for a loaded machine.
func TestSubstrateATEAPIReconnectsQuickly(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var attempts atomic.Int32
	go func() {
		for {
			c, err := lis.Accept()
			if err != nil {
				return
			}
			attempts.Add(1)
			_ = c.Close()
		}
	}()
	t.Cleanup(func() { _ = lis.Close() })

	var conn grpc.ClientConnInterface
	f := parseFlags(t, func(c grpc.ClientConnInterface) substrate.ControlClient { conn = c; return nil },
		"-substrate-harness", "w=t/w", "-substrate-atespace", "s", "-ateapi-addr", lis.Addr().String(), "-ateapi-insecure-skip-verify")
	_, closeConn, err := f.Backends(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer closeConn()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	// WaitForReady keeps the call pending, so the channel keeps reconnecting until the deadline.
	_ = conn.Invoke(ctx, "/ateapi.Control/GetActor", &emptypb.Empty{}, &emptypb.Empty{}, grpc.WaitForReady(true))
	n := attempts.Load()
	t.Logf("%d connection attempts in 3s", n)
	if n < 5 {
		t.Fatalf("%d connection attempts in 3s, want at least 5 (a bounded reconnect backoff)", n)
	}
}

// TestSubstrateFlagsRefusedWithoutHarness sets each flag RegisterFlags registers, one at a time and
// without -substrate-harness, and wants Harnesses to refuse it, so a substrate flag added later
// cannot be silently ignored. A flag the FlagSet held before registration is not refused.
func TestSubstrateFlagsRefusedWithoutHarness(t *testing.T) {
	newFlags := func() (*flag.FlagSet, *substrateFlags) {
		fs := flag.NewFlagSet("test", flag.ContinueOnError)
		fs.String("model", "", "")
		f := newSubstrateFlags(nil)
		f.RegisterFlags(fs)
		return fs, f
	}
	fs, f := newFlags()
	if err := fs.Parse([]string{"-model", "m"}); err != nil {
		t.Fatal(err)
	}
	if names, err := f.Harnesses(); err != nil || len(names) != 0 {
		t.Fatalf("Harnesses() = %q, %v; want no harness and no refusal of a flag registered before the substrate flags", names, err)
	}
	var names []string
	fs.VisitAll(func(fl *flag.Flag) {
		if fl.Name != "model" && fl.Name != "substrate-harness" {
			names = append(names, fl.Name)
		}
	})
	if len(names) == 0 {
		t.Fatal("RegisterFlags registered no flags besides -substrate-harness")
	}
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			fs, f := newFlags()
			arg := "-" + name + "=x"
			if bf, ok := fs.Lookup(name).Value.(interface{ IsBoolFlag() bool }); ok && bf.IsBoolFlag() {
				arg = "-" + name
			}
			if err := fs.Parse([]string{arg}); err != nil {
				t.Fatal(err)
			}
			want := "-" + name + " given without -substrate-harness"
			if _, err := f.Harnesses(); err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("Harnesses error = %v, want %q", err, want)
			}
		})
	}
}

// A token file that disappears or empties after startup fails the session's ate-api call, and the
// error a Sessions API client sees names neither the file nor its content: the cause is logged for
// the operator instead. The test drives Exec through the Sessions API over the real ate-api adapter
// and a TLS ate-api, which the call never reaches because the per-call credentials fail first.
func TestSubstrateTokenFileErrorIsNotSentToClients(t *testing.T) {
	certPEM, keyPEM := selfSignedCert(t, "api.ate-system.svc")
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	reached := make(chan struct{}, 16)
	srv := grpc.NewServer(
		grpc.Creds(credentials.NewTLS(&tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})),
		grpc.UnknownServiceHandler(func(any, grpc.ServerStream) error {
			reached <- struct{}{}
			return status.Error(codes.Unavailable, "test ate-api")
		}),
	)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	for _, tt := range []struct {
		name  string
		after func(path string) error
	}{
		{name: "deleted", after: os.Remove},
		{name: "emptied", after: func(path string) error { return os.WriteFile(path, nil, 0o600) }},
		{name: "whitespace", after: func(path string) error { return os.WriteFile(path, []byte(" \n\t"), 0o600) }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			tokenFile := filepath.Join(t.TempDir(), "ateapi-token-"+tt.name)
			const tok = "sa-token-content"
			if err := os.WriteFile(tokenFile, []byte(tok), 0o600); err != nil {
				t.Fatal(err)
			}
			f := parseFlags(t, newControl,
				"-substrate-harness", "writer=agentsuite/writer", "-substrate-atespace", "sessions",
				"-ateapi-addr", lis.Addr().String(), "-ateapi-insecure-skip-verify", "-ateapi-token-file", tokenFile)
			var logs syncBuffer
			backends, closeConn, err := f.Backends(slog.New(slog.NewTextHandler(&logs, nil)))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(closeConn)
			registry, err := placement.NewRegistry("writer", map[string]*placement.Placer{"writer": placement.New(backends["writer"], echoagent.Model)})
			if err != nil {
				t.Fatal(err)
			}
			c := newTestClient(t, registry)

			if err := tt.after(tokenFile); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			_, err = c.Exec(ctx, client.ExecOptions{Harness: "writer", Inputs: []string{"hello"}})
			if err == nil {
				t.Fatal("exec succeeded without a token")
			}
			st, _ := status.FromError(err)
			if st.Code() != codes.Internal {
				t.Fatalf("exec error code = %v (%v), want Internal, the code the client saw before the message was sanitized", st.Code(), err)
			}
			for _, leak := range []string{tokenFile, filepath.Base(tokenFile), tok} {
				if strings.Contains(st.Message(), leak) {
					t.Fatalf("client-visible error %q contains %q", st.Message(), leak)
				}
			}
			if !strings.Contains(st.Message(), "ate-api token unavailable") {
				t.Fatalf("client-visible error %q, want it to say the ate-api token is unavailable", st.Message())
			}
			select {
			case <-reached:
				t.Fatal("the call reached the ate-api without a token")
			default:
			}
			if got := logs.String(); !strings.Contains(got, tokenFile) || strings.Contains(got, tok) {
				t.Fatalf("daemon log = %q, want the token file's path for the operator and never its content", got)
			}
		})
	}
}

// syncBuffer is a log destination safe to write from gRPC goroutines and read from the test.
type syncBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}
