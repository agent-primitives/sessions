package main

import (
	"strings"
	"testing"

	"github.com/agent-primitives/sessions/internal/modelconfig"
)

func TestValidateModelRoutes(t *testing.T) {
	for _, tc := range []struct {
		name, base, path, header, want string
		key                            bool
	}{
		{"local", "http://ollama.example.svc:11434/v1", "/chat/completions", "Authorization", "", false},
		{"external", "https://api.example/v1", "/chat/completions", "Authorization", "", true},
		{"versioned custom route", "https://api.example", "/deployments/model/chat/completions?api-version=2025-04-01-preview", "api-key", "", true},
		{"not a URL", "not-a-url", "/chat/completions", "Authorization", "MODEL_BASE_URL", false},
		{"unsupported scheme", "ftp://api.example", "/chat/completions", "Authorization", "MODEL_BASE_URL", false},
		{"missing host", "https:///v1", "/chat/completions", "Authorization", "MODEL_BASE_URL", false},
		{"userinfo", "https://fixture-marker@api.example", "/chat/completions", "Authorization", "MODEL_BASE_URL", false},
		{"base query", "https://api.example?api-key=fixture-marker", "/chat/completions", "Authorization", "MODEL_BASE_URL", false},
		{"fragment", "https://api.example#fixture-marker", "/chat/completions", "Authorization", "MODEL_BASE_URL", false},
		{"remote keyed HTTP", "http://fixture-marker.example/v1", "/chat/completions", "Authorization", "prevent agentsessionsd from starting", true},
		{"path authority", "https://api.example", "//fixture-marker.example/chat", "Authorization", "MODEL_PATH", false},
		{"path credential", "https://api.example", "/chat?api-key=fixture-marker", "Authorization", "MODEL_PATH", false},
		{"duplicate version", "https://api.example", "/chat?api-version=2025-04-01&api-version=2025-04-01", "Authorization", "MODEL_PATH", false},
		{"invalid header", "https://api.example", "/chat/completions", "fixture-marker\n", "MODEL_AUTH_HEADER", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := modelconfig.Config{Model: "fixture-model", BaseURL: tc.base, Path: tc.path, AuthHeader: tc.header}
			if tc.key {
				cfg.APIKey = "set"
			}
			err := validate(cfg)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("valid route refused: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("validate = %v, want refusal mentioning %s", err, tc.want)
			}
			if strings.Contains(err.Error(), "fixture-marker") {
				t.Fatalf("refusal exposed configuration: %v", err)
			}
		})
	}
}
