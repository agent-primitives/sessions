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
	"slices"
	"sync"

	"github.com/kaimahi-agents/kaimahi/agentsuite"

	"github.com/aramase/agentsessions/api"
)

// RetiredModelEnv named the environment variable that overrode the model the agent's manifest
// declares. The suite now pins the model and the binding must serve exactly that one, so the
// harness refuses to start when it is set.
const RetiredModelEnv = "AGENTSUITE_MODEL"

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
	// Invokes are the agents the manifest's invokes edges let this agent delegate to. A harness
	// with any runs the coordinator loop (see coordinator.go).
	Invokes []string
	// Approval are the invoke targets the deployment gates (ApprovalEnv): the harness sends calls
	// to them as REQUIRES_APPROVAL.
	Approval []string
	memory   *transcript
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
	h := Harness{ID: a.ID, Model: a.Model.Model, Instructions: string(instructions)}
	for _, inv := range a.Invokes {
		h.Invokes = append(h.Invokes, inv.Agent)
	}
	return h, nil
}

// WithApproval returns h sending its calls to the named invoke targets as REQUIRES_APPROVAL. Every
// name must be one of h.Invokes, so a typo cannot leave a delegation ungated.
func (h Harness) WithApproval(targets []string) (Harness, error) {
	for _, t := range targets {
		if !slices.Contains(h.Invokes, t) {
			return h, fmt.Errorf("approval for %q: agent %s has no invokes edge to it", t, h.ID)
		}
	}
	h.Approval = slices.Clone(targets)
	return h, nil
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
	if len(h.Invokes) > 0 {
		return h.runCoordinator(ctx, start, sink)
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
	log.Printf("harness: stateless tier: %d journal events in, %d messages to the model", len(start.History), len(messages))
	resp, err := sink.Model(ctx, api.ModelRequest{Model: h.Model, Messages: messages})
	if err != nil {
		return err
	}
	return reportUsage(ctx, sink, resp)
}

// reportUsage journals the token usage the provider returned with a completion. Replay serves the
// recorded usage with the recorded completion, so a replayed turn reports the same record. A
// provider that returns no usage records nothing.
func reportUsage(ctx context.Context, sink api.EventSink, resp api.ModelResponse) error {
	if resp.Usage == (api.Usage{}) {
		return nil
	}
	log.Printf("harness: model %s used %d input and %d output tokens", resp.Usage.Model, resp.Usage.InputTokens, resp.Usage.OutputTokens)
	return sink.Usage(ctx, resp.Usage)
}

// runFromMemory is Run for the memory tier. The transcript gains this turn's inputs and the model's
// reply only when the model call and its usage record succeed, so a failed turn leaves RAM as it
// was. It logs how much it holds, which is what a resumed or replaced actor shows in its logs.
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
	if err := reportUsage(ctx, sink, resp); err != nil {
		return err
	}
	t.messages = append(t.messages, start.Inputs...)
	t.messages = append(t.messages, resp.Message)
	t.turns++
	return nil
}
