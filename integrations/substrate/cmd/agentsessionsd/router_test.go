package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"log/slog"
	"math/big"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"github.com/agent-primitives/sessions/runtime/substrate"

	"github.com/agent-primitives/sessions/integrations/substrate/internal/routertls"
)

// testRouterClientID is the SPIFFE ID of the router client the tests mount.
const testRouterClientID = "spiffe://cluster.local/ns/sessions/sa/agentsessions"

// routerPKI stands in for substrate's two pod-certificate signers: podidentity issues the router
// client's SVID, and servicedns the router's serving certificate. TestMain writes the client's
// files once; every test that starts a substrate harness mounts them unless it sets its own.
var routerPKI struct {
	certFile, caFile string
	clientCA         *x509.CertPool
	serverCert       tls.Certificate
	serial           atomic.Int64
	podidentity      testCA
	servicedns       testCA
}

type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
}

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "router-pki")
	if err != nil {
		panic(err)
	}
	code := func() int {
		defer func() { _ = os.RemoveAll(dir) }()
		if err := setupRouterPKI(dir); err != nil {
			panic(err)
		}
		return m.Run()
	}()
	os.Exit(code)
}

func setupRouterPKI(dir string) error {
	var err error
	if routerPKI.podidentity, err = newTestCA("podidentity"); err != nil {
		return err
	}
	if routerPKI.servicedns, err = newTestCA("servicedns"); err != nil {
		return err
	}
	routerPKI.clientCA = x509.NewCertPool()
	routerPKI.clientCA.AddCert(routerPKI.podidentity.cert)
	if routerPKI.serverCert, _, err = routerPKI.servicedns.issue([]string{routertls.DefaultServerName}, nil, time.Now().Add(24*time.Hour)); err != nil {
		return err
	}
	_, bundle, err := routerPKI.podidentity.issue(nil, []string{testRouterClientID}, time.Now().Add(24*time.Hour))
	if err != nil {
		return err
	}
	routerPKI.certFile = filepath.Join(dir, "credential-bundle.pem")
	routerPKI.caFile = filepath.Join(dir, "router-ca.pem")
	if err := os.WriteFile(routerPKI.certFile, bundle, 0o600); err != nil {
		return err
	}
	return os.WriteFile(routerPKI.caFile, pemCert(routerPKI.servicedns.cert.Raw), 0o600)
}

func newTestCA(name string) (testCA, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return testCA{}, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(routerPKI.serial.Add(1)),
		Subject:               pkix.Name{CommonName: name},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(48 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return testCA{}, err
	}
	cert, err := x509.ParseCertificate(der)
	return testCA{cert: cert, key: key}, err
}

// issue returns a leaf as a tls.Certificate and as a podCertificate credential bundle.
func (c testCA) issue(dns, uris []string, notAfter time.Time) (tls.Certificate, []byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(routerPKI.serial.Add(1)),
		DNSNames:     dns,
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
	}
	for _, s := range uris {
		u, err := url.Parse(s)
		if err != nil {
			return tls.Certificate{}, nil, err
		}
		tmpl.URIs = append(tmpl.URIs, u)
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, c.cert, &key.PublicKey, c.key)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	kder, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	bundle := append(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kder}), pemCert(der)...)
	return tls.Certificate{Certificate: [][]byte{der, c.cert.Raw}, PrivateKey: key}, bundle, nil
}

func pemCert(der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// withRouterClient adds the shared router client files to a start that configures a substrate
// harness and sets no router client of its own.
func withRouterClient(args []string) []string {
	if !slices.Contains(args, "-substrate-harness") || slices.ContainsFunc(args, func(a string) bool { return strings.HasPrefix(a, "-atenet-router-cert-file") }) {
		return args
	}
	return append(slices.Clone(args), "-atenet-router-cert-file", routerPKI.certFile, "-atenet-router-ca-file", routerPKI.caFile)
}

// routerServerTLS is the TLS config of a router that, like v0.6.0's envoy, requires a podidentity
// client certificate and admits only the allowed SPIFFE ID.
func routerServerTLS(cert tls.Certificate, allowed string) *tls.Config {
	return &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{cert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    routerPKI.clientCA,
		VerifyPeerCertificate: func(_ [][]byte, chains [][]*x509.Certificate) error {
			for _, u := range chains[0][0].URIs {
				if u.String() == allowed {
					return nil
				}
			}
			return errors.New("SPIFFE ID not allowlisted")
		},
	}
}

// serveTLSRouter serves an empty gRPC server behind routerServerTLS.
func serveTLSRouter(t *testing.T, cert tls.Certificate, allowed string) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer(grpc.Creds(credentials.NewTLS(routerServerTLS(cert, allowed))))
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return lis.Addr().String()
}

func writeFile(t *testing.T, name string, b []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// A substrate harness needs a router client: both files, a podidentity SVID with one SPIFFE ID and
// a CA bundle. Each refusal comes before the journal opens or anything is dialed.
func TestSubstrateRouterClientStartupChecks(t *testing.T) {
	_, expired, err := routerPKI.podidentity.issue(nil, []string{testRouterClientID}, time.Now().Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	_, servicedns, err := routerPKI.servicedns.issue([]string{"agentsessions.sessions.svc"}, nil, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	base := []string{"-model", "m", "-substrate-harness", "w=t/w", "-substrate-atespace", "sessions"}
	dir := t.TempDir()
	for _, tt := range []struct {
		name  string
		flags []string
		want  string
	}{
		{name: "no router client", flags: []string{"-atenet-router-cert-file="}, want: "-atenet-router-cert-file and -atenet-router-ca-file are required with -substrate-harness"},
		{name: "no CA", flags: []string{"-atenet-router-cert-file", routerPKI.certFile}, want: "are required with -substrate-harness"},
		{name: "missing SVID", flags: []string{"-atenet-router-cert-file", filepath.Join(dir, "absent"), "-atenet-router-ca-file", routerPKI.caFile}, want: "no such file"},
		{name: "missing CA", flags: []string{"-atenet-router-cert-file", routerPKI.certFile, "-atenet-router-ca-file", filepath.Join(dir, "absent")}, want: "router CA"},
		{name: "expired SVID", flags: []string{"-atenet-router-cert-file", writeFile(t, "expired.pem", expired), "-atenet-router-ca-file", routerPKI.caFile}, want: "expired at"},
		{name: "servicedns certificate mounted as the SVID", flags: []string{"-atenet-router-cert-file", writeFile(t, "servicedns.pem", servicedns), "-atenet-router-ca-file", routerPKI.caFile}, want: "spiffe:// URI SAN"},
		{name: "CA without certificates", flags: []string{"-atenet-router-cert-file", routerPKI.certFile, "-atenet-router-ca-file", writeFile(t, "ca.pem", []byte("x"))}, want: "no PEM certificates"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dialed := false
			journal := filepath.Join(t.TempDir(), "journal.db")
			err := run(append(append([]string{"-journal", journal, "-addr", "127.0.0.1:0"}, base...), tt.flags...), func(grpc.ClientConnInterface) substrate.ControlClient {
				dialed = true
				return nil
			})
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

func TestRouterProbe(t *testing.T) {
	creds, err := routertls.Load(routertls.Config{CertFile: routerPKI.certFile, CAFile: routerPKI.caFile})
	if err != nil {
		t.Fatal(err)
	}
	impostorCA, err := newTestCA("impostor")
	if err != nil {
		t.Fatal(err)
	}
	impostor, _, err := impostorCA.issue([]string{routertls.DefaultServerName}, nil, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closedAddr := closed.Addr().String()
	_ = closed.Close()

	for _, tt := range []struct {
		name    string
		addr    string
		want    probeOutcome
		wantLog string
	}{
		{name: "allowlisted", addr: serveTLSRouter(t, routerPKI.serverCert, testRouterClientID), want: probeAccepted, wantLog: "atenet-router accepted the router client"},
		{name: "not on the allowlist", addr: serveTLSRouter(t, routerPKI.serverCert, "spiffe://cluster.local/ns/x/sa/y"), want: probeRefused, wantLog: "add its ID to the router's --ingress-allowed-spiffe-ids"},
		{name: "router from another CA", addr: serveTLSRouter(t, impostor, testRouterClientID), want: probeUnverified, wantLog: "cannot verify the atenet-router's serving certificate"},
		{name: "router unreachable", addr: closedAddr, want: probeFailed, wantLog: "atenet-router probe failed"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			got, err := probeRouter(ctx, creds, tt.addr)
			if got != tt.want {
				t.Fatalf("probeRouter = %d (%v), want %d", got, err, tt.want)
			}
			var logs bytes.Buffer
			logRouterProbe(slog.New(slog.NewTextHandler(&logs, nil)), got, err, creds, tt.addr, routerPKI.caFile)
			if !strings.Contains(logs.String(), tt.wantLog) || !strings.Contains(logs.String(), testRouterClientID) && tt.want != probeUnverified {
				t.Fatalf("log = %q, want %q and the client ID", logs.String(), tt.wantLog)
			}
		})
	}
}

// The probe runs in the background: Backends returns while a router that never answers holds it,
// and closing the backends cancels it.
func TestRouterProbeDoesNotHoldStartup(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lis.Close() })
	accepted := make(chan net.Conn, 1)
	go func() {
		if c, err := lis.Accept(); err == nil {
			accepted <- c // never answers the handshake
		}
	}()
	f := parseFlags(t, newControl, "-substrate-harness", "w=t/w", "-substrate-atespace", "s", "-atenet-router", lis.Addr().String(), "-ateapi-insecure-skip-verify")
	var logs syncBuffer
	start := time.Now()
	_, _, closeConn, err := f.Backends(slog.New(slog.NewTextHandler(&logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("Backends took %v with a silent router, want it not to wait for the probe", d)
	}
	select {
	case c := <-accepted:
		defer func() { _ = c.Close() }()
	case <-time.After(5 * time.Second):
		t.Fatal("the probe never connected to the router")
	}
	start = time.Now()
	closeConn()
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("closing the backends took %v, want the probe cancelled", d)
	}
	if !strings.Contains(logs.String(), "atenet-router probe failed") {
		t.Fatalf("log = %q, want the cancelled probe logged as failed", logs.String())
	}
}
