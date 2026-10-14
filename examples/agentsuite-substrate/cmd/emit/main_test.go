package main

import (
	"strings"
	"testing"
)

// check-model refuses what agentsessionsd's model client refuses at startup (model/openai), which
// includes absolute URLs a scheme://host pattern accepts, and a scheme other than http(s). It also
// refuses a URL with userinfo, a query or a fragment, which up.sh would print and render into the
// Deployment, and none of its errors repeats any part of the URL.
func TestCheckModel(t *testing.T) {
	const hidden = "the value is not shown"
	for _, tc := range []struct {
		name, baseURL, model string
		wantErr              string // "" accepts
	}{
		{name: "https", baseURL: "https://api.example.com/v1", model: "m"},
		{name: "uppercase scheme with port", baseURL: "HTTP://llm.example:8080/v1", model: "m"},
		{name: "bracketed IPv6", baseURL: "http://[::1]:8080/v1", model: "m"},
		{name: "userinfo", baseURL: "https://user:secret@llm.example/v1", model: "m", wantErr: "carries userinfo"},
		{name: "user only", baseURL: "https://secret@llm.example/v1", model: "m", wantErr: "carries userinfo"},
		{name: "only userinfo for a host", baseURL: "https://secret@/v1", model: "m", wantErr: "carries userinfo"},
		{name: "query", baseURL: "https://llm.example/v1?key=secret", model: "m", wantErr: "carries a query"},
		{name: "empty query", baseURL: "https://llm.example/v1?", model: "m", wantErr: "carries a query"},
		{name: "fragment", baseURL: "https://llm.example/v1#secret", model: "m", wantErr: "carries a fragment"},
		{name: "empty fragment", baseURL: "https://llm.example/v1#", model: "m", wantErr: "carries a fragment"},
		{name: "userinfo with no scheme", baseURL: "user:secret@llm.example/v1", model: "m", wantErr: "is not http or https"},
		{name: "userinfo read as a port", baseURL: "https://user:secret/x@llm.example/v1", model: "m", wantErr: "is not a valid URL"},
		{name: "no scheme", baseURL: "api.example.com/v1", model: "m", wantErr: "is not http or https"},
		{name: "no host", baseURL: "https:///v1", model: "m", wantErr: "has no host"},
		{name: "a port that is not a number", baseURL: "http://host:abc/v1", model: "m", wantErr: "is not a valid URL"},
		{name: "a bad escape in the host", baseURL: "http://h%zz/v1", model: "m", wantErr: "is not a valid URL"},
		{name: "a bad escape in the path", baseURL: "https://llm.example/v%zz", model: "m", wantErr: "is not a valid URL"},
		{name: "another scheme", baseURL: "ftp://llm.example/v1", model: "m", wantErr: "is not http or https"},
		{name: "no base URL", baseURL: "", model: "m", wantErr: "-base-url is required"},
		{name: "no model", baseURL: "https://api.example.com/v1", model: "", wantErr: "a default model is required"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := checkModel([]string{"-base-url", tc.baseURL, "-model", tc.model})
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("err=%v, want it accepted", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Fatalf("err=%v, want one containing %q", err, tc.wantErr)
			}
			if err != nil && tc.baseURL != "" && tc.model != "" {
				msg := err.Error()
				for _, leak := range []string{"secret", "llm.example", "api.example", "abc", "%zz", "/v1"} {
					if strings.Contains(msg, leak) {
						t.Errorf("err=%q repeats %q from the URL", msg, leak)
					}
				}
				if !strings.Contains(msg, hidden) {
					t.Errorf("err=%q does not say the value is hidden", msg)
				}
			}
		})
	}
	if err := checkModel([]string{"-base-url", "https://a.example", "-model", "m", "extra"}); err == nil || !strings.Contains(err.Error(), "unexpected arguments") {
		t.Fatalf("extra argument: err=%v", err)
	}
}

// check-model's -path may carry only a date-shaped api-version, the one query Azure OpenAI needs on a
// deployment path, since up.sh renders the path into the Deployment and the daemon logs it; and
// -auth-header must be a header name. No refusal repeats any part of the path.
func TestCheckModelPathAndAuthHeader(t *testing.T) {
	const azure = "https://res.openai.azure.com"
	for _, tc := range []struct {
		name, path, header string
		wantErr            string // "" accepts
	}{
		{name: "the default path", path: ""},
		{name: "an Azure deployment path", path: "/openai/deployments/dep/chat/completions?api-version=2024-10-21", header: "api-key"},
		{name: "a preview api-version", path: "/openai/deployments/dep/chat/completions?api-version=2025-04-01-preview", header: "api-key"},
		{name: "a path without a query", path: "/v1/chat/completions"},
		{name: "a key in the query", path: "/openai/deployments/dep/chat/completions?api-version=2024-10-21&api-key=secret", wantErr: "other than api-version"},
		{name: "only a key in the query", path: "/chat/completions?api-key=secret", wantErr: "other than api-version"},
		{name: "a key as the api-version", path: "/chat/completions?api-version=secret", wantErr: "is not one date"},
		{name: "two api-versions", path: "/chat/completions?api-version=2024-10-21&api-version=2024-10-22", wantErr: "is not one date"},
		{name: "an empty api-version", path: "/chat/completions?api-version=", wantErr: "is not one date"},
		{name: "an empty query", path: "/chat/completions?", wantErr: "empty query"},
		{name: "a query that does not parse", path: "/chat/completions?api-version=2024-10-21;secret", wantErr: "does not parse"},
		{name: "a fragment", path: "/chat/completions#secret", wantErr: "carries a fragment"},
		{name: "userinfo", path: "/user:secret@llm.example/chat", wantErr: "contains '@'"},
		{name: "an authority", path: "//secret.example/chat", wantErr: "single '/'"},
		{name: "a relative path", path: "secret/chat", wantErr: "single '/'"},
		{name: "a bad escape", path: "/secret%zz", wantErr: "not a valid URL path"},
		{name: "a trailing colon", path: "/openai/deployments/secret:", wantErr: "ends with ':'"},
		{name: "a header with a space", path: "", header: "api key", wantErr: "not an HTTP header name"},
		{name: "a header with a colon", path: "", header: "api-key:", wantErr: "not an HTTP header name"},
		{name: "x-api-key", path: "", header: "x-api-key"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := []string{"-base-url", azure, "-model", "dep", "-path", tc.path}
			if tc.header != "" {
				args = append(args, "-auth-header", tc.header)
			}
			err := checkModel(args)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("err=%v, want it accepted", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Fatalf("err=%v, want one containing %q", err, tc.wantErr)
			}
			if err != nil {
				for _, leak := range []string{"secret", "deployments", "llm.example", "%zz", "chat"} {
					if strings.Contains(err.Error(), leak) {
						t.Errorf("err=%q repeats %q from the path", err, leak)
					}
				}
			}
		})
	}
	if err := checkModel([]string{"-base-url", azure, "-model", "dep", "-auth-header", ""}); err == nil || !strings.Contains(err.Error(), "not an HTTP header name") {
		t.Fatalf("empty -auth-header: err=%v", err)
	}
}
