package main

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/aramase/agentsessions/examples/agentsuite-substrate/agent"
)

// AGENTSUITE_MEMBERS_JSON must be base64 JSON whose members carry their own id, a model and
// instructions matching their digest.
func TestParseMembers(t *testing.T) {
	enc := func(m map[string]agent.Member) string {
		b, _ := json.Marshal(m)
		return base64.StdEncoding.EncodeToString(b)
	}
	ok := agent.Member{ID: "writer", Model: "m", Instructions: "Write.", InstructionsDigest: agent.Digest("Write.")}
	if got, err := parseMembers(enc(map[string]agent.Member{"writer": ok})); err != nil || got["writer"].Instructions != "Write." {
		t.Fatalf("parseMembers = %v, %v", got, err)
	}
	tampered := ok
	tampered.Instructions = "Other."
	for name, v := range map[string]string{
		"not base64":   "%%%",
		"empty":        enc(map[string]agent.Member{}),
		"wrong id":     enc(map[string]agent.Member{"reviewer": ok}),
		"wrong digest": enc(map[string]agent.Member{"writer": tampered}),
	} {
		if _, err := parseMembers(v); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// AGENTSUITE_INVOKES is parsed with the same Policy.Set the emit output round-trips through; a
// malformed edge refuses the start.
func TestParsePolicy(t *testing.T) {
	p, err := parsePolicy("coordinator=reviewer:1:1:approval, coordinator=writer:1:1")
	if err != nil {
		t.Fatal(err)
	}
	if got := p.String(); got != "coordinator=reviewer:1:1:approval,coordinator=writer:1:1" {
		t.Fatalf("policy = %q", got)
	}
	if p, err := parsePolicy(""); err != nil || len(p) != 0 {
		t.Fatalf("empty: %v, %v", p, err)
	}
	for _, bad := range []string{"coordinator=writer", "coordinator=writer:1:1,coordinator=writer:1:1", "coordinator=writer:1:1:auto"} {
		if _, err := parsePolicy(bad); err == nil {
			t.Errorf("parsePolicy(%q) accepted", bad)
		} else if !strings.Contains(err.Error(), "coordinator") && !strings.Contains(err.Error(), "want") {
			t.Errorf("parsePolicy(%q): %v", bad, err)
		}
	}
}
