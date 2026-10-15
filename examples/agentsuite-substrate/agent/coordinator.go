package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"slices"
	"strings"

	"github.com/aramase/agentsessions/api"
	"github.com/aramase/agentsessions/examples/agentsuite-substrate/invoke"
)

// ApprovalEnv names the environment variable listing, comma-separated, the invoke targets whose
// calls this harness sends as REQUIRES_APPROVAL. The ActorTemplate sets it from emit render
// -approve; the host's executor refuses an ungated call to a gated target either way.
const ApprovalEnv = "AGENTSUITE_APPROVAL"

// maxDelegations bounds one turn's delegations, so a model that keeps delegating ends the turn
// with an error instead of running without bound.
const maxDelegations = 4

// delegationPrompt is appended to the agent's instructions for an agent with invokes edges. The
// model has no tool definitions (model/openai does not map them), so it asks for a delegation with
// a one-line JSON object, which the harness turns into the agentsuite.invoke tool call.
func delegationPrompt(targets []string) string {
	return fmt.Sprintf(`

You can delegate to these agents: %s.
To delegate, reply with only one line of JSON and nothing else: {"invoke": "<agent>", "task": "<the complete task for that agent>"}
The next message gives you that agent's reply, or says why the delegation did not run.
Delegate one task at a time. When you have what you need, reply with the final answer as plain text, not JSON.`, strings.Join(targets, ", "))
}

// delegation is a model reply asking for an invocation.
type delegation struct {
	Invoke string `json:"invoke"`
	Task   string `json:"task"`
}

// parseDelegation reads a reply that is one JSON object with a non-empty "invoke", optionally in a
// code fence. Anything else is the final answer.
func parseDelegation(text string) (delegation, bool) {
	s := strings.TrimSpace(text)
	s = strings.TrimPrefix(s, "```json")
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimSuffix(s, "```")
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "{") || !strings.HasSuffix(s, "}") {
		return delegation{}, false
	}
	var d delegation
	if err := json.Unmarshal([]byte(s), &d); err != nil || d.Invoke == "" {
		return delegation{}, false
	}
	return d, true
}

func replyText(m api.Message) string {
	var b strings.Builder
	for _, p := range m.Parts {
		if p.Text != nil {
			b.WriteString(p.Text.Text)
		}
	}
	return b.String()
}

// runCoordinator is Run for an agent with invokes edges: it calls the model, and while the reply
// asks for a delegation it makes the agentsuite.invoke tool call and feeds the result back. The host
// executes the call (a child session on the target's harness) and journals it; on replay or resume
// the recorded result is served, so a delegation never runs twice. A gated call parks the turn
// (sink.ToolCall returns the park, which Run returns) until a decision; a denial comes back as an
// error result the model sees, and the turn continues without that delegation.
func (h Harness) runCoordinator(ctx context.Context, start *api.Start, sink api.EventSink) error {
	messages := make([]api.Message, 0, 1+len(start.History)+len(start.Inputs)+2*maxDelegations)
	messages = append(messages, *api.TextMessage("system", h.Instructions+delegationPrompt(h.Invokes)))
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
	for step := 1; ; step++ {
		resp, err := sink.Model(ctx, api.ModelRequest{Model: h.Model, Messages: messages})
		if err != nil {
			return err
		}
		if err := reportUsage(ctx, sink, resp); err != nil {
			return err
		}
		d, ok := parseDelegation(replyText(resp.Message))
		if !ok {
			return nil
		}
		if step > maxDelegations {
			return fmt.Errorf("coordinator: the model asked for more than %d delegations in one turn", maxDelegations)
		}
		// The id is stable across replay and resume, which re-drive the turn under its recorded
		// execution id, so the recorded call and result match.
		id := fmt.Sprintf("%s-invoke-%d", start.ExecutionID, step)
		mediation := api.MediationControllerMediated
		if slices.Contains(h.Approval, d.Invoke) {
			mediation = api.MediationRequiresApproval
		}
		log.Printf("harness: delegating to %s (%s), call %s", d.Invoke, mediation, id)
		res, err := sink.ToolCall(ctx, api.ToolCall{
			ID:             id,
			Tool:           invoke.Tool,
			Args:           map[string]any{"agent": d.Invoke, "task": d.Task},
			Mediation:      mediation,
			IdempotencyKey: id,
		})
		if err != nil {
			return err
		}
		messages = append(messages, resp.Message, *api.TextMessage("user", resultText(d.Invoke, res)))
	}
}

// resultText is how a delegation's outcome reaches the model.
func resultText(agent string, res api.ToolResult) string {
	if res.IsError {
		return fmt.Sprintf("The delegation to %s did not run: %s", agent, res.Error)
	}
	out, _ := res.Output["output"].(string)
	return fmt.Sprintf("Reply from %s:\n%s", agent, out)
}
