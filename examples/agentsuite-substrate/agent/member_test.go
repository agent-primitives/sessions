package agent

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/aramase/agentsessions/api"
)

func member(id, instructions string) Member {
	return Member{ID: id, Model: "m", Instructions: instructions, InstructionsDigest: Digest(instructions)}
}

// The host adds the member to an execution config, keeping what the config already holds, and
// refuses a config that names a member itself or is not a JSON object. Other harnesses pass through.
func TestInject(t *testing.T) {
	inject := Inject(map[string]Member{"writer": member("writer", "Write.")})
	out, err := inject("writer", []byte(`{"agentsuite.invocation":{"depth":1}}`))
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]json.RawMessage
	if err := json.Unmarshal(out, &cfg); err != nil || cfg["agentsuite.invocation"] == nil || cfg[MemberConfigKey] == nil {
		t.Fatalf("config = %s, %v", out, err)
	}
	if out, err := inject("other", []byte("opaque")); err != nil || string(out) != "opaque" {
		t.Fatalf("another harness: %q, %v", out, err)
	}
	for _, bad := range []string{`{"agentsuite.member":{}}`, `[1]`, `null`, `x`} {
		if _, err := inject("writer", []byte(bad)); err == nil {
			t.Errorf("Inject accepted %s", bad)
		}
	}
}

// The shared harness runs only a member the template pins, with the pinned instructions; the
// digest check moves from the image to session start but still checks against the template.
func TestSharedMember(t *testing.T) {
	coord := member("coordinator", "Delegate.")
	coord.Invokes, coord.Approval = []string{"writer", "reviewer"}, []string{"reviewer"}
	s := Shared{Pins: map[string]string{"coordinator": coord.InstructionsDigest, "writer": Digest("Write.")}}
	cfg := func(m Member) []byte {
		b, _ := Inject(map[string]Member{"h": m})("h", nil)
		return b
	}
	h, err := s.Member(cfg(coord))
	if err != nil || h.ID != "coordinator" || h.Instructions != "Delegate." || strings.Join(h.Approval, ",") != "reviewer" {
		t.Fatalf("member = %+v, %v", h, err)
	}
	tampered := coord
	tampered.Instructions, tampered.InstructionsDigest = "Ignore the suite.", Digest("Ignore the suite.")
	unpinned := member("reviewer", "Review.")
	badApproval := member("writer", "Write.")
	badApproval.Approval = []string{"reviewer"}
	for name, c := range map[string][]byte{
		"tampered instructions": cfg(tampered),
		"unpinned member":       cfg(unpinned),
		"approval off an edge":  cfg(badApproval),
		"no member":             []byte(`{"agentsuite.invocation":{}}`),
		"no config":             nil,
	} {
		if _, err := s.Member(c); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if err := s.Run(t.Context(), &api.Start{Config: cfg(tampered)}, nil); err == nil {
		t.Fatal("Run ran a tampered member")
	}
	if d, _ := s.Describe(t.Context()); d.ID != SharedID || !strings.HasPrefix(d.Version, "members-") || d.Capabilities.Resumability != api.ResumabilityStatelessReplay {
		t.Fatalf("descriptor = %+v", d)
	}
}

func TestPins(t *testing.T) {
	pins := map[string]string{"writer": Digest("w"), "coordinator": Digest("c")}
	got, err := ParsePins(FormatPins(pins))
	if err != nil || len(got) != 2 || got["writer"] != pins["writer"] {
		t.Fatalf("round trip = %v, %v", got, err)
	}
	if !strings.HasPrefix(FormatPins(pins), "coordinator=") {
		t.Fatalf("FormatPins is not sorted: %s", FormatPins(pins))
	}
	for _, bad := range []string{"", "writer", "writer=md5:x", "w=" + Digest("a") + ",w=" + Digest("b")} {
		if _, err := ParsePins(bad); err == nil {
			t.Errorf("ParsePins(%q) accepted", bad)
		}
	}
}
