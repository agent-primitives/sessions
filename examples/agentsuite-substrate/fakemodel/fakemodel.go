// Package fakemodel is a deterministic OpenAI-compatible chat completions endpoint that counts the
// calls it serves. The demo points agentsessionsd at it so it needs no credentials, and reads the
// count to prove a replay made no model call.
package fakemodel

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
)

// Server answers POST /v1/chat/completions (streamed or not) and GET /stats.
type Server struct {
	calls atomic.Int64
}

type message struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

type request struct {
	Model    string    `json:"model"`
	Messages []message `json:"messages"`
	Stream   bool      `json:"stream"`
}

// Calls is how many completions the server has answered.
func (s *Server) Calls() int64 { return s.calls.Load() }

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/stats":
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]int64{"calls": s.Calls()})
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/chat/completions"):
		s.complete(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) complete(w http.ResponseWriter, r *http.Request) {
	var req request
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	n := s.calls.Add(1)
	reply := Reply(req.Messages)
	if !req.Stream {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":      fmt.Sprintf("fake-%d", n),
			"object":  "chat.completion",
			"model":   req.Model,
			"choices": []any{map[string]any{"index": 0, "message": map[string]string{"role": "assistant", "content": reply}, "finish_reason": "stop"}},
			"usage":   map[string]int{"prompt_tokens": len(req.Messages), "completion_tokens": 1, "total_tokens": len(req.Messages) + 1},
		})
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	flusher, _ := w.(http.Flusher)
	send := func(v any) {
		b, _ := json.Marshal(v)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", b)
		if flusher != nil {
			flusher.Flush()
		}
	}
	send(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]string{"content": reply}}}})
	send(map[string]any{"choices": []any{}, "usage": map[string]int{"prompt_tokens": len(req.Messages), "completion_tokens": 1}})
	_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
}

// Reply is a pure function of the conversation, so the same request always gets the same answer:
// the issues the user has raised so far, one per user message, and the next step. It is how the demo
// shows state: after a resume the list continues, and a fork carries only its prefix.
func Reply(messages []message) string {
	var issues []string
	system := false
	for _, m := range messages {
		text := strings.TrimSpace(contentText(m.Content))
		switch m.Role {
		case "system":
			system = text != ""
		case "user":
			issues = append(issues, text)
		}
	}
	if len(issues) == 0 {
		return "no issues raised yet"
	}
	var b strings.Builder
	for i, issue := range issues {
		if i > 0 {
			b.WriteString("; ")
		}
		fmt.Fprintf(&b, "%d) %s", i+1, issue)
	}
	next := "ask for the account id"
	if !system {
		next = "no instructions received"
	}
	return fmt.Sprintf("open issues: %s | next: %s", b.String(), next)
}

// contentText reads an OpenAI content field, a string or a list of {type: text, text} parts.
func contentText(c any) string {
	switch v := c.(type) {
	case string:
		return v
	case []any:
		var b strings.Builder
		for _, p := range v {
			if m, ok := p.(map[string]any); ok {
				if t, ok := m["text"].(string); ok {
					b.WriteString(t)
				}
			}
		}
		return b.String()
	}
	return ""
}
