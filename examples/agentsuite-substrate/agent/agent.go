// Package agent is the harness an AgentSuite agent runs as: a stateless conversational loop whose
// system prompt is the agent's instructions. Every model call goes through the host (sink.Model), so
// the host records each completion and serves it from the journal on replay.
package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"

	"github.com/kaimahi-agents/kaimahi/agentsuite"

	"github.com/aramase/agentsessions/api"
)

// ModelEnv names the environment variable that overrides the model the agent's manifest declares.
// The ActorTemplate sets it when the host is pointed at a real provider, which serves its own model
// IDs rather than the fixture's.
const ModelEnv = "AGENTSUITE_MODEL"

// MemoryEnv names the environment variable that selects the memory tier: set to MemoryRAM, the
// harness keeps the conversation in its own RAM instead of rebuilding it from the journal.
const MemoryEnv = "AGENTSUITE_MEMORY"

// MemoryRAM is the one value of MemoryEnv.
const MemoryRAM = "ram"

// Harness runs one agent. By default it keeps nothing in memory between turns: each turn's context
// is the instructions, the recorded history, and the new inputs, so a fresh actor resumes it by
// replay. WithMemory returns the memory-tier variant, whose conversation lives only in RAM.
type Harness struct {
	ID           string
	Model        string
	Instructions string
	memory       *transcript
}

// transcript is the conversation a memory-tier harness holds in RAM: one actor serves one session,
// so one transcript per process is the session's.
type transcript struct {
	mu       sync.Mutex
	messages []api.Message
	turns    int
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

// WithModel returns h requesting model instead of the manifest's model. An empty model keeps the
// manifest's.
func (h Harness) WithModel(model string) Harness {
	if model != "" {
		h.Model = model
	}
	return h
}

// WithMemory returns h as a REQUIRES_MEMORY_SNAPSHOT harness: it builds each model request from the
// conversation it holds in RAM and ignores the journal history the host sends, so only a memory
// snapshot of its actor (suspend, fork by clone) carries the conversation to the next turn. A fresh
// actor starts with an empty transcript, which is why the host refuses to give a session that has
// started turns a new one.
func (h Harness) WithMemory() Harness {
	h.memory = &transcript{}
	return h
}

// Describe reports the agent id and a version derived from the instructions, so a replay against a
// harness with different instructions is refused as a version mismatch rather than diverging.
func (h Harness) Describe(context.Context) (api.Descriptor, error) {
	sum := sha256.Sum256([]byte(h.Instructions))
	resumability := api.ResumabilityStatelessReplay
	if h.memory != nil {
		resumability = api.ResumabilityRequiresMemorySnapshot
	}
	return api.Descriptor{
		ID:      h.ID,
		Version: "instructions-" + hex.EncodeToString(sum[:])[:12],
		Models:  []string{h.Model},
		Capabilities: api.Capabilities{
			Resumability: resumability,
			ForkSafe:     true,
		},
	}, nil
}

// Run sends one model request: the instructions as the system message, then the conversation so
// far, then this turn's inputs. The conversation is the recorded history in journal order, or for
// the memory tier the transcript in RAM.
func (h Harness) Run(ctx context.Context, start *api.Start, sink api.EventSink) error {
	if h.memory != nil {
		return h.runFromMemory(ctx, start, sink)
	}
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

// runFromMemory is Run for the memory tier. The transcript gains this turn's inputs and the model's
// reply only when the model call succeeds, so a failed turn leaves RAM as it was. It logs how much
// it holds, which is what a resumed or replaced actor shows in its logs.
func (h Harness) runFromMemory(ctx context.Context, start *api.Start, sink api.EventSink) error {
	t := h.memory
	t.mu.Lock()
	defer t.mu.Unlock()
	messages := make([]api.Message, 0, 1+len(t.messages)+len(start.Inputs))
	if h.Instructions != "" {
		messages = append(messages, *api.TextMessage("system", h.Instructions))
	}
	messages = append(messages, t.messages...)
	messages = append(messages, start.Inputs...)
	log.Printf("harness: memory tier: turn %d of this process; %d messages in RAM, %d journal events not used",
		t.turns+1, len(t.messages), len(start.History))
	resp, err := sink.Model(ctx, api.ModelRequest{Model: h.Model, Messages: messages})
	if err != nil {
		return err
	}
	t.messages = append(t.messages, start.Inputs...)
	t.messages = append(t.messages, resp.Message)
	t.turns++
	return nil
}
