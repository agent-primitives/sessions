package routertls_test

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
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"

	"github.com/agent-primitives/sessions/api"
	v1 "github.com/agent-primitives/sessions/api/genpb"
	"github.com/agent-primitives/sessions/harness/echoagent"
	"github.com/agent-primitives/sessions/harnesswire"

	"github.com/agent-primitives/sessions/integrations/substrate/internal/routertls"
)

const (
	routerName = routertls.DefaultServerName
	clientID   = "spiffe://cluster.local/ns/sessions/sa/agentsessions"
)

var (
	serial  int64
	serialM sync.Mutex
	t0      = time.Now()
)

type ca struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pem  []byte
}

func newKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func nextSerial() *big.Int {
	serialM.Lock()
	defer serialM.Unlock()
	serial++
	return big.NewInt(serial)
}

func newCA(t *testing.T, name string) *ca {
	t.Helper()
	key := newKey(t)
	tmpl := &x509.Certificate{
		SerialNumber:          nextSerial(),
		Subject:               pkix.Name{CommonName: name},
		NotBefore:             t0.Add(-time.Hour),
		NotAfter:              t0.Add(48 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	return &ca{cert: cert, key: key, pem: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}
}

type leafOpts struct {
	dns       []string
	uris      []string
	notBefore time.Time
	notAfter  time.Time
}

// issue returns a leaf from c as a tls.Certificate and as a credential bundle (key, then chain),
// the layout kubelet writes for a podCertificate volume.
func (c *ca) issue(t *testing.T, o leafOpts) (tls.Certificate, []byte) {
	t.Helper()
	key := newKey(t)
	if o.notBefore.IsZero() {
		o.notBefore = t0.Add(-time.Minute)
	}
	if o.notAfter.IsZero() {
		o.notAfter = t0.Add(24 * time.Hour)
	}
	tmpl := &x509.Certificate{
		SerialNumber: nextSerial(),
		DNSNames:     o.dns,
		NotBefore:    o.notBefore,
		NotAfter:     o.notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
	}
	for _, s := range o.uris {
		u, err := url.Parse(s)
		if err != nil {
			t.Fatal(err)
		}
		tmpl.URIs = append(tmpl.URIs, u)
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, c.cert, &key.PublicKey, c.key)
	if err != nil {
		t.Fatal(err)
	}
	kder, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	bundle := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kder})
	bundle = append(bundle, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})...)
	bundle = append(bundle, c.pem...)
	leaf, _ := x509.ParseCertificate(der)
	return tls.Certificate{Certificate: [][]byte{der, c.cert.Raw}, PrivateKey: key, Leaf: leaf}, bundle
}

// router is a TLS 1.3 gRPC harness server that, like v0.6.0's envoy router, requires a client
// certificate from clientCA and admits only allowlisted SPIFFE IDs.
type router struct {
	addr string
	mu   sync.Mutex
	seen []*x509.Certificate
}

func startRouter(t *testing.T, serverCert tls.Certificate, clientCA *ca, allowed string) *router {
	t.Helper()
	pool := x509.NewCertPool()
	pool.AddCert(clientCA.cert)
	r := &router{}
	cfg := &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{serverCert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    pool,
		VerifyPeerCertificate: func(_ [][]byte, chains [][]*x509.Certificate) error {
			leaf := chains[0][0]
			r.mu.Lock()
			r.seen = append(r.seen, leaf)
			r.mu.Unlock()
			for _, u := range leaf.URIs {
				if u.String() == allowed {
					return nil
				}
			}
			return errors.New("SPIFFE ID not allowlisted")
		},
	}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer(grpc.Creds(credentials.NewTLS(cfg)))
	v1.RegisterHarnessServer(srv, harnesswire.NewServer(echoagent.Harness{}))
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	r.addr = lis.Addr().String()
	return r
}

func (r *router) lastSeen() *x509.Certificate {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.seen) == 0 {
		return nil
	}
	return r.seen[len(r.seen)-1]
}

type pki struct {
	clientCA, serverCA *ca
	serverCert         tls.Certificate
	dir                string
	certFile, caFile   string
}

func newPKI(t *testing.T) *pki {
	t.Helper()
	p := &pki{clientCA: newCA(t, "podidentity"), serverCA: newCA(t, "servicedns"), dir: t.TempDir()}
	p.serverCert, _ = p.serverCA.issue(t, leafOpts{dns: []string{routerName}})
	p.certFile = filepath.Join(p.dir, "credential-bundle.pem")
	p.caFile = filepath.Join(p.dir, "router-ca.pem")
	_, bundle := p.clientCA.issue(t, leafOpts{uris: []string{clientID}})
	write(t, p.certFile, bundle)
	write(t, p.caFile, p.serverCA.pem)
	return p
}

// write replaces path the way kubelet updates a projected volume: never a partly written file.
func write(t *testing.T, path string, b []byte) {
	t.Helper()
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
}

func (p *pki) load(t *testing.T, now func() time.Time) *routertls.Credentials {
	t.Helper()
	c, err := routertls.Load(routertls.Config{CertFile: p.certFile, CAFile: p.caFile, Now: now})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return c
}

// describe calls Describe through the router address with the routing metadata a substrate
// incarnation carries.
func describe(t *testing.T, creds *routertls.Credentials, addr string) error {
	t.Helper()
	har, closeHarness, err := routertls.Dialer(creds)(api.Incarnation{
		Address:      addr,
		CallMetadata: map[string]string{"ate-target-actor": "space/s1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = closeHarness() }()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err = har.Describe(ctx)
	return err
}

func TestLoadRefuses(t *testing.T) {
	p := newPKI(t)
	_, noKey := p.clientCA.issue(t, leafOpts{uris: []string{clientID}})
	noKey = noKey[strings.Index(string(noKey), "-----BEGIN CERTIFICATE"):]
	_, expired := p.clientCA.issue(t, leafOpts{uris: []string{clientID}, notBefore: t0.Add(-2 * time.Hour), notAfter: t0.Add(-time.Hour)})
	_, future := p.clientCA.issue(t, leafOpts{uris: []string{clientID}, notBefore: t0.Add(time.Hour)})
	_, noURI := p.clientCA.issue(t, leafOpts{dns: []string{"agentsessions.sessions.svc"}})
	_, twoURIs := p.clientCA.issue(t, leafOpts{uris: []string{clientID, "spiffe://cluster.local/ns/x/sa/y"}})
	_, httpsURI := p.clientCA.issue(t, leafOpts{uris: []string{"https://example.com/agentsessions"}})

	file := func(name string, b []byte) string {
		path := filepath.Join(t.TempDir(), name)
		write(t, path, b)
		return path
	}
	cases := []struct {
		name string
		cfg  routertls.Config
		want string
	}{
		{"no cert file configured", routertls.Config{CAFile: p.caFile}, "router client certificate file not set"},
		{"no CA file configured", routertls.Config{CertFile: p.certFile}, "router CA file not set"},
		{"missing SVID", routertls.Config{CertFile: filepath.Join(p.dir, "absent.pem"), CAFile: p.caFile}, "no such file"},
		{"missing CA", routertls.Config{CertFile: p.certFile, CAFile: filepath.Join(p.dir, "absent.pem")}, "router CA: "},
		{"empty bundle", routertls.Config{CertFile: file("b.pem", nil), CAFile: p.caFile}, "router client certificate"},
		{"bundle without a private key", routertls.Config{CertFile: file("b.pem", noKey), CAFile: p.caFile}, "PRIVATE KEY"},
		{"empty CA", routertls.Config{CertFile: p.certFile, CAFile: file("ca.pem", nil)}, "no PEM certificates"},
		{"garbage CA", routertls.Config{CertFile: p.certFile, CAFile: file("ca.pem", []byte("not a pem"))}, "no PEM certificates"},
		{"expired leaf", routertls.Config{CertFile: file("b.pem", expired), CAFile: p.caFile}, "expired at"},
		{"leaf not yet valid", routertls.Config{CertFile: file("b.pem", future), CAFile: p.caFile}, "not valid until"},
		{"leaf without a SPIFFE ID", routertls.Config{CertFile: file("b.pem", noURI), CAFile: p.caFile}, "got 0"},
		{"leaf with two SPIFFE IDs", routertls.Config{CertFile: file("b.pem", twoURIs), CAFile: p.caFile}, "got 2"},
		{"leaf with a non-SPIFFE URI only", routertls.Config{CertFile: file("b.pem", httpsURI), CAFile: p.caFile}, "got 0"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := routertls.Load(tc.cfg)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Load() error = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

func TestHandshakeWithAnAllowlistedSVID(t *testing.T) {
	p := newPKI(t)
	r := startRouter(t, p.serverCert, p.clientCA, clientID)
	creds := p.load(t, nil)
	if got := creds.SPIFFEID(); got != clientID {
		t.Fatalf("SPIFFEID() = %q, want %q", got, clientID)
	}
	if err := describe(t, creds, r.addr); err != nil {
		t.Fatalf("Describe through the router: %v", err)
	}
	if leaf := r.lastSeen(); leaf == nil || leaf.URIs[0].String() != clientID {
		t.Fatalf("router saw client %v, want %s", leaf, clientID)
	}
}

// The dial address is an IP literal, so a handshake that took its name from the authority would
// fail; the configured name is what is checked.
func TestHandshakeChecksTheConfiguredServerNameNotTheDialAddress(t *testing.T) {
	p := newPKI(t)
	r := startRouter(t, p.serverCert, p.clientCA, clientID)
	if !strings.HasPrefix(r.addr, "127.0.0.1:") {
		t.Fatalf("router address %s is not an IP literal", r.addr)
	}
	if err := describe(t, p.load(t, nil), r.addr); err != nil {
		t.Fatalf("Describe: %v", err)
	}
}

func TestHandshakeRefuses(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, p *pki) (addr string, creds *routertls.Credentials)
		// want lists the accepted causes: a refusal by the router arrives as its TLS alert, or as a
		// reset when the client's first write races the alert.
		want []string
	}{
		{
			name: "wrong client SPIFFE ID (not on the router's allowlist)",
			setup: func(t *testing.T, p *pki) (string, *routertls.Credentials) {
				r := startRouter(t, p.serverCert, p.clientCA, "spiffe://cluster.local/ns/other/sa/other")
				return r.addr, p.load(t, nil)
			},
			// Under TLS 1.3 the client finishes its handshake before the router checks its
			// certificate, so the refusal arrives after it, on the connection.
			want: []string{"bad certificate", "connection reset"},
		},
		{
			name: "client certificate from another CA",
			setup: func(t *testing.T, p *pki) (string, *routertls.Credentials) {
				r := startRouter(t, p.serverCert, newCA(t, "other podidentity"), clientID)
				return r.addr, p.load(t, nil)
			},
			// Go's client sends no certificate when the router's CertificateRequest names other CAs.
			want: []string{"certificate required", "unknown certificate authority", "bad certificate", "connection reset"},
		},
		{
			name: "router certificate for another name",
			setup: func(t *testing.T, p *pki) (string, *routertls.Credentials) {
				wrong, _ := p.serverCA.issue(t, leafOpts{dns: []string{"api.ate-system.svc"}})
				r := startRouter(t, wrong, p.clientCA, clientID)
				return r.addr, p.load(t, nil)
			},
			want: []string{"not " + routerName},
		},
		{
			name: "router certificate from another CA",
			setup: func(t *testing.T, p *pki) (string, *routertls.Credentials) {
				other, _ := newCA(t, "impostor").issue(t, leafOpts{dns: []string{routerName}})
				r := startRouter(t, other, p.clientCA, clientID)
				return r.addr, p.load(t, nil)
			},
			want: []string{"unknown authority"},
		},
		{
			name: "client certificate expired after Load",
			setup: func(t *testing.T, p *pki) (string, *routertls.Credentials) {
				r := startRouter(t, p.serverCert, p.clientCA, clientID)
				now := t0
				creds := p.load(t, func() time.Time { return now })
				now = t0.Add(25 * time.Hour)
				return r.addr, creds
			},
			want: []string{"router client certificate expired"},
		},
		{
			name: "SVID removed after Load",
			setup: func(t *testing.T, p *pki) (string, *routertls.Credentials) {
				r := startRouter(t, p.serverCert, p.clientCA, clientID)
				creds := p.load(t, nil)
				if err := os.Remove(p.certFile); err != nil {
					t.Fatal(err)
				}
				return r.addr, creds
			},
			want: []string{"router client certificate unreadable"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := newPKI(t)
			addr, creds := tc.setup(t, p)
			err := describe(t, creds, addr)
			if status.Code(err) != codes.Unavailable || !slices.ContainsFunc(tc.want, func(w string) bool { return strings.Contains(err.Error(), w) }) {
				t.Fatalf("Describe error = %v, want UNAVAILABLE containing one of %q", err, tc.want)
			}
		})
	}
}

// A handshake failure reaches session clients as the RPC error, so a refusal of the mounted files
// carries fixed text; the paths and OS error go to the logger.
func TestHandshakeHidesCredentialPathsFromTheCaller(t *testing.T) {
	cases := []struct {
		name, want string
		mutate     func(t *testing.T, p *pki)
		logged     func(p *pki) string
	}{
		{"SVID removed", "router client certificate unreadable", func(t *testing.T, p *pki) { remove(t, p.certFile) }, func(p *pki) string { return p.certFile }},
		{"CA removed", "router CA unreadable", func(t *testing.T, p *pki) { remove(t, p.caFile) }, func(p *pki) string { return p.caFile }},
		{"SVID replaced with garbage", "router client certificate invalid", func(t *testing.T, p *pki) { write(t, p.certFile, []byte("garbage")) }, func(p *pki) string { return p.certFile }},
		{"CA replaced with garbage", "router CA has no certificates", func(t *testing.T, p *pki) { write(t, p.caFile, []byte("garbage")) }, func(p *pki) string { return p.caFile }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := newPKI(t)
			r := startRouter(t, p.serverCert, p.clientCA, clientID)
			var logs bytes.Buffer
			creds := p.load(t, nil).WithLogger(slog.New(slog.NewTextHandler(&logs, nil)))
			tc.mutate(t, p)
			err := describe(t, creds, r.addr)
			if status.Code(err) != codes.Unavailable || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Describe error = %v, want UNAVAILABLE containing %q", err, tc.want)
			}
			if dir := filepath.Dir(p.certFile); strings.Contains(err.Error(), dir) {
				t.Fatalf("Describe error %q names the credential directory %s", err, dir)
			}
			if path := tc.logged(p); !strings.Contains(logs.String(), path) {
				t.Fatalf("log %q does not name %s", logs.String(), path)
			}
		})
	}
}

func remove(t *testing.T, path string) {
	t.Helper()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
}

// kubelet rotates the client certificate (about every 23.5h) and the trust bundle in place. The
// next handshake must use both without a restart.
func TestHandshakePicksUpRotatedFiles(t *testing.T) {
	p := newPKI(t)
	creds := p.load(t, nil)
	r1 := startRouter(t, p.serverCert, p.clientCA, clientID)
	if err := describe(t, creds, r1.addr); err != nil {
		t.Fatalf("Describe before rotation: %v", err)
	}
	first := r1.lastSeen().SerialNumber

	newCert, bundle := p.clientCA.issue(t, leafOpts{uris: []string{clientID}})
	write(t, p.certFile, bundle)
	if err := describe(t, creds, r1.addr); err != nil {
		t.Fatalf("Describe after client certificate rotation: %v", err)
	}
	if got := r1.lastSeen().SerialNumber; got.Cmp(newCert.Leaf.SerialNumber) != 0 || got.Cmp(first) == 0 {
		t.Fatalf("router saw client serial %v after rotation, want the new %v", got, newCert.Leaf.SerialNumber)
	}

	// A router serving from a new servicedns CA is refused until the bundle carries that CA.
	rotatedCA := newCA(t, "servicedns rotated")
	rotatedServer, _ := rotatedCA.issue(t, leafOpts{dns: []string{routerName}})
	r2 := startRouter(t, rotatedServer, p.clientCA, clientID)
	if err := describe(t, creds, r2.addr); err == nil || !strings.Contains(err.Error(), "unknown authority") {
		t.Fatalf("Describe to a router on a CA not yet in the bundle: %v, want unknown authority", err)
	}
	write(t, p.caFile, append(append([]byte{}, p.serverCA.pem...), rotatedCA.pem...))
	if err := describe(t, creds, r2.addr); err != nil {
		t.Fatalf("Describe after the trust bundle gained the new CA: %v", err)
	}
}

func TestClientOnly(t *testing.T) {
	creds := newPKI(t).load(t, nil)
	if err := creds.OverrideServerName("evil.example"); err == nil {
		t.Fatal("OverrideServerName succeeded, want a refusal")
	}
	if _, _, err := creds.ServerHandshake(nil); err == nil {
		t.Fatal("ServerHandshake succeeded, want a refusal")
	}
	clone, ok := creds.Clone().(*routertls.Credentials)
	if !ok || clone == creds || clone.SPIFFEID() != clientID || clone.ServerName() != routerName {
		t.Fatalf("Clone() = %#v, want a distinct copy for %s", clone, clientID)
	}
	if got := creds.Info().SecurityProtocol; got != "tls" {
		t.Fatalf("Info().SecurityProtocol = %q, want tls", got)
	}
}
