package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"slices"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/backoff"
	"google.golang.org/grpc/credentials"

	"github.com/agent-primitives/sessions/api"
	"github.com/agent-primitives/sessions/internal/agentsessionsd"
	"github.com/agent-primitives/sessions/placement"
	"github.com/agent-primitives/sessions/runtime/substrate"
)

// controlFunc turns a connection to the agent-substrate ate-api into the substrate backend's
// ControlClient. main passes newControl, the ate-api adapter; tests pass a fake.
type controlFunc func(grpc.ClientConnInterface) substrate.ControlClient

// substrateHarness is one -substrate-harness entry: the ActorTemplate whose image is the harness,
// and the resumability tier the operator declares for it. The tier must be known before any actor
// exists (placement gates on it before Create), so it is configuration, as it is for the
// conformance suite, rather than asked of a live harness.
type substrateHarness struct {
	template substrate.ObjectRef
	memory   bool
}

// substrateHarnesses holds the -substrate-harness name=atespace/template[,memory] flag. It takes
// one entry: runtime/substrate names a session's actor by session UID in the one
// -substrate-atespace, so a second substrate harness would attach to the first one's actor (see
// Set).
type substrateHarnesses map[string]substrateHarness

func (s substrateHarnesses) String() string {
	return strings.Join(slices.Sorted(maps.Keys(s)), ",")
}

func (s substrateHarnesses) Set(value string) error {
	name, ref, ok := strings.Cut(value, "=")
	name, ref = strings.TrimSpace(name), strings.TrimSpace(ref)
	if !ok || name == "" || ref == "" {
		return fmt.Errorf("want name=atespace/template or name=atespace/template,memory, got %q", value)
	}
	if len(s) > 0 {
		// A turn an ExecRequest.harness override sent to the second harness would run on the first
		// one's actor and template, and the cold-create guard would count it on the wrong harness.
		// Refused until actors are named per harness (docs/substrate-conformance.md, "Substrate
		// placement binding").
		return fmt.Errorf("given more than once (already %q): only one substrate harness is supported, because substrate harnesses would share one actor per session", s.String())
	}
	ref, tier, hasTier := strings.Cut(ref, ",")
	h := substrateHarness{}
	if hasTier {
		if tier != "memory" {
			return fmt.Errorf("harness %q: unknown option %q; the only option is memory (REQUIRES_MEMORY_SNAPSHOT)", name, tier)
		}
		h.memory = true
	}
	atespace, template, ok := strings.Cut(ref, "/")
	if !ok || !isShortName(atespace) || !isShortName(template) {
		return fmt.Errorf("harness %q: template %q is not atespace/template, each a lowercase DNS label", name, ref)
	}
	h.template = substrate.ObjectRef{Atespace: atespace, Name: template}
	s[name] = h
	return nil
}

// isShortName accepts a lowercase RFC 1123 DNS label, substrate's k8s-short-name format for
// atespace, template and actor names.
func isShortName(s string) bool {
	if s == "" || len(s) > 63 || s[0] == '-' || s[len(s)-1] == '-' {
		return false
	}
	for _, r := range s {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
			return false
		}
	}
	return true
}

// substrateFlags is this build's agentsessionsd.HarnessBackends: the -substrate-harness, -ateapi-*
// and -atenet-router flags, and the ate-api connection they configure. The neutral agentsessionsd
// does not register them, so its -help does not offer a backend it cannot serve.
type substrateFlags struct {
	newControl controlFunc
	// fs is the flag set RegisterFlags added the flags to; Harnesses reads which of them were set.
	fs *flag.FlagSet

	harnesses      substrateHarnesses
	atespace       *string
	ateapi         *string
	serverName     *string
	caFile         *string
	insecureVerify *bool
	tokenFile      *string
	router         *string
	// only names the flags RegisterFlags adds besides -substrate-harness. Harnesses refuses them
	// without a -substrate-harness. It is derived at registration, so a flag added later is
	// covered without a second list to keep in sync.
	only []string
}

var _ agentsessionsd.HarnessBackends = (*substrateFlags)(nil)

func newSubstrateFlags(newControl controlFunc) *substrateFlags {
	return &substrateFlags{newControl: newControl, harnesses: substrateHarnesses{}}
}

// RegisterFlags adds the substrate flags to fs.
func (f *substrateFlags) RegisterFlags(fs *flag.FlagSet) {
	f.fs = fs
	before := map[string]bool{}
	fs.VisitAll(func(fl *flag.Flag) { before[fl.Name] = true })
	defer fs.VisitAll(func(fl *flag.Flag) {
		if !before[fl.Name] && fl.Name != "substrate-harness" {
			f.only = append(f.only, fl.Name)
		}
	})
	fs.Var(f.harnesses, "substrate-harness", "run a harness as an agent-substrate actor per session, as name=atespace/template; append ,memory for a REQUIRES_MEMORY_SNAPSHOT harness. The tier is not checked against the image's Describe: a stateful image declared without ,memory loses its RAM state on fork. Give it at most once")
	f.atespace = fs.String("substrate-atespace", "", "atespace the session actors are created in; required with -substrate-harness. Must not change while sessions exist: their actors stay in the old atespace, and a REQUIRES_MEMORY_SNAPSHOT session with a started turn is then refused")
	f.ateapi = fs.String("ateapi-addr", "api.ate-system.svc:443", "address of the substrate ate-api Control service (TLS)")
	f.serverName = fs.String("ateapi-server-name", "", "TLS server name for the ate-api; empty uses the host of -ateapi-addr")
	f.caFile = fs.String("ateapi-ca-file", "", "PEM CA bundle that signs the ate-api serving certificate; empty uses the system roots")
	f.insecureVerify = fs.Bool("ateapi-insecure-skip-verify", false, "do not verify the ate-api serving certificate, so a server impersonating the ate-api receives the -ateapi-token-file bearer token; for a dev cluster only")
	f.tokenFile = fs.String("ateapi-token-file", "", "file holding the bearer token for the ate-api, re-read on every call so a rotated projected token is picked up")
	f.router = fs.String("atenet-router", substrate.DefaultRouterAddress, "host:port of the atenet-router that reaches session actors over h2c")
}

// Harnesses runs every substrate flag check that needs no side effect, so agentsessionsd refuses a
// misconfigured start before it dials anything or opens the journal, and returns the
// -substrate-harness name. agentsessionsd itself checks that name against -model and its other
// harness names.
func (f *substrateFlags) Harnesses() ([]string, error) {
	if len(f.harnesses) == 0 {
		// Every other substrate flag only configures -substrate-harness entries. Given alone, it
		// would be ignored, which hides a dropped -substrate-harness from the operator.
		var set []string
		f.fs.Visit(func(fl *flag.Flag) {
			if slices.Contains(f.only, fl.Name) {
				set = append(set, "-"+fl.Name)
			}
		})
		if len(set) > 0 {
			return nil, fmt.Errorf("%s given without -substrate-harness: these flags only configure substrate harnesses", strings.Join(set, ", "))
		}
		return nil, nil
	}
	if !isShortName(*f.atespace) {
		return nil, fmt.Errorf("-substrate-atespace %q: want the atespace session actors are created in, a lowercase DNS label", *f.atespace)
	}
	if err := agentsessionsd.CheckDialAddress(*f.ateapi); err != nil {
		return nil, fmt.Errorf("-ateapi-addr: %w", err)
	}
	// The router is dialed the way placement dials every incarnation, over cleartext h2c.
	if err := agentsessionsd.CheckDialAddress(*f.router); err != nil {
		return nil, fmt.Errorf("-atenet-router: %w", err)
	}
	if *f.caFile != "" && *f.insecureVerify {
		return nil, errors.New("-ateapi-ca-file and -ateapi-insecure-skip-verify are mutually exclusive")
	}
	if *f.tokenFile != "" {
		// Fail at startup rather than on the first session: a projected token is mounted before the
		// container starts, so a missing or empty file is a deployment mistake.
		if _, err := readToken(*f.tokenFile); err != nil {
			return nil, fmt.Errorf("-ateapi-token-file: %w", err)
		}
	}
	return slices.Sorted(maps.Keys(f.harnesses)), nil
}

// ateapiReconnectBackoff bounds the wait between reconnect attempts to the ate-api, for the reason
// runtime/remote bounds its Describe connection: gRPC's default grows to 120s, and every Create,
// Resume and Suspend goes over this one connection, so after an ate-api restart sessions would keep
// failing UNAVAILABLE for up to two minutes after it was back.
var ateapiReconnectBackoff = grpc.ConnectParams{
	Backoff: backoff.Config{
		BaseDelay:  100 * time.Millisecond,
		Multiplier: backoff.DefaultConfig.Multiplier,
		Jitter:     backoff.DefaultConfig.Jitter,
		MaxDelay:   time.Second,
	},
	// gRPC's default; WithConnectParams would otherwise set it to zero.
	MinConnectTimeout: 20 * time.Second,
}

// Backends dials the ate-api once and builds the substrate backend for the -substrate-harness. The
// returned close function releases the connection. grpc.NewClient does not connect, so nothing is
// sent before the first session is placed.
func (f *substrateFlags) Backends(logger *slog.Logger) (map[string]placement.Backend, func(), error) {
	tlsConfig := &tls.Config{
		MinVersion:         tls.VersionTLS12,
		ServerName:         *f.serverName,
		InsecureSkipVerify: *f.insecureVerify, //nolint:gosec // opt-in for a dev cluster; the bearer token is then sent to whatever server answers
	}
	if *f.caFile != "" {
		pem, err := os.ReadFile(*f.caFile)
		if err != nil {
			return nil, nil, fmt.Errorf("-ateapi-ca-file: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, nil, fmt.Errorf("-ateapi-ca-file %s: no PEM certificates", *f.caFile)
		}
		tlsConfig.RootCAs = pool
	}
	dialOpts := []grpc.DialOption{
		grpc.WithTransportCredentials(credentials.NewTLS(tlsConfig)),
		grpc.WithConnectParams(ateapiReconnectBackoff),
	}
	if *f.tokenFile != "" {
		dialOpts = append(dialOpts, grpc.WithPerRPCCredentials(fileTokenCreds{path: *f.tokenFile}))
	}
	conn, err := grpc.NewClient(*f.ateapi, dialOpts...)
	if err != nil {
		return nil, nil, fmt.Errorf("dial ate-api %s: %w", *f.ateapi, err)
	}
	ctl := f.newControl(conn)
	backends := make(map[string]placement.Backend, len(f.harnesses))
	for name, h := range f.harnesses {
		// The declared tier is trusted as given; nothing compares it with the image's Describe, and
		// the atespace and template are not recorded per session. docs/substrate-conformance.md
		// ("Substrate placement binding") tracks both.
		resumability := api.ResumabilityStatelessReplay
		if h.memory {
			resumability = api.ResumabilityRequiresMemorySnapshot
		}
		desc := api.Descriptor{ID: name, Capabilities: api.Capabilities{Resumability: resumability}}
		backends[name] = substrate.New(ctl, *f.atespace, h.template, desc,
			substrate.WithLogger(logger), substrate.WithRouter(*f.router))
	}
	f.logConfig(logger)
	return backends, func() { _ = conn.Close() }, nil
}

// logConfig records where substrate sessions go, without the token.
func (f *substrateFlags) logConfig(logger *slog.Logger) {
	if logger == nil {
		return
	}
	for _, name := range slices.Sorted(maps.Keys(f.harnesses)) {
		h := f.harnesses[name]
		logger.Info("substrate harness",
			"harness", name,
			"template", h.template.Atespace+"/"+h.template.Name,
			"memory_snapshot", h.memory,
			"actor_atespace", *f.atespace,
			"ateapi", *f.ateapi,
			"atenet_router", *f.router,
			"ateapi_token_file", *f.tokenFile,
			"ateapi_insecure_skip_verify", *f.insecureVerify,
		)
	}
}

// fileTokenCreds presents a token file as a per-call bearer token. It re-reads the file on every
// call, so a rotated Kubernetes projected ServiceAccount token is picked up without a restart, and
// it requires transport security so the token never crosses a cleartext connection.
type fileTokenCreds struct{ path string }

func (c fileTokenCreds) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	tok, err := readToken(c.path)
	if err != nil {
		return nil, err
	}
	return map[string]string{"authorization": "Bearer " + tok}, nil
}

func (fileTokenCreds) RequireTransportSecurity() bool { return true }

func readToken(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read token file: %w", err)
	}
	tok := strings.TrimSpace(string(b))
	if tok == "" {
		return "", fmt.Errorf("token file %s is empty", path)
	}
	return tok, nil
}
