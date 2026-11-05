package harnessnode

import (
	"cmp"
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/agent-primitives/sessions/api"
	"github.com/agent-primitives/sessions/harness/chatagent"
	"github.com/agent-primitives/sessions/harness/toolagent"
)

func TestSelectHarness(t *testing.T) {
	for _, tt := range []struct {
		name         string
		kind         string
		model        string
		id           string
		resumability api.Resumability
	}{
		{"default", "", "", "echo", api.ResumabilityStatelessReplay},
		{"echo", "echo", "ignored", "echo", api.ResumabilityStatelessReplay},
		{"counter", "counter", "", "counter", api.ResumabilityRequiresMemorySnapshot},
		{"existing fallback", "unknown", "", "echo", api.ResumabilityStatelessReplay},
		{"chat", "chat", "test-model", "chat", api.ResumabilityStatelessReplay},
		{"tool", "tool", "test-model", "tool", api.ResumabilityStatelessReplay},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("HARNESS_KIND", tt.kind)
			t.Setenv("HARNESS_MODEL", tt.model)
			harness, kind, err := selectHarness(options{}, Settings{})
			if err != nil {
				t.Fatal(err)
			}
			if wantKind := cmp.Or(tt.kind, "echo"); kind != wantKind {
				t.Fatalf("kind = %q, want %q", kind, wantKind)
			}
			desc, err := harness.Describe(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if desc.ID != tt.id || desc.Capabilities.Resumability != tt.resumability {
				t.Fatalf("descriptor = %+v, want %s with %s", desc, tt.id, tt.resumability)
			}
			if tt.kind == "chat" {
				chat, ok := harness.(chatagent.Harness)
				if !ok || chat.Model != tt.model {
					t.Fatalf("harness = %+v, want chat with configured model", harness)
				}
				if !desc.Capabilities.ForkSafe || !reflect.DeepEqual(desc.Models, []string{tt.model}) {
					t.Fatalf("chat descriptor = %+v", desc)
				}
			}
			if tt.kind == "tool" {
				tool, ok := harness.(toolagent.Harness)
				if !ok || tool.Model != tt.model || !reflect.DeepEqual(tool.Tools, []api.ToolDefinition{toolagent.PublishTool()}) {
					t.Fatalf("harness = %+v, want tool with the publish tool", harness)
				}
				if !reflect.DeepEqual(desc.Tools, []api.ToolSpec{{Name: "publish", Description: tool.Tools[0].Description, Mediation: api.MediationRequiresApproval}}) {
					t.Fatalf("tool descriptor = %+v", desc)
				}
			}
		})
	}
}

func TestSelectChatRequiresModel(t *testing.T) {
	for _, kind := range []string{"chat", "tool"} {
		t.Setenv("HARNESS_KIND", kind)
		t.Setenv("HARNESS_MODEL", "")
		harness, _, err := selectHarness(options{}, Settings{})
		if err == nil || !strings.Contains(err.Error(), "HARNESS_MODEL is required when HARNESS_KIND="+kind) {
			t.Fatalf("%s: error = %v, want missing HARNESS_MODEL", kind, err)
		}
		if harness != nil {
			t.Fatalf("invalid %s configuration returned a fallback harness", kind)
		}
	}
}

// namedHarness is a harness a WithHarness build serves.
type namedHarness struct{ s Settings }

func (namedHarness) Describe(context.Context) (api.Descriptor, error) {
	return api.Descriptor{ID: "extra"}, nil
}
func (namedHarness) Run(context.Context, *api.Start, api.EventSink) error { return nil }

func TestWithHarness(t *testing.T) {
	newExtra := func(s Settings) (api.Harness, error) { return namedHarness{s: s}, nil }
	for _, kind := range []string{"", "extra"} {
		t.Run("HARNESS_KIND="+kind, func(t *testing.T) {
			t.Setenv("HARNESS_KIND", kind)
			o, err := newOptions([]Option{WithHarness("extra", newExtra)})
			if err != nil {
				t.Fatal(err)
			}
			harness, got, err := selectHarness(o, Settings{MaxRecvMsgSize: 16 << 20})
			if err != nil {
				t.Fatal(err)
			}
			if got != "extra" || harness != (namedHarness{s: Settings{MaxRecvMsgSize: 16 << 20}}) {
				t.Fatalf("selectHarness = %+v, %q; want the extra harness with the server settings", harness, got)
			}
		})
	}
}

// A WithHarness build serves only its own harness: a built-in or unknown HARNESS_KIND fails the
// start instead of falling back to echo.
func TestWithHarnessRefusesOtherKinds(t *testing.T) {
	called := false
	o, err := newOptions([]Option{WithHarness("extra", func(Settings) (api.Harness, error) {
		called = true
		return namedHarness{}, nil
	})})
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"echo", "counter", "extr", "unknown"} {
		t.Setenv("HARNESS_KIND", kind)
		harness, _, err := selectHarness(o, Settings{})
		if err == nil || err.Error() != `HARNESS_KIND="`+kind+`": this build serves only "extra"` {
			t.Fatalf("HARNESS_KIND=%s: error = %v", kind, err)
		}
		if harness != nil {
			t.Fatalf("HARNESS_KIND=%s: returned harness %T", kind, harness)
		}
	}
	if called {
		t.Fatal("the harness was built for another kind")
	}
}

func TestWithHarnessErrors(t *testing.T) {
	errBuild := errors.New("bad configuration")
	ok := func(Settings) (api.Harness, error) { return namedHarness{}, nil }
	t.Setenv("HARNESS_KIND", "")
	for _, tt := range []struct {
		name    string
		opts    []Option
		wantOpt string // error from the options
		wantSel string // error from selectHarness
	}{
		{name: "empty kind", opts: []Option{WithHarness("", ok)}, wantOpt: "WithHarness needs a kind and a HarnessFunc"},
		{name: "nil func", opts: []Option{WithHarness("extra", nil)}, wantOpt: "WithHarness needs a kind and a HarnessFunc"},
		{name: "twice", opts: []Option{WithHarness("extra", ok), WithHarness("other", ok)}, wantOpt: "WithHarness given more than once"},
		{name: "build error", opts: []Option{WithHarness("extra", func(Settings) (api.Harness, error) { return nil, errBuild })}, wantSel: errBuild.Error()},
		{name: "nil harness", opts: []Option{WithHarness("extra", func(Settings) (api.Harness, error) { return nil, nil })}, wantSel: "extra harness: HarnessFunc returned no harness"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			o, err := newOptions(tt.opts)
			if tt.wantOpt != "" {
				if err == nil || err.Error() != tt.wantOpt {
					t.Fatalf("options error = %v, want %q", err, tt.wantOpt)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			harness, _, err := selectHarness(o, Settings{})
			if err == nil || err.Error() != tt.wantSel || harness != nil {
				t.Fatalf("selectHarness = %v, %v; want error %q", harness, err, tt.wantSel)
			}
		})
	}
}
