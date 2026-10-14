package main

import (
	"strings"
	"testing"
)

// check-model refuses what agentsessionsd's model client refuses at startup (model/openai), which
// includes absolute URLs a scheme://host pattern accepts, and a scheme other than http(s).
func TestCheckModel(t *testing.T) {
	for _, tc := range []struct {
		name, baseURL, model string
		wantErr              string // "" accepts
	}{
		{name: "https", baseURL: "https://api.example.com/v1", model: "m"},
		{name: "uppercase scheme with port, query and fragment", baseURL: "HTTP://llm.example:8080/v1?api-version=2#x", model: "m"},
		{name: "bracketed IPv6", baseURL: "http://[::1]:8080/v1", model: "m"},
		{name: "userinfo", baseURL: "https://user@llm.example/v1", model: "m"},
		{name: "no scheme", baseURL: "api.example.com/v1", model: "m", wantErr: "is not absolute"},
		{name: "no host", baseURL: "https:///v1", model: "m", wantErr: "is not absolute"},
		{name: "only userinfo for a host", baseURL: "https://user@/v1", model: "m", wantErr: "is not absolute"},
		{name: "a port that is not a number", baseURL: "http://host:abc/v1", model: "m", wantErr: "invalid port"},
		{name: "a bad escape in the host", baseURL: "http://h%zz/v1", model: "m", wantErr: "invalid URL escape"},
		{name: "a bad escape in the path", baseURL: "https://llm.example/v%zz", model: "m", wantErr: "invalid URL escape"},
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
		})
	}
	if err := checkModel([]string{"-base-url", "https://a.example", "-model", "m", "extra"}); err == nil || !strings.Contains(err.Error(), "unexpected arguments") {
		t.Fatalf("extra argument: err=%v", err)
	}
}
