// Package modelconfig constructs the OpenAI-compatible client shared by repository-owned hosts.
// Callers own flag and environment handling, logging, and any built-in model fallback.
package modelconfig

import (
	"fmt"
	"net"
	"net/url"
	"strings"

	"github.com/agent-primitives/sessions/model/openai"
)

// Config supplies explicit host-side model settings. Empty BaseURL and Path use openai's defaults.
type Config struct {
	Model   string
	BaseURL string
	Path    string
	// AuthHeader selects bearer authentication for Authorization (case-insensitive), or a raw
	// credential for any other header name. Empty AuthHeader or APIKey sends no credential.
	AuthHeader string
	APIKey     string
}

// New builds a client whose Model and StreamModel methods satisfy the controller's model seams.
// A model is required; this package neither reads the environment nor selects a fallback model.
//
// With an APIKey set, New refuses a plain-http BaseURL unless its host is a loopback address,
// localhost, or a Kubernetes service name, the last as a trusted-network exception (see
// checkKeyedHTTP): the key and every prompt would otherwise cross the network unencrypted.
func New(cfg Config) (*openai.Client, error) {
	if err := checkKeyedHTTP(cfg); err != nil {
		return nil, err
	}
	opts := []openai.Option{
		openai.WithModel(cfg.Model),
		openai.WithBaseURL(cfg.BaseURL),
		openai.WithPath(cfg.Path),
	}
	if cfg.APIKey != "" {
		if strings.EqualFold(cfg.AuthHeader, "Authorization") {
			opts = append(opts, openai.WithAPIKey(cfg.APIKey))
		} else {
			opts = append(opts, openai.WithHeader(cfg.AuthHeader, cfg.APIKey))
		}
	}
	return openai.New(opts...)
}

// checkKeyedHTTP refuses an http BaseURL when an APIKey is set, unless the host (case-insensitive,
// port ignored) is a loopback IP literal, localhost, or a name ending in .svc or .svc.cluster.local.
// https, and http with no key, are left alone. Any key counts, whatever AuthHeader says, so a
// configuration that might send one fails closed. A BaseURL that does not parse is left to
// openai.New. The error names the host only: the key is never printed, and the URL's other parts are
// the caller's to show.
//
// The service-name allowance is a trusted-network exception, not a check that the traffic stays
// local: it looks at the name only. Nothing here proves the name resolves to a Kubernetes service
// (resolver search domains or an external DNS zone can answer it), and a service's traffic still
// crosses the pod network in cleartext, readable by anything on that path. An operator who points a
// keyed client at such a name accepts that network as trusted.
func checkKeyedHTTP(cfg Config) error {
	if cfg.APIKey == "" || cfg.BaseURL == "" {
		return nil
	}
	u, err := url.Parse(cfg.BaseURL)
	if err != nil || u.Scheme != "http" { // url.Parse lowercases the scheme
		return nil
	}
	host := u.Hostname()
	if host == "" || plainHTTPAllowed(host) { // openai.New refuses a URL with no host
		return nil
	}
	return fmt.Errorf("modelconfig: the model base URL uses plain http to host %q while a model API key is set, "+
		"so the key and prompts would be sent unencrypted; use https (plain http with a key is allowed only to "+
		"a loopback address, localhost, or, as a trusted-network exception, a *.svc or *.svc.cluster.local name)", host)
}

// plainHTTPAllowed reports whether host is a loopback IP literal, localhost, or a name ending in .svc
// or .svc.cluster.local, compared case-insensitively. It is a name check (see checkKeyedHTTP). A
// trailing dot is not stripped, so a fully qualified "x.ns.svc.cluster.local." is refused, which
// fails closed.
func plainHTTPAllowed(host string) bool {
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	h := strings.ToLower(host)
	return h == "localhost" || strings.HasSuffix(h, ".svc") || strings.HasSuffix(h, ".svc.cluster.local")
}
