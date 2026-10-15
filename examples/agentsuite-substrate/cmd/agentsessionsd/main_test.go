package main

import (
	"strings"
	"testing"
)

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
