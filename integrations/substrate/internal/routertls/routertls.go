// Package routertls is the mTLS client to substrate's atenet-router. From substrate v0.6.0 the
// default (envoy) router serves only TLS and admits a client whose certificate chains to the pod
// identity CA and carries a SPIFFE ID on its --ingress-allowed-spiffe-ids allowlist.
//
// The client identity is a podCertificate projected volume (signer
// podidentity.podcert.ate.dev/identity), and the router's CA is a clusterTrustBundle projected
// volume (signer servicedns.podcert.ate.dev/identity), as substrate's router README documents for
// in-cluster clients. kubelet rotates both files; Credentials re-reads them on every handshake, so
// a rotated certificate or CA is used without a restart.
//
// The router is verified by DNS name, not SPIFFE ID: its servicedns certificate carries only DNS
// SANs. Any pod in ate-system that the atenet-router Service selects can present that certificate
// (docs/security.md).
package routertls

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"sync"
	"time"

	"google.golang.org/grpc/credentials"

	"github.com/agent-primitives/sessions/api"
	"github.com/agent-primitives/sessions/internal/harnessdial"
	"github.com/agent-primitives/sessions/placement"
)

// DefaultServerName is the DNS name on the router's servicedns serving certificate in a stock
// install: the atenet-router Service in ate-system. The servicedns signer issues only
// <service>.<namespace>.svc names, so it is not derived from the dial address.
const DefaultServerName = "atenet-router.ate-system.svc"

// Config names the mounted files and the router's expected name.
type Config struct {
	// CertFile is the podCertificate credential bundle: the private key and the certificate chain
	// in one PEM file, leaf first.
	CertFile string
	// CAFile holds the PEM roots the router's serving certificate must chain to.
	CAFile string
	// ServerName is the DNS name the router's certificate must carry; empty is DefaultServerName.
	ServerName string
	// Now is the clock the leaf's validity is checked against; nil is time.Now.
	Now func() time.Time
	// Logger records why a handshake could not use the files; nil is slog.Default.
	Logger *slog.Logger
}

// Credentials is a client-only grpc credentials.TransportCredentials for the router.
type Credentials struct {
	cfg Config

	mu    sync.Mutex
	files files
}

// files is the parsed state of the last bundle and CA read. Handshakes reparse only when the bytes
// change, which is on rotation.
type files struct {
	certPEM, caPEM []byte
	cert           tls.Certificate
	id             string
	roots          *x509.CertPool
}

var _ credentials.TransportCredentials = (*Credentials)(nil)

// Load reads and checks both files. It refuses a missing or unreadable file, a bundle with no
// private key or a leaf without exactly one spiffe:// URI SAN, a leaf outside its validity window,
// and a CA file with no certificate.
func Load(cfg Config) (*Credentials, error) {
	if cfg.CertFile == "" {
		return nil, errors.New("router client certificate file not set")
	}
	if cfg.CAFile == "" {
		return nil, errors.New("router CA file not set")
	}
	if cfg.ServerName == "" {
		cfg.ServerName = DefaultServerName
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	c := &Credentials{cfg: cfg}
	if _, err := c.config(); err != nil {
		return nil, err
	}
	return c, nil
}

// SPIFFEID is the client's own SPIFFE ID from the last certificate read: the ID the router's
// allowlist must name.
func (c *Credentials) SPIFFEID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.files.id
}

// ServerName is the DNS name the router's certificate is checked against.
func (c *Credentials) ServerName() string { return c.cfg.ServerName }

// TLSConfig returns the TLS config for one connection to the router from the current files, as
// ClientHandshake uses it. The caller sets NextProtos.
func (c *Credentials) TLSConfig() (*tls.Config, error) { return c.config() }

// WithLogger returns credentials for the same files and server name that log handshake-time
// credential failures to logger.
func (c *Credentials) WithLogger(logger *slog.Logger) *Credentials {
	c.mu.Lock()
	defer c.mu.Unlock()
	cfg := c.cfg
	if logger != nil {
		cfg.Logger = logger
	}
	return &Credentials{cfg: cfg, files: c.files}
}

// credError is a refusal of the mounted files. Error carries the paths and OS error for Load and
// the log; public is fixed text a handshake failure may carry to the RPC caller.
type credError struct {
	public string
	err    error
}

func (e *credError) Error() string { return e.err.Error() }
func (e *credError) Unwrap() error { return e.err }

func refuse(public string, err error) error { return &credError{public: public, err: err} }

// config re-reads both files, reparses whichever changed, rechecks the leaf's validity window and
// returns the TLS config for one handshake.
func (c *Credentials) config() (*tls.Config, error) {
	certPEM, err := os.ReadFile(c.cfg.CertFile)
	if err != nil {
		return nil, refuse("router client certificate unreadable", fmt.Errorf("router client certificate: %w", err))
	}
	caPEM, err := os.ReadFile(c.cfg.CAFile)
	if err != nil {
		return nil, refuse("router CA unreadable", fmt.Errorf("router CA: %w", err))
	}
	c.mu.Lock()
	f := c.files
	c.mu.Unlock()
	// The nil checks make the first read parse even an empty file: bytes.Equal treats empty and nil
	// alike, and a nil RootCAs would mean the system roots.
	if f.cert.Leaf == nil || !bytes.Equal(certPEM, f.certPEM) {
		cert, id, err := parseBundle(certPEM)
		if err != nil {
			return nil, refuse("router client certificate invalid", fmt.Errorf("router client certificate %s: %w", c.cfg.CertFile, err))
		}
		f.certPEM, f.cert, f.id = certPEM, cert, id
	}
	if f.roots == nil || !bytes.Equal(caPEM, f.caPEM) {
		roots := x509.NewCertPool()
		if !roots.AppendCertsFromPEM(caPEM) {
			return nil, refuse("router CA has no certificates", fmt.Errorf("router CA %s: no PEM certificates", c.cfg.CAFile))
		}
		f.caPEM, f.roots = caPEM, roots
	}
	c.mu.Lock()
	c.files = f
	c.mu.Unlock()

	leaf := f.cert.Leaf
	if now := c.cfg.Now(); now.After(leaf.NotAfter) {
		return nil, refuse("router client certificate expired", fmt.Errorf("router client certificate expired at %s", leaf.NotAfter.UTC().Format(time.RFC3339)))
	} else if now.Before(leaf.NotBefore) {
		return nil, refuse("router client certificate not yet valid", fmt.Errorf("router client certificate not valid until %s", leaf.NotBefore.UTC().Format(time.RFC3339)))
	}
	return &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{f.cert},
		RootCAs:      f.roots,
		ServerName:   c.cfg.ServerName,
	}, nil
}

// parseBundle parses a credential bundle into a certificate and its SPIFFE ID.
func parseBundle(pemBytes []byte) (tls.Certificate, string, error) {
	cert, err := tls.X509KeyPair(pemBytes, pemBytes)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	var ids []string
	for _, u := range cert.Leaf.URIs {
		if u.Scheme == "spiffe" {
			ids = append(ids, u.String())
		}
	}
	if len(ids) != 1 {
		return tls.Certificate{}, "", fmt.Errorf("want exactly one spiffe:// URI SAN on the leaf, got %d (is this a podidentity certificate?)", len(ids))
	}
	return cert, ids[0], nil
}

// ClientHandshake verifies the router's certificate chain against the CA file and its DNS name
// against ServerName, and presents the current client certificate. The dial authority does not
// change the name checked.
//
// A failure reaches session clients as the RPC's error, so a refusal of the files returns fixed
// text and the logger gets the paths and OS error.
func (c *Credentials) ClientHandshake(ctx context.Context, authority string, rawConn net.Conn) (net.Conn, credentials.AuthInfo, error) {
	cfg, err := c.config()
	if err != nil {
		public := "router client credentials unusable"
		if ce := (*credError)(nil); errors.As(err, &ce) {
			public = ce.public
		}
		c.cfg.Logger.WarnContext(ctx, "router client credentials unusable", "error", err)
		return nil, nil, errors.New(public)
	}
	return credentials.NewTLS(cfg).ClientHandshake(ctx, authority, rawConn)
}

// ServerHandshake refuses: these credentials are for the router client only.
func (c *Credentials) ServerHandshake(net.Conn) (net.Conn, credentials.AuthInfo, error) {
	return nil, nil, errors.New("routertls: client credentials cannot serve")
}

// Info reports TLS, as credentials.NewTLS does.
func (c *Credentials) Info() credentials.ProtocolInfo {
	return credentials.NewTLS(&tls.Config{ServerName: c.cfg.ServerName}).Info()
}

// Clone returns credentials for the same files and server name.
func (c *Credentials) Clone() credentials.TransportCredentials {
	c.mu.Lock()
	defer c.mu.Unlock()
	return &Credentials{cfg: c.cfg, files: c.files}
}

// OverrideServerName refuses: the router's name is fixed by configuration.
//
// Deprecated: part of the grpc interface; not used.
func (c *Credentials) OverrideServerName(string) error {
	return errors.New("routertls: the router server name is fixed by configuration")
}

// Dialer returns a placement.Dialer that reaches each incarnation's router address over these
// credentials, with the routing metadata placement.DefaultDial attaches.
func Dialer(creds *Credentials) placement.Dialer {
	return func(inc api.Incarnation) (api.Harness, func() error, error) {
		return harnessdial.Dial(inc, creds)
	}
}
