package modelconfig_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aramase/agentsessions/api"
	"github.com/aramase/agentsessions/internal/modelconfig"
)

func TestNewAppliesExplicitConfig(t *testing.T) {
	// Environment policy belongs to the host, not this constructor.
	t.Setenv("MODEL_API_KEY", "ignored-model-key")
	t.Setenv("OPENAI_API_KEY", "ignored-openai-key")
	const key = "test-key"
	for _, tt := range []struct {
		name       string
		authHeader string
		key        string
		wantBearer string
		wantAPIKey string
	}{
		{"bearer", "Authorization", key, "Bearer " + key, ""},
		{"case insensitive bearer", "authorization", key, "Bearer " + key, ""},
		{"custom header", "api-key", key, "", key},
		{"no key", "Authorization", "", "", ""},
		{"no header", "", key, "", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			type request struct {
				uri    string
				header http.Header
				model  string
				err    error
			}
			requests := make(chan request, 1)
			endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body struct {
					Model string `json:"model"`
				}
				err := json.NewDecoder(r.Body).Decode(&body)
				requests <- request{uri: r.RequestURI, header: r.Header.Clone(), model: body.Model, err: err}
				_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
			}))
			t.Cleanup(endpoint.Close)

			c, err := modelconfig.New(modelconfig.Config{
				Model:      "configured-model",
				BaseURL:    endpoint.URL + "/v1",
				Path:       "/deployment/completions?api-version=test",
				AuthHeader: tt.authHeader,
				APIKey:     tt.key,
			})
			if err != nil {
				t.Fatal(err)
			}
			_, err = c.Model(t.Context(), api.ModelRequest{
				Messages: []api.Message{*api.TextMessage("user", "hello")},
			})
			if err != nil {
				t.Fatal(err)
			}
			got := <-requests
			if got.err != nil {
				t.Fatal(got.err)
			}
			if got.uri != "/v1/deployment/completions?api-version=test" || got.model != "configured-model" {
				t.Fatalf("request URI = %q, model = %q", got.uri, got.model)
			}
			if got.header.Get("Authorization") != tt.wantBearer || got.header.Get("api-key") != tt.wantAPIKey {
				t.Fatal("credential header does not match explicit configuration")
			}
		})
	}
}

func TestNewRequiresModel(t *testing.T) {
	c, err := modelconfig.New(modelconfig.Config{})
	if err == nil || c != nil {
		t.Fatalf("client = %v, error = %v, want missing-model failure", c, err)
	}
}

// A model API key over plain http is refused unless the host is local or in-cluster; https, and
// http with no key, are unchanged. The refusal names the host and says to use https, and never
// repeats the key.
func TestNewRefusesAKeyOverRemotePlainHTTP(t *testing.T) {
	const key = "sk-secret-value"
	for _, tt := range []struct {
		name    string
		baseURL string
		key     string
		refuse  string // the host the refusal names; "" when allowed
	}{
		{name: "IPv4 loopback", baseURL: "http://127.0.0.1:8080/v1", key: key},
		{name: "another IPv4 loopback address", baseURL: "http://127.0.0.2/v1", key: key},
		{name: "IPv6 loopback", baseURL: "http://[::1]:8080/v1", key: key},
		{name: "localhost", baseURL: "http://localhost:11434/v1", key: key},
		{name: "localhost in capitals", baseURL: "HTTP://LocalHost/v1", key: key},
		{name: "service name", baseURL: "http://ollama.ns.svc/v1", key: key},
		{name: "service name with a port", baseURL: "http://ollama.ns.svc:11434/v1", key: key},
		{name: "cluster-local service name with a port", baseURL: "http://vllm.ns.svc.cluster.local:8000/v1", key: key},
		{name: "cluster-local service name in capitals", baseURL: "http://VLLM.NS.SVC.Cluster.Local/v1", key: key},
		{name: "remote host", baseURL: "http://example.com/v1", key: key, refuse: "example.com"},
		{name: "remote host with a port", baseURL: "http://example.com:8080/v1", key: key, refuse: "example.com"},
		{name: "IPv4 address that is not loopback", baseURL: "http://10.0.0.5:8000/v1", key: key, refuse: "10.0.0.5"},
		{name: "IPv6 address that is not loopback", baseURL: "http://[2001:db8::1]/v1", key: key, refuse: "2001:db8::1"},
		{name: "name that only contains svc", baseURL: "http://svc.example.com/v1", key: key, refuse: "svc.example.com"},
		{name: "name ending in svc without a dot", baseURL: "http://evilsvc/v1", key: key, refuse: "evilsvc"},
		{name: "localhost subdomain", baseURL: "http://localhost.example.com/v1", key: key, refuse: "localhost.example.com"},
		{name: "fully qualified name with a trailing dot", baseURL: "http://vllm.ns.svc.cluster.local./v1", key: key, refuse: "vllm.ns.svc.cluster.local."},
		{name: "https remote host", baseURL: "https://example.com/v1", key: key},
		{name: "https IP address", baseURL: "https://10.0.0.5/v1", key: key},
		{name: "https default base URL", baseURL: "", key: key},
		{name: "http remote host without a key", baseURL: "http://example.com/v1"},
		{name: "http IP address without a key", baseURL: "http://10.0.0.5/v1"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c, err := modelconfig.New(modelconfig.Config{Model: "m", BaseURL: tt.baseURL, AuthHeader: "api-key", APIKey: tt.key})
			if tt.refuse == "" {
				if err != nil || c == nil {
					t.Fatalf("New(%q) = %v, %v, want a client", tt.baseURL, c, err)
				}
				return
			}
			if err == nil || c != nil {
				t.Fatalf("New(%q) = %v, %v, want a refusal", tt.baseURL, c, err)
			}
			msg := err.Error()
			for _, want := range []string{`host "` + tt.refuse + `"`, "plain http", "use https"} {
				if !strings.Contains(msg, want) {
					t.Errorf("error %q does not contain %q", msg, want)
				}
			}
			if strings.Contains(msg, key) {
				t.Errorf("error %q repeats the key", msg)
			}
		})
	}
}
