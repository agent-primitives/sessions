package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"testing"

	atepb "github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/aramase/agentsessions/api"
	"github.com/aramase/agentsessions/examples/agentsuite-substrate/render"
	"github.com/aramase/agentsessions/internal/modelconfig"
)

// check-model refuses what agentsessionsd's model client refuses at startup (model/openai), which
// includes absolute URLs a scheme://host pattern accepts, and a scheme other than http(s). It also
// refuses a URL with userinfo, a query or a fragment, which up.sh would print and render into the
// Deployment, and none of its errors repeats any part of the URL.
func TestCheckModel(t *testing.T) {
	const hidden = "the value is not shown"
	for _, tc := range []struct {
		name, baseURL, model string
		wantErr              string // "" accepts
	}{
		{name: "https", baseURL: "https://api.example.com/v1", model: "m"},
		{name: "Azure OpenAI's v1 endpoint", baseURL: "https://llm.example.openai.azure.com/openai/v1", model: "m"},
		{name: "an api-version on the v1 endpoint", baseURL: "https://llm.example.openai.azure.com/openai/v1?api-version=2025-04-01-preview", model: "m", wantErr: "carries a query"},
		{name: "uppercase scheme with port", baseURL: "HTTP://llm.example:8080/v1", model: "m"},
		{name: "bracketed IPv6", baseURL: "http://[::1]:8080/v1", model: "m"},
		{name: "userinfo", baseURL: "https://user:secret@llm.example/v1", model: "m", wantErr: "carries userinfo"},
		{name: "user only", baseURL: "https://secret@llm.example/v1", model: "m", wantErr: "carries userinfo"},
		{name: "only userinfo for a host", baseURL: "https://secret@/v1", model: "m", wantErr: "carries userinfo"},
		{name: "query", baseURL: "https://llm.example/v1?key=secret", model: "m", wantErr: "carries a query"},
		{name: "empty query", baseURL: "https://llm.example/v1?", model: "m", wantErr: "carries a query"},
		{name: "fragment", baseURL: "https://llm.example/v1#secret", model: "m", wantErr: "carries a fragment"},
		{name: "empty fragment", baseURL: "https://llm.example/v1#", model: "m", wantErr: "carries a fragment"},
		{name: "userinfo with no scheme", baseURL: "user:secret@llm.example/v1", model: "m", wantErr: "is not http or https"},
		{name: "userinfo read as a port", baseURL: "https://user:secret/x@llm.example/v1", model: "m", wantErr: "is not a valid URL"},
		{name: "no scheme", baseURL: "api.example.com/v1", model: "m", wantErr: "is not http or https"},
		{name: "no host", baseURL: "https:///v1", model: "m", wantErr: "has no host"},
		{name: "a port that is not a number", baseURL: "http://host:abc/v1", model: "m", wantErr: "is not a valid URL"},
		{name: "a bad escape in the host", baseURL: "http://h%zz/v1", model: "m", wantErr: "is not a valid URL"},
		{name: "a bad escape in the path", baseURL: "https://llm.example/v%zz", model: "m", wantErr: "is not a valid URL"},
		{name: "another scheme", baseURL: "ftp://llm.example/v1", model: "m", wantErr: "is not http or https"},
		{name: "no base URL", baseURL: "", model: "m", wantErr: "-base-url is required"},
		{name: "no model", baseURL: "https://api.example.com/v1", model: "", wantErr: "a default model is required"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := checkModel([]string{"-base-url", tc.baseURL, "-model", tc.model})
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("err=%v, want it accepted", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Fatalf("err=%v, want one containing %q", err, tc.wantErr)
			}
			if err != nil && tc.baseURL != "" && tc.model != "" {
				msg := err.Error()
				for _, leak := range []string{"secret", "llm.example", "api.example", "abc", "%zz", "/v1"} {
					if strings.Contains(msg, leak) {
						t.Errorf("err=%q repeats %q from the URL", msg, leak)
					}
				}
				if !strings.Contains(msg, hidden) {
					t.Errorf("err=%q does not say the value is hidden", msg)
				}
			}
		})
	}
	if err := checkModel([]string{"-base-url", "https://a.example", "-model", "m", "extra"}); err == nil || !strings.Contains(err.Error(), "unexpected arguments") {
		t.Fatalf("extra argument: err=%v", err)
	}
}

// check-model's -auth-header must be a header name. It has no -path: the daemon keeps its default
// -model-path, which Azure OpenAI's v1 endpoint takes, so a -path is a usage error.
func TestCheckModelAuthHeader(t *testing.T) {
	const azure = "https://res.openai.azure.com/openai/v1"
	for _, tc := range []struct {
		name, header string
		wantErr      string // "" accepts
	}{
		{name: "the default header"},
		{name: "api-key", header: "api-key"},
		{name: "x-api-key", header: "x-api-key"},
		{name: "a header with a space", header: "api key", wantErr: "not an HTTP header name"},
		{name: "a header with a colon", header: "api-key:", wantErr: "not an HTTP header name"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := []string{"-base-url", azure, "-model", "dep"}
			if tc.header != "" {
				args = append(args, "-auth-header", tc.header)
			}
			err := checkModel(args)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("err=%v, want it accepted", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Fatalf("err=%v, want one containing %q", err, tc.wantErr)
			}
		})
	}
	if err := checkModel([]string{"-base-url", azure, "-model", "dep", "-auth-header", ""}); err == nil || !strings.Contains(err.Error(), "not an HTTP header name") {
		t.Fatalf("empty -auth-header: err=%v", err)
	}
	if err := checkModel([]string{"-base-url", azure, "-model", "dep", "-path", "/chat/completions"}); err == nil || !strings.Contains(err.Error(), "flag provided but not defined: -path") {
		t.Fatalf("-path: err=%v, want the retired flag refused", err)
	}
}

// The coordinator binding's base URL is Azure OpenAI's v1 endpoint (<resource>/openai/v1). Built
// with the constructor agentsessionsd uses and the daemon's default -model-path, the client posts to
// <resource>/openai/v1/chat/completions with no query, the deployment as the body's model and the
// key as a bearer token: the request Azure answered with HTTP 200 on 2026-10-15.
func TestV1BaseURLReachesTheCompletionsPath(t *testing.T) {
	b, err := render.LoadBinding("../../binding-coordinator.json")
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(b.Model.BaseURL)
	if err != nil || u.Path != "/openai/v1" {
		t.Fatalf("binding-coordinator.json's model.baseURL path = %q (err %v), want /openai/v1", u.Path, err)
	}
	var gotPath, gotQuery, gotAuth, gotModel string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery, gotAuth = r.URL.Path, r.URL.RawQuery, r.Header.Get("Authorization")
		var body struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotModel = body.Model
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}]}`)
	}))
	t.Cleanup(srv.Close)
	c, err := modelconfig.New(modelconfig.Config{Model: b.Model.Name, BaseURL: srv.URL + u.Path, AuthHeader: "Authorization", APIKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Model(t.Context(), api.ModelRequest{Messages: []api.Message{*api.TextMessage("user", "q")}}); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/openai/v1/chat/completions" || gotQuery != "" || gotModel != "gpt-5.4-mini" || gotAuth != "Bearer k" {
		t.Fatalf("request path=%q query=%q model=%q auth=%q, want /openai/v1/chat/completions, no query, gpt-5.4-mini, Bearer k",
			gotPath, gotQuery, gotModel, gotAuth)
	}
}

// With -api-key-set, check-model refuses a plain-http URL to a host that is not loopback, localhost
// or an in-cluster service, as agentsessionsd does with the key, and names the host; without it, or
// over https, the same URLs pass.
func TestCheckModelRefusesAKeyOverRemotePlainHTTP(t *testing.T) {
	for _, tc := range []struct {
		baseURL string
		keySet  bool
		refuse  string // the host the refusal names; "" accepts
	}{
		{baseURL: "http://llm.example:8000/v1", keySet: true, refuse: "llm.example"},
		{baseURL: "http://10.1.2.3/v1", keySet: true, refuse: "10.1.2.3"},
		{baseURL: "http://ollama.ns.svc:11434/v1", keySet: true},
		{baseURL: "http://vllm.ns.svc.cluster.local:8000/v1", keySet: true},
		{baseURL: "http://127.0.0.1:8082/v1", keySet: true},
		{baseURL: "https://llm.example/v1", keySet: true},
		{baseURL: "http://llm.example:8000/v1"},
	} {
		args := []string{"-base-url", tc.baseURL, "-model", "m"}
		if tc.keySet {
			args = append(args, "-api-key-set")
		}
		err := checkModel(args)
		if tc.refuse == "" {
			if err != nil {
				t.Errorf("check-model %v: err=%v, want it accepted", args, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), `host "`+tc.refuse+`"`) || !strings.Contains(err.Error(), "use https") {
			t.Errorf("check-model %v: err=%v, want a refusal naming host %q and asking for https", args, err, tc.refuse)
		}
	}
}

// validate runs kaimahi's validator over a suite and prints one line naming the suite, its
// composition and the kaimahi module; a suite the validator refuses is an error, not a report.
func TestValidate(t *testing.T) {
	var out strings.Builder
	if err := validate([]string{"-suite", "../../suites/kaimahi-minimal"}, &out); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	if len(lines) != 3 || lines[0] != "suite minimal valid: 1 agent; compositions: writer linux/amd64 profile default" ||
		!strings.HasPrefix(lines[1], "digest sha256:") || len(lines[1]) != len("digest sha256:")+64 ||
		!strings.HasPrefix(lines[2], "validated by github.com/kaimahi-agents/kaimahi/agentsuite ") {
		t.Errorf("validate printed %q, want the suite line, its digest and the validator line", out.String())
	}
	if got, want := validLines("s", "sha256:d", "v1", 2, []string{"a linux/amd64 profile p", "b linux/arm64 profile p"}),
		"suite s valid: 2 agents; compositions: a linux/amd64 profile p; b linux/arm64 profile p\ndigest sha256:d\nvalidated by github.com/kaimahi-agents/kaimahi/agentsuite v1\n"; got != want {
		t.Errorf("validLines for two agents = %q, want %q", got, want)
	}
	// The suite version is the digest of agentsuite.json: the pinned suite has its own.
	digests := map[string]bool{}
	for _, dir := range []string{"../../suites/coordinator-workers", "../../suites/coordinator-workers-v2"} {
		var o strings.Builder
		if err := validate([]string{"-suite", dir}, &o); err != nil {
			t.Fatal(err)
		}
		digests[strings.Split(o.String(), "\n")[1]] = true
	}
	if !digests["digest sha256:2d05b90f3ddef4b72eee99e6fad9f2c1e7ba9b2e160029b3767c9628bf008e70"] ||
		!digests["digest sha256:306685d5375bb021ae1424f610012441b4381d0d49feab3b4c208ac2ced96c60"] {
		t.Errorf("suite digests = %v, want 75f27d6's and the pinned version's (suites/coordinator-workers-v2/README.md)", digests)
	}
	dir := t.TempDir()
	if err := validate([]string{"-suite", dir}, &out); err == nil {
		t.Error("validate accepted a directory with no agentsuite.json")
	}
	if err := validate([]string{"-suite", dir, "extra"}, &out); err == nil || !strings.Contains(err.Error(), "unexpected arguments") {
		t.Errorf("validate with an extra argument: %v", err)
	}
}

func TestDepVersion(t *testing.T) {
	const kaimahi = "github.com/kaimahi-agents/kaimahi"
	for _, tc := range []struct {
		name string
		deps []*debug.Module
		want string
	}{
		{"required", []*debug.Module{{Path: kaimahi, Version: "v0.5.2"}}, "v0.5.2"},
		{"replaced by a version", []*debug.Module{{Path: kaimahi, Version: "v0.5.2", Replace: &debug.Module{Path: "example.com/fork", Version: "v0.6.0"}}}, "v0.6.0"},
		{"replaced by a directory", []*debug.Module{{Path: kaimahi, Version: "v0.5.2", Replace: &debug.Module{Path: "../kaimahi"}}}, "(local replace of v0.5.2)"},
		{"not a dependency", []*debug.Module{{Path: "example.com/other", Version: "v1.0.0"}}, "(unknown)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := depVersion(tc.deps, kaimahi); got != tc.want {
				t.Errorf("depVersion = %q, want %q", got, tc.want)
			}
		})
	}
}

// render writes each file in one fixed form (two-space indent, protojson's field order, a trailing
// newline) that reads back as the same message, so two renders of the same inputs are the same
// bytes and a file can be cited by line.
func TestStableJSON(t *testing.T) {
	m := &atepb.ActorTemplate{
		Metadata: &atepb.ResourceMetadata{Atespace: "a", Name: "n"},
		Containers: []*atepb.Container{{
			Name:  "harness",
			Image: "img@sha256:00",
			Env:   []*atepb.EnvVar{{Name: "K", Value: "v"}},
		}},
		SandboxConfig: &atepb.SandboxConfig{SandboxClass: atepb.SandboxClass_SANDBOX_CLASS_MICROVM, ConfigName: "microvm"},
	}
	got, err := stableJSON(m)
	if err != nil {
		t.Fatal(err)
	}
	const want = `{
  "metadata": {
    "atespace": "a",
    "name": "n"
  },
  "containers": [
    {
      "name": "harness",
      "image": "img@sha256:00",
      "env": [
        {
          "name": "K",
          "value": "v"
        }
      ]
    }
  ],
  "sandboxConfig": {
    "sandboxClass": "SANDBOX_CLASS_MICROVM",
    "configName": "microvm"
  }
}
`
	if string(got) != want {
		t.Errorf("stableJSON =\n%s\nwant\n%s", got, want)
	}
	back := &atepb.ActorTemplate{}
	if err := protojson.Unmarshal(got, back); err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(back, m) {
		t.Errorf("stableJSON does not read back as the message: %v", back)
	}
}

const (
	pinnedSuite   = "../../suites/coordinator-workers-v2"
	originalSuite = "../../suites/coordinator-workers"
	coordBinding  = "../../binding-coordinator.json"
	image         = "localhost:5001/agentsuite-harness@sha256:" + "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
)

// bindingWith writes the coordinator binding with its approval edges replaced by gates
// ("caller->agent"); none drops approval.
func bindingWith(t *testing.T, gates ...string) string {
	t.Helper()
	raw, err := os.ReadFile(coordBinding)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	delete(m, "approval")
	var list []any
	for _, g := range gates {
		caller, agent, _ := strings.Cut(g, "->")
		list = append(list, map[string]any{"caller": caller, "agent": agent})
	}
	if list != nil {
		m["approval"] = map[string]any{"invokes": list}
	}
	out, _ := json.Marshal(m)
	path := filepath.Join(t.TempDir(), "binding.json")
	if err := os.WriteFile(path, out, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// emit render and check-binding refuse a binding that serves another model than the suite pins, and
// accept the suite version that pins it; the render writes the daemon's model flags from the binding.
func TestRenderChecksTheModel(t *testing.T) {
	var out strings.Builder
	if err := checkBinding([]string{"-suite", pinnedSuite, "-binding", coordBinding}, &out); err != nil {
		t.Fatalf("check-binding of the pinned suite: %v", err)
	}
	if !strings.Contains(out.String(), "serves gpt-5.4-mini for coordinator, writer, reviewer") {
		t.Fatalf("check-binding printed %q", out.String())
	}
	if err := checkBinding([]string{"-suite", originalSuite, "-binding", coordBinding}, &out); !errors.Is(err, render.ErrModelMismatch) {
		t.Fatalf("check-binding of 75f27d6's suite: err = %v, want the model mismatch", err)
	}
	if err := checkBinding([]string{"-suite", originalSuite, "-agent", "writer", "-binding", coordBinding}, &out); !errors.Is(err, render.ErrModelMismatch) {
		t.Fatalf("check-binding -agent writer of 75f27d6's suite: err = %v, want the model mismatch", err)
	}
	dir := t.TempDir()
	err := renderCmd([]string{"-suite", originalSuite, "-agent", "coordinator", "-binding", coordBinding, "-image", image, "-out", dir})
	if !errors.Is(err, render.ErrModelMismatch) {
		t.Fatalf("render of 75f27d6's suite: err = %v, want the model mismatch", err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("a refused render wrote %d files", len(entries))
	}
	if err := renderCmd([]string{"-suite", pinnedSuite, "-agent", "coordinator", "-binding", coordBinding, "-image", image, "-out", dir}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "agentsessionsd.model-args"))
	if err != nil {
		t.Fatal(err)
	}
	// No -model-path: the daemon keeps its default, which Azure OpenAI's v1 endpoint takes.
	if want := "-model=gpt-5.4-mini\n-model-base-url=https://example.openai.azure.com/openai/v1\n"; string(got) != want {
		t.Fatalf("agentsessionsd.model-args = %q, want %q", got, want)
	}
	if err := renderCmd([]string{"-suite", pinnedSuite, "-agent", "coordinator", "-model", "gpt-x", "-binding", coordBinding, "-image", image, "-out", dir}); err == nil {
		t.Fatal("render accepted -model: the model is the suite's, not a render-time choice")
	}
}

// The binding chooses the gated edges: emit invokes marks them in AGENTSUITE_INVOKES, emit render
// sets them in the caller's template, and both refuse a gate on an edge the suite does not declare.
func TestBindingDrivesApproval(t *testing.T) {
	for _, tc := range []struct {
		gates []string
		want  string
	}{
		{nil, "coordinator=reviewer:1:1,coordinator=writer:1:1"},
		{[]string{"coordinator->reviewer"}, "coordinator=reviewer:1:1:approval,coordinator=writer:1:1"},
		{[]string{"coordinator->writer", "coordinator->reviewer"}, "coordinator=reviewer:1:1:approval,coordinator=writer:1:1:approval"},
	} {
		var out strings.Builder
		if err := invokesCmd([]string{"-suite", pinnedSuite, "-binding", bindingWith(t, tc.gates...)}, &out); err != nil {
			t.Fatal(err)
		}
		if got := strings.TrimSpace(out.String()); got != tc.want {
			t.Errorf("gates %v: invokes = %q, want %q", tc.gates, got, tc.want)
		}
	}
	var out strings.Builder
	if err := invokesCmd([]string{"-suite", pinnedSuite, "-binding", coordBinding}, &out); err != nil || strings.TrimSpace(out.String()) != "coordinator=reviewer:1:1:approval,coordinator=writer:1:1" {
		t.Fatalf("invokes with the demo's binding = %q (err %v)", out.String(), err)
	}
	if err := invokesCmd([]string{"-suite", pinnedSuite}, &out); err == nil {
		t.Fatal("invokes ran without a binding")
	}
	for _, gate := range []string{"writer->reviewer", "coordinator->publisher"} {
		b := bindingWith(t, gate)
		if err := invokesCmd([]string{"-suite", pinnedSuite, "-binding", b}, &out); err == nil || !strings.Contains(err.Error(), "the suite declares no such invokes edge") {
			t.Errorf("invokes with gate %s: err = %v", gate, err)
		}
		// A worker's render refuses it too, though the bad edge is not the worker's.
		if err := renderCmd([]string{"-suite", pinnedSuite, "-agent", "reviewer", "-binding", b, "-image", image, "-out", t.TempDir()}); err == nil || !strings.Contains(err.Error(), "the suite declares no such invokes edge") {
			t.Errorf("render of reviewer with gate %s: err = %v", gate, err)
		}
	}
	approval := func(gates ...string) string {
		dir := t.TempDir()
		if err := renderCmd([]string{"-suite", pinnedSuite, "-agent", "coordinator", "-binding", bindingWith(t, gates...), "-image", image, "-out", dir}); err != nil {
			t.Fatal(err)
		}
		raw, err := os.ReadFile(filepath.Join(dir, "actortemplate.json"))
		if err != nil {
			t.Fatal(err)
		}
		tmpl := &atepb.ActorTemplate{}
		if err := protojson.Unmarshal(raw, tmpl); err != nil {
			t.Fatal(err)
		}
		for _, e := range tmpl.GetContainers()[0].GetEnv() {
			if e.GetName() == "AGENTSUITE_APPROVAL" {
				return e.GetValue()
			}
		}
		return ""
	}
	if got := approval("coordinator->reviewer"); got != "reviewer" {
		t.Errorf("AGENTSUITE_APPROVAL = %q, want reviewer", got)
	}
	if got := approval(); got != "" {
		t.Errorf("AGENTSUITE_APPROVAL = %q with no approval edges", got)
	}
}
