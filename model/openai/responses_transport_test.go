package openai_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agent-primitives/sessions/api"
	"github.com/agent-primitives/sessions/model/openai"
)

type responsesRoundTripper func(*http.Request) (*http.Response, error)

func (f responsesRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// The independent protocol must retain the existing URL, header and copied-client safety.
func TestResponsesCustomTransportAndAzureURL(t *testing.T) {
	var calls int
	h := &http.Client{Transport: responsesRoundTripper(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.String() != "https://azure.invalid/openai/v1/responses?tenant=a&api-version=2025-04-01" {
			t.Error(r.URL)
		}
		if r.Header.Get("Authorization") != "Bearer local-key" || r.Header.Get("X-Team") != "team" {
			t.Error(r.Header)
		}
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(responsesText)), Request: r}, nil
	})}
	c, err := openai.NewResponses(openai.WithModel("m"), openai.WithBaseURL("https://azure.invalid/openai/v1?tenant=a#fragment"), openai.WithPath("responses?api-version=2025-04-01"), openai.WithAPIKey("local-key"), openai.WithHeader("X-Team", "team"), openai.WithHTTPClient(h))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Model(t.Context(), api.ModelRequest{}); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || h.CheckRedirect != nil {
		t.Fatalf("calls=%d, client was modified=%v", calls, h.CheckRedirect != nil)
	}
}

func TestResponsesRedirectSafety(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "nonstream", true: "stream"}[stream], func(t *testing.T) {
			var targetHits atomic.Int32
			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				targetHits.Add(1)
				_, _ = io.WriteString(w, responsesText)
			}))
			defer target.Close()
			c := newResponses(t, func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, target.URL+"/responses", http.StatusTemporaryRedirect)
			}, openai.WithHeader("api-key", "must-not-leak"))
			var err error
			if stream {
				_, err = c.StreamModel(t.Context(), api.ModelRequest{}, nil)
			} else {
				_, err = c.Model(t.Context(), api.ModelRequest{})
			}
			if err == nil || !strings.Contains(err.Error(), "request failed") || strings.Contains(err.Error(), target.URL) || targetHits.Load() != 0 {
				t.Fatalf("err=%v target hits=%d", err, targetHits.Load())
			}
		})
	}
	var policyCalls atomic.Int32
	c := newResponses(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/moved" {
			http.Redirect(w, r, "/moved", http.StatusTemporaryRedirect)
			return
		}
		if r.Header.Get("api-key") != "local-key" {
			t.Error("lost configured credential")
		}
		_, _ = io.WriteString(w, responsesText)
	}, openai.WithHeader("api-key", "local-key"), openai.WithHTTPClient(&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { policyCalls.Add(1); return nil }}))
	if _, err := c.Model(t.Context(), api.ModelRequest{}); err != nil {
		t.Fatal(err)
	}
	if policyCalls.Load() != 1 {
		t.Fatalf("redirect policy calls=%d", policyCalls.Load())
	}
}

func TestResponsesHTTPErrorsAndDeadline(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, body := range []string{`{"error":{"message":"provider rejected"}}`, "provider rejected"} {
			t.Run(body, func(t *testing.T) {
				c := newResponses(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(429); _, _ = io.WriteString(w, body) })
				var err error
				if stream {
					_, err = c.StreamModel(t.Context(), api.ModelRequest{}, nil)
				} else {
					_, err = c.Model(t.Context(), api.ModelRequest{})
				}
				if err == nil || !strings.Contains(err.Error(), "429") || strings.Contains(err.Error(), "provider rejected") {
					t.Fatalf("err=%v", err)
				}
			})
		}
		t.Run(map[bool]string{false: "deadline nonstream", true: "deadline stream"}[stream], func(t *testing.T) {
			c, err := openai.NewResponses(openai.WithModel("m"), openai.WithHTTPClient(&http.Client{Transport: responsesRoundTripper(func(r *http.Request) (*http.Response, error) { <-r.Context().Done(); return nil, r.Context().Err() })}))
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
			defer cancel()
			if stream {
				_, err = c.StreamModel(ctx, api.ModelRequest{}, nil)
			} else {
				_, err = c.Model(ctx, api.ModelRequest{})
			}
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("err=%v", err)
			}
		})
	}
}
