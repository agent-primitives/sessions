// Command check-model validates deployment model settings without contacting a provider.
// Configuration comes from the environment so URLs and credentials do not enter argv.
package main

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"

	"github.com/agent-primitives/sessions/internal/modelconfig"
)

var (
	headerName = regexp.MustCompile(`^[A-Za-z0-9-]+$`)
	apiVersion = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}(-preview)?$`)
)

func main() {
	cfg := modelconfig.Config{
		Model:      os.Getenv("MODEL_NAME"),
		BaseURL:    os.Getenv("MODEL_BASE_URL"),
		Path:       os.Getenv("MODEL_PATH"),
		AuthHeader: os.Getenv("MODEL_AUTH_HEADER"),
	}
	if os.Getenv("MODEL_API_KEY") != "" {
		cfg.APIKey = "set" // The constructor needs only credential presence, not its value.
	}
	if err := validate(cfg); err != nil {
		fmt.Fprintln(os.Stderr, "check-model:", err)
		os.Exit(1)
	}
}

func validate(cfg modelconfig.Config) error {
	base, err := url.Parse(cfg.BaseURL)
	if err != nil || (base.Scheme != "http" && base.Scheme != "https") || base.Hostname() == "" {
		return errors.New("MODEL_BASE_URL must be an http(s) URL with a host (value omitted)")
	}
	if base.User != nil || base.RawQuery != "" || base.ForceQuery || strings.Contains(cfg.BaseURL, "#") {
		return errors.New("MODEL_BASE_URL must not contain userinfo, a query, or a fragment; use MODEL_API_KEY for credentials (value omitted)")
	}
	path, err := url.Parse(cfg.Path)
	if err != nil || !strings.HasPrefix(cfg.Path, "/") || strings.HasPrefix(cfg.Path, "//") ||
		path.IsAbs() || path.Host != "" || strings.Contains(cfg.Path, "#") {
		return errors.New("MODEL_PATH must be an absolute path without an authority or fragment (value omitted)")
	}
	query, err := url.ParseQuery(path.RawQuery)
	if err != nil || path.ForceQuery {
		return errors.New("MODEL_PATH has an invalid query (value omitted)")
	}
	for key, values := range query {
		if key != "api-version" || len(values) != 1 || !apiVersion.MatchString(values[0]) {
			return errors.New("MODEL_PATH permits only one date-shaped api-version query parameter; use MODEL_API_KEY for credentials (value omitted)")
		}
	}
	if !headerName.MatchString(cfg.AuthHeader) {
		return errors.New("MODEL_AUTH_HEADER must contain only letters, digits, or hyphens (value omitted)")
	}
	if _, err := modelconfig.New(cfg); err != nil {
		return errors.New("model settings would prevent agentsessionsd from starting (values omitted)")
	}
	return nil
}
