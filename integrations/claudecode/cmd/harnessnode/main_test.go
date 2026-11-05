package main

import (
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/agent-primitives/sessions/api"
	"github.com/agent-primitives/sessions/internal/harnessnode"
)

func TestNewHarness(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	valid := map[string]string{
		"HARNESS_CLAUDE_BIN":             exe,
		"HARNESS_CLAUDE_WORKDIR":         t.TempDir(),
		"HARNESS_CLAUDE_PERMISSION_MODE": "dontAsk",
		"HARNESS_MODEL":                  "test-model",
		"HARNESS_CLAUDE_MAX_TURNS":       "8",
		"HARNESS_CLAUDE_MAX_BUDGET_USD":  "2.5",
	}
	for _, tt := range []struct {
		name     string
		set      map[string]string
		settings harnessnode.Settings
		want     string // error substring; empty for a valid configuration
	}{
		{name: "valid"},
		{name: "optional values unset", set: map[string]string{"HARNESS_MODEL": "", "HARNESS_CLAUDE_MAX_TURNS": "", "HARNESS_CLAUDE_MAX_BUDGET_USD": ""}},
		{name: "receive limit", settings: harnessnode.Settings{MaxRecvMsgSize: 16 << 20}},
		{name: "missing binary", set: map[string]string{"HARNESS_CLAUDE_BIN": "claudecode-test-no-such-binary"}, want: "find claude binary"},
		{name: "missing workdir", set: map[string]string{"HARNESS_CLAUDE_WORKDIR": ""}, want: "WorkDir is required"},
		{name: "missing permission mode", set: map[string]string{"HARNESS_CLAUDE_PERMISSION_MODE": ""}, want: "PermissionMode is required"},
		{name: "max turns not a number", set: map[string]string{"HARNESS_CLAUDE_MAX_TURNS": "many"}, want: "HARNESS_CLAUDE_MAX_TURNS"},
		{name: "negative max turns", set: map[string]string{"HARNESS_CLAUDE_MAX_TURNS": "-1"}, want: "HARNESS_CLAUDE_MAX_TURNS"},
		{name: "budget not a number", set: map[string]string{"HARNESS_CLAUDE_MAX_BUDGET_USD": "lots"}, want: "HARNESS_CLAUDE_MAX_BUDGET_USD"},
		{name: "negative budget", set: map[string]string{"HARNESS_CLAUDE_MAX_BUDGET_USD": "-1"}, want: "HARNESS_CLAUDE_MAX_BUDGET_USD"},
		{name: "NaN budget", set: map[string]string{"HARNESS_CLAUDE_MAX_BUDGET_USD": "NaN"}, want: "HARNESS_CLAUDE_MAX_BUDGET_USD"},
		{name: "infinite budget", set: map[string]string{"HARNESS_CLAUDE_MAX_BUDGET_USD": "Inf"}, want: "HARNESS_CLAUDE_MAX_BUDGET_USD"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			for k, v := range valid {
				t.Setenv(k, v)
			}
			for k, v := range tt.set {
				t.Setenv(k, v)
			}
			harness, err := newHarness(tt.settings)
			if tt.want != "" {
				if err == nil || !strings.Contains(err.Error(), tt.want) {
					t.Fatalf("error = %v, want one containing %q", err, tt.want)
				}
				if harness != nil {
					t.Fatalf("invalid configuration returned harness %T", harness)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			desc, err := harness.Describe(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			var models []string
			if m := os.Getenv("HARNESS_MODEL"); m != "" {
				models = []string{m}
			}
			if desc.ID != "claudecode" || desc.Capabilities.Resumability != api.ResumabilityRequiresMemorySnapshot ||
				desc.Capabilities.ForkSafe || !reflect.DeepEqual(desc.Models, models) {
				t.Fatalf("descriptor = %+v", desc)
			}
		})
	}
}
