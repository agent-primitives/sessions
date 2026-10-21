package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestConversationContext(t *testing.T) {
	for _, stream := range []string{"false", "true"} {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
			`{"model":"smoke-model","stream":`+stream+`,"messages":[{"content":"first"},{"content":"reply"},{"content":"second"}]}`))
		res := httptest.NewRecorder()
		chat(res, req)
		if res.Code != http.StatusOK || !strings.Contains(res.Body.String(), "context=3 last=second") {
			t.Fatalf("response = %d %s", res.Code, res.Body.String())
		}
		if stream == "true" && !strings.Contains(res.Body.String(), "[DONE]") {
			t.Fatal("stream did not terminate")
		}
	}
}
