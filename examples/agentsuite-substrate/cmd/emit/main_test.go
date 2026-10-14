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
