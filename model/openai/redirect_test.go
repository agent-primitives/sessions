package openai_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/aramase/agentsessions/api"
	"github.com/aramase/agentsessions/model/openai"
)

const redirectSecret = "secret-value-must-not-leak"

var redirectRequest = api.ModelRequest{Messages: []api.Message{*api.TextMessage("user", "q")}}

// callers covers both request paths, since each sends the configured headers.
var callers = []struct {
	name string
	call func(context.Context, *openai.Client) error
}{
	{"Model", func(ctx context.Context, c *openai.Client) error {
		_, err := c.Model(ctx, redirectRequest)
		return err
	}},
	{"StreamModel", func(ctx context.Context, c *openai.Client) error {
		_, err := c.StreamModel(ctx, redirectRequest, nil)
		return err
	}},
}

// credentials covers a custom header (Azure OpenAI's api-key) and the bearer path.
var credentials = []struct {
	name string
	opt  openai.Option
}{
	{"api-key", openai.WithHeader("api-key", redirectSecret)},
	{"bearer", openai.WithAPIKey(redirectSecret)},
}

// countingServer starts a server that counts the requests it receives before handling them.
func countingServer(t *testing.T, h http.HandlerFunc) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		if h != nil {
			h(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &n
}

func redirectTo(code int, location string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, location, code)
	}
}

// okIfAuthenticated answers a completion (plain or streamed) only when the request carries the
// configured credential and the original body.
func okIfAuthenticated(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.Header.Get("api-key") != redirectSecret && r.Header.Get("Authorization") != "Bearer "+redirectSecret {
			t.Errorf("redirect target got no credential")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if !strings.Contains(string(body), `"messages"`) {
			t.Errorf("redirect target got body %q, want the original request", body)
		}
		if strings.Contains(string(body), `"stream":true`) {
			_, _ = io.WriteString(w, sseCompletion("ok"))
			return
		}
		_, _ = io.WriteString(w, completion("ok"))
	}
}

// A redirect to another origin is refused before anything is sent there, so the configured
// credential never reaches a host the caller did not configure. net/http would otherwise copy a
// custom header such as api-key to any redirect target.
func TestCrossOriginRedirectIsRefused(t *testing.T) {
	codes := []int{
		http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther,
		http.StatusTemporaryRedirect, http.StatusPermanentRedirect,
	}
	for _, caller := range callers {
		for _, cred := range credentials {
			for _, code := range codes {
				t.Run(caller.name+"/"+cred.name+"/"+http.StatusText(code), func(t *testing.T) {
					b, bHits := countingServer(t, okIfAuthenticated(t))
					a, aHits := countingServer(t, redirectTo(code, b.URL+"/v1/chat/completions"))
					c, err := openai.New(openai.WithBaseURL(a.URL), openai.WithModel("m"), cred.opt)
					if err != nil {
						t.Fatal(err)
					}

					err = caller.call(t.Context(), c)
					if err == nil {
						t.Fatal("followed a cross-origin redirect")
					}
					if got := bHits.Load(); got != 0 {
						t.Fatalf("redirect target received %d requests, want 0", got)
					}
					if got := aHits.Load(); got != 1 {
						t.Fatalf("base URL received %d requests, want 1", got)
					}
					msg := err.Error()
					for _, host := range []string{a.Listener.Addr().String(), b.Listener.Addr().String()} {
						if !strings.Contains(msg, host) {
							t.Errorf("error %q does not name %s", msg, host)
						}
					}
					if strings.Contains(msg, redirectSecret) {
						t.Errorf("error %q carries the credential", msg)
					}
				})
			}
		}
	}
}

// A redirect within the base URL's origin is followed with the headers and body intact, as before.
func TestSameOriginRedirectIsFollowed(t *testing.T) {
	for _, caller := range callers {
		for _, cred := range credentials {
			for _, code := range []int{http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
				t.Run(caller.name+"/"+cred.name+"/"+http.StatusText(code), func(t *testing.T) {
					ok := okIfAuthenticated(t)
					var moved atomic.Int32
					srv, hits := countingServer(t, func(w http.ResponseWriter, r *http.Request) {
						if r.URL.Path == "/moved" {
							moved.Add(1)
							ok(w, r)
							return
						}
						http.Redirect(w, r, "/moved", code)
					})
					c, err := openai.New(openai.WithBaseURL(srv.URL), openai.WithModel("m"), cred.opt)
					if err != nil {
						t.Fatal(err)
					}
					if err := caller.call(t.Context(), c); err != nil {
						t.Fatal(err)
					}
					if hits.Load() != 2 || moved.Load() != 1 {
						t.Fatalf("requests = %d, to target = %d, want 2 and 1", hits.Load(), moved.Load())
					}
				})
			}
		}
	}
}

// Same-origin redirects keep net/http's default limit: the client sends exactly as many requests
// as a plain http.Client does before giving up.
func TestSameOriginRedirectLimit(t *testing.T) {
	srv, hits := countingServer(t, redirectTo(http.StatusTemporaryRedirect, "/chat/completions"))
	resp, err := http.Post(srv.URL+"/chat/completions", "application/json", strings.NewReader("{}"))
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("plain client followed a redirect loop")
	}
	want := hits.Swap(0)

	c, err := openai.New(openai.WithBaseURL(srv.URL), openai.WithModel("m"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Model(t.Context(), redirectRequest)
	if err == nil || !strings.Contains(err.Error(), "stopped after 10 redirects") {
		t.Fatalf("err = %v, want the redirect limit", err)
	}
	if got := hits.Load(); got != want {
		t.Fatalf("requests = %d, want %d as with net/http's default policy", got, want)
	}
}

// A caller-supplied client keeps its own redirect policy for the redirects this package allows,
// and is not modified.
func TestWithHTTPClientRedirectPolicy(t *testing.T) {
	var policyCalls atomic.Int32
	policy := func(*http.Request, []*http.Request) error {
		policyCalls.Add(1)
		return nil
	}

	t.Run("same-origin runs the caller's policy", func(t *testing.T) {
		policyCalls.Store(0)
		ok := okIfAuthenticated(t)
		srv, _ := countingServer(t, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/moved" {
				ok(w, r)
				return
			}
			http.Redirect(w, r, "/moved", http.StatusTemporaryRedirect)
		})
		h := &http.Client{CheckRedirect: policy}
		c, err := openai.New(openai.WithBaseURL(srv.URL), openai.WithModel("m"),
			openai.WithHeader("api-key", redirectSecret), openai.WithHTTPClient(h))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.Model(t.Context(), redirectRequest); err != nil {
			t.Fatal(err)
		}
		if got := policyCalls.Load(); got != 1 {
			t.Fatalf("caller's CheckRedirect called %d times, want 1", got)
		}
	})

	t.Run("cross-origin is refused before the caller's policy", func(t *testing.T) {
		policyCalls.Store(0)
		b, bHits := countingServer(t, okIfAuthenticated(t))
		a, _ := countingServer(t, redirectTo(http.StatusTemporaryRedirect, b.URL+"/chat/completions"))
		h := &http.Client{CheckRedirect: policy}
		c, err := openai.New(openai.WithBaseURL(a.URL), openai.WithModel("m"),
			openai.WithHeader("api-key", redirectSecret), openai.WithHTTPClient(h))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.Model(t.Context(), redirectRequest); err == nil {
			t.Fatal("followed a cross-origin redirect")
		}
		if got := bHits.Load(); got != 0 {
			t.Fatalf("redirect target received %d requests, want 0", got)
		}
		if got := policyCalls.Load(); got != 0 {
			t.Fatalf("caller's CheckRedirect called %d times for a refused redirect, want 0", got)
		}

		// h still holds the caller's own policy: called directly with a cross-origin hop, it
		// allows it and counts the call, which the wrapped policy would refuse.
		from, _ := http.NewRequest(http.MethodPost, a.URL, nil)
		to, _ := http.NewRequest(http.MethodPost, b.URL, nil)
		if err := h.CheckRedirect(to, []*http.Request{from}); err != nil || policyCalls.Load() != 1 {
			t.Fatalf("caller's CheckRedirect was replaced: err = %v, calls = %d", err, policyCalls.Load())
		}
	})

	t.Run("a client without a policy is not modified", func(t *testing.T) {
		h := &http.Client{}
		if _, err := openai.New(openai.WithModel("m"), openai.WithHTTPClient(h)); err != nil {
			t.Fatal(err)
		}
		if h.CheckRedirect != nil {
			t.Fatal("New set CheckRedirect on the caller's client")
		}
	})
}

// fakeTransport answers the first request with a 307 to location and every later one with a
// completion, recording each request's URL. It reaches origins httptest cannot listen on (default
// ports, named hosts) without the network.
type fakeTransport struct {
	location string
	mu       sync.Mutex
	urls     []string
}

func (f *fakeTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	f.mu.Lock()
	f.urls = append(f.urls, r.URL.String())
	first := len(f.urls) == 1
	f.mu.Unlock()
	if r.Body != nil {
		_, _ = io.Copy(io.Discard, r.Body)
		_ = r.Body.Close()
	}
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Request: r,
		Body: io.NopCloser(strings.NewReader(completion("ok")))}
	if first {
		resp.StatusCode = http.StatusTemporaryRedirect
		resp.Header.Set("Location", f.location)
		resp.Body = http.NoBody
	}
	return resp, nil
}

// Origin is scheme, host and port: a change to any of them is refused, while host case and an
// explicit default port are not changes.
func TestRedirectOriginComparison(t *testing.T) {
	const base = "https://api.example.com/v1"
	for _, tc := range []struct {
		location string
		follow   bool
	}{
		{"https://api.example.com/v1/moved", true},
		{"https://API.Example.com:443/v1/moved", true},
		{"/v1/moved", true},
		{"http://api.example.com/v1/moved", false},
		{"https://api.example.com:8443/v1/moved", false},
		{"https://other.example.com/v1/moved", false},
		{"https://api.example.com.attacker.test/v1/moved", false},
		{"https://user@attacker.test/v1/moved", false},
	} {
		t.Run(tc.location, func(t *testing.T) {
			ft := &fakeTransport{location: tc.location}
			c, err := openai.New(openai.WithBaseURL(base), openai.WithModel("m"),
				openai.WithHeader("api-key", redirectSecret), openai.WithHTTPClient(&http.Client{Transport: ft}))
			if err != nil {
				t.Fatal(err)
			}
			_, err = c.Model(t.Context(), redirectRequest)
			if tc.follow {
				if err != nil || len(ft.urls) != 2 {
					t.Fatalf("err = %v, requests = %v, want the redirect followed", err, ft.urls)
				}
				return
			}
			if err == nil || len(ft.urls) != 1 {
				t.Fatalf("err = %v, requests = %v, want the redirect refused", err, ft.urls)
			}
			if !strings.Contains(err.Error(), "refusing redirect from https://api.example.com to ") {
				t.Fatalf("err = %v, want it to name the base origin", err)
			}
		})
	}
}
