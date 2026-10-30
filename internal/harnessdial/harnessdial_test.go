package harnessdial_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"slices"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"

	"github.com/agent-primitives/sessions/api"
	v1 "github.com/agent-primitives/sessions/api/genpb"
	"github.com/agent-primitives/sessions/harness/echoagent"
	"github.com/agent-primitives/sessions/harnesswire"
	"github.com/agent-primitives/sessions/internal/harnessdial"
)

// Dial with transport credentials other than plaintext keeps the routing metadata: the TLS router
// path must pick the incarnation's actor, not one the caller put on the context.
func TestDialWithTLSCredentialsCarriesTheIncarnationsMetadata(t *testing.T) {
	cert, roots := selfSigned(t, "harness.test")
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var seen [][]string
	srv := grpc.NewServer(grpc.Creds(credentials.NewServerTLSFromCert(&cert)),
		grpc.ChainUnaryInterceptor(func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
			md, _ := metadata.FromIncomingContext(ctx)
			mu.Lock()
			seen = append(seen, md.Get("ate-target-actor"))
			mu.Unlock()
			return handler(ctx, req)
		}))
	v1.RegisterHarnessServer(srv, harnesswire.NewServer(echoagent.Harness{}))
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	creds := credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots, ServerName: "harness.test"})
	har, closeHarness, err := harnessdial.Dial(api.Incarnation{
		Address:      lis.Addr().String(),
		CallMetadata: map[string]string{"ate-target-actor": "space/mine"},
	}, creds)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = closeHarness() }()
	ctx := metadata.AppendToOutgoingContext(context.Background(), "ate-target-actor", "space/other")
	if _, err := har.Describe(ctx); err != nil {
		t.Fatalf("Describe over TLS: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 1 || !slices.Equal(seen[0], []string{"space/mine"}) {
		t.Fatalf("Describe carried ate-target-actor=%v, want [[space/mine]]", seen)
	}
}

func selfSigned(t *testing.T, name string) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: name},
		DNSNames:     []string{name},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(leaf)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, roots
}
