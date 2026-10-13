package fakemodel

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/aramase/agentsessions/api"
	"github.com/aramase/agentsessions/model/openai"
)

// The fake speaks the dialect the host's OpenAI-compatible client reads, streamed and not, and
// counts every completion it serves.
func TestServesTheHostClient(t *testing.T) {
	srv := &Server{}
	ts := httptest.NewServer(srv)
	defer ts.Close()
	client, err := openai.New(openai.WithModel("fake"), openai.WithBaseURL(ts.URL+"/v1"))
	if err != nil {
		t.Fatal(err)
	}
	req := api.ModelRequest{Messages: []api.Message{
		*api.TextMessage("system", "triage"),
		*api.TextMessage("user", "laptop"),
		*api.TextMessage("assistant", "noted"),
		*api.TextMessage("user", "vpn"),
	}}
	const want = "open issues: 1) laptop; 2) vpn | next: ask for the account id"
	resp, err := client.Model(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if got := resp.Message.Text(); got != want {
		t.Fatalf("reply = %q, want %q", got, want)
	}
	var streamed string
	resp, err = client.StreamModel(context.Background(), req, func(s string) { streamed += s })
	if err != nil {
		t.Fatal(err)
	}
	if resp.Message.Text() != want || streamed != want {
		t.Fatalf("streamed reply = %q / %q, want %q", resp.Message.Text(), streamed, want)
	}
	if srv.Calls() != 2 {
		t.Fatalf("calls = %d, want 2", srv.Calls())
	}
	stats, err := http.Get(ts.URL + "/stats")
	if err != nil || stats.StatusCode != http.StatusOK {
		t.Fatalf("stats: %v %v", stats, err)
	}
	_ = stats.Body.Close()
}

func TestReplyWithoutInstructionsSaysSo(t *testing.T) {
	got := Reply([]message{{Role: "user", Content: []any{map[string]any{"type": "text", "text": "hi"}}}})
	if want := "open issues: 1) hi | next: no instructions received"; got != want {
		t.Fatalf("reply = %q, want %q", got, want)
	}
}
