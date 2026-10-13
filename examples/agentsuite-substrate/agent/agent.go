// Package agent is the harness an AgentSuite agent runs as: a stateless conversational loop whose
// system prompt is the agent's instructions. Every model call goes through the host (sink.Model), so
// the host records each completion and serves it from the journal on replay.
package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"

	"github.com/kaimahi-agents/kaimahi/agentsuite"

	"github.com/aramase/agentsessions/api"
)

// Harness runs one agent. It keeps nothing in memory between turns: each turn's context is the
// instructions, the recorded history, and the new inputs, so a fresh actor resumes it by replay.
type Harness struct {
	ID           string
	Model        string
	Instructions string
}

var _ api.Harness = Harness{}

// Load reads the agent manifest (agent.json) and the instructions it names from dir, and refuses
// instructions whose SHA-256 differs from the digest the manifest pins.
func Load(dir string) (Harness, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "agent.json"))
	if err != nil {
		return Harness{}, err
	}
	var a agentsuite.Agent
	if err := agentsuite.Unmarshal(raw, &a); err != nil {
		return Harness{}, fmt.Errorf("agent.json: %w", err)
	}
	instructions, err := os.ReadFile(filepath.Join(dir, "instructions.md"))
	if err != nil {
		return Harness{}, err
	}
	sum := sha256.Sum256(instructions)
	if got := "sha256:" + hex.EncodeToString(sum[:]); got != a.Instructions.Digest {
		return Harness{}, fmt.Errorf("instructions digest %s does not match the agent manifest's %s", got, a.Instructions.Digest)
	}
	return Harness{ID: a.ID, Model: a.Model.Model, Instructions: string(instructions)}, nil
}

// Describe reports the agent id and a version derived from the instructions, so a replay against a
// harness with different instructions is refused as a version mismatch rather than diverging.
func (h Harness) Describe(context.Context) (api.Descriptor, error) {
	sum := sha256.Sum256([]byte(h.Instructions))
	return api.Descriptor{
		ID:      h.ID,
		Version: "instructions-" + hex.EncodeToString(sum[:])[:12],
		Models:  []string{h.Model},
		Capabilities: api.Capabilities{
			Resumability: api.ResumabilityStatelessReplay,
			ForkSafe:     true,
		},
	}, nil
}

// Run sends one model request: the instructions as the system message, then the recorded
// conversation in journal order, then this turn's inputs.
func (h Harness) Run(ctx context.Context, start *api.Start, sink api.EventSink) error {
	messages := make([]api.Message, 0, 1+len(start.History)+len(start.Inputs))
	if h.Instructions != "" {
		messages = append(messages, *api.TextMessage("system", h.Instructions))
	}
	for _, event := range start.History {
		if event.Message == nil {
			continue
		}
		switch event.Kind {
		case api.EventInput, api.EventOutput:
			messages = append(messages, *event.Message)
		}
	}
	messages = append(messages, start.Inputs...)
	_, err := sink.Model(ctx, api.ModelRequest{Model: h.Model, Messages: messages})
	return err
}
