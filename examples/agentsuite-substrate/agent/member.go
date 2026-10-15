package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/aramase/agentsessions/api"
)

// MembersEnv names the environment variable that selects the shared harness: one image and one
// ActorTemplate for every member of a suite. Its value pins each member's instructions,
// comma-separated <agent id>=sha256:<hex> pairs that emit render sets in the template. The member a
// session runs as, and its instructions, arrive in each execution's config (MemberConfigKey), which
// the host sets; the harness refuses a member or instructions the template does not pin.
const MembersEnv = "AGENTSUITE_MEMBERS"

// MemberConfigKey is the key of the member in an execution config.
const MemberConfigKey = "agentsuite.member"

// SharedID is the descriptor id of the shared harness, which is no one agent.
const SharedID = "agentsuite-member"

// Member is one suite agent as the host hands it to the shared harness at session start.
type Member struct {
	ID           string   `json:"id"`
	Model        string   `json:"model"`
	Invokes      []string `json:"invokes,omitempty"`
	Approval     []string `json:"approval,omitempty"`
	Instructions string   `json:"instructions"`
	// InstructionsDigest is the agent manifest's sha256:<hex> of Instructions.
	InstructionsDigest string `json:"instructionsDigest"`
}

// ParsePins reads a MembersEnv value.
func ParsePins(v string) (map[string]string, error) {
	pins := map[string]string{}
	for _, pair := range strings.Split(v, ",") {
		id, digest, ok := strings.Cut(strings.TrimSpace(pair), "=")
		if !ok || id == "" || !strings.HasPrefix(digest, "sha256:") || len(digest) != len("sha256:")+64 {
			return nil, fmt.Errorf("%s: want <agent>=sha256:<64 hex>[,...], got %q", MembersEnv, pair)
		}
		if _, dup := pins[id]; dup {
			return nil, fmt.Errorf("%s: agent %q is pinned twice", MembersEnv, id)
		}
		pins[id] = digest
	}
	return pins, nil
}

// FormatPins is ParsePins' inverse, sorted by agent id.
func FormatPins(pins map[string]string) string {
	var out []string
	for _, id := range slices.Sorted(maps.Keys(pins)) {
		out = append(out, id+"="+pins[id])
	}
	return strings.Join(out, ",")
}

// Digest is the sha256:<hex> of instructions, as an agent manifest pins it.
func Digest(instructions string) string {
	sum := sha256.Sum256([]byte(instructions))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// Inject returns the host's execution-config hook for the shared harness: for a turn on a member's
// harness (named by its agent id) it adds that member under MemberConfigKey to the config, which
// must be empty or a JSON object, so the member is journaled in the turn's EXECUTION_START. A config
// that already names a member is refused: only the host chooses it. Other harnesses' configs pass
// through unchanged.
func Inject(members map[string]Member) func(harness string, config []byte) ([]byte, error) {
	return func(harness string, config []byte) ([]byte, error) {
		m, ok := members[harness]
		if !ok {
			return config, nil
		}
		cfg := map[string]json.RawMessage{}
		if len(config) > 0 {
			if err := json.Unmarshal(config, &cfg); err != nil || cfg == nil {
				return nil, fmt.Errorf("harness %s: the execution config must be a JSON object, so the host can add the suite member", harness)
			}
		}
		if _, set := cfg[MemberConfigKey]; set {
			return nil, fmt.Errorf("harness %s: the execution config sets %s, which only the host sets", harness, MemberConfigKey)
		}
		raw, err := json.Marshal(m)
		if err != nil {
			return nil, err
		}
		cfg[MemberConfigKey] = raw
		return json.Marshal(cfg)
	}
}

// Shared is the harness of a shared template: it runs whichever pinned member the execution config
// names, with that member's instructions, model, invokes edges and approval targets.
type Shared struct {
	Pins map[string]string
}

var _ api.Harness = Shared{}

// Describe reports the shared harness: its version covers the pinned members, so a replay against a
// template pinning other instructions is refused as a version mismatch.
func (s Shared) Describe(context.Context) (api.Descriptor, error) {
	sum := sha256.Sum256([]byte(FormatPins(s.Pins)))
	return api.Descriptor{
		ID:      SharedID,
		Version: "members-" + hex.EncodeToString(sum[:])[:12],
		Capabilities: api.Capabilities{
			Resumability: api.ResumabilityStatelessReplay,
			ForkSafe:     true,
		},
	}, nil
}

// Member reads and checks the member an execution config names: it must be pinned, and its
// instructions must hash to the pinned digest (and to the digest it carries).
func (s Shared) Member(config []byte) (Harness, error) {
	var cfg map[string]json.RawMessage
	if len(config) == 0 || json.Unmarshal(config, &cfg) != nil || cfg[MemberConfigKey] == nil {
		return Harness{}, errors.New("shared harness: the execution config names no suite member; the host sets it at session start")
	}
	var m Member
	if err := json.Unmarshal(cfg[MemberConfigKey], &m); err != nil {
		return Harness{}, fmt.Errorf("shared harness: member: %w", err)
	}
	pinned, ok := s.Pins[m.ID]
	if !ok {
		return Harness{}, fmt.Errorf("shared harness: member %q is not one this template pins", m.ID)
	}
	if got := Digest(m.Instructions); got != pinned || m.InstructionsDigest != pinned {
		return Harness{}, fmt.Errorf("shared harness: member %s: instructions digest %s does not match the template's %s", m.ID, got, pinned)
	}
	return Harness{ID: m.ID, Model: m.Model, Instructions: m.Instructions, Invokes: slices.Clone(m.Invokes)}.WithApproval(m.Approval)
}

// Run checks the member in the execution config and runs the turn as that member.
func (s Shared) Run(ctx context.Context, start *api.Start, sink api.EventSink) error {
	h, err := s.Member(start.Config)
	if err != nil {
		return err
	}
	return h.Run(ctx, start, sink)
}
