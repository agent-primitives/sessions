package render

import (
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	atepb "github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"github.com/kaimahi-agents/kaimahi/agentsuite"

	v1 "github.com/aramase/agentsessions/api/genpb"
	"github.com/aramase/agentsessions/examples/agentsuite-substrate/agent"
)

const digestA = "sha256:" + "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func TestTemplateNameIsContentAddressed(t *testing.T) {
	base := TemplateName("support-desk", "triage", digestA, "sha256:b", Options{})
	if !strings.HasPrefix(base, "support-desk-triage-") || len(base) != len("support-desk-triage-")+8 {
		t.Fatalf("name = %q", base)
	}
	if TemplateName("support-desk", "triage", digestA, "sha256:b", Options{}) != base {
		t.Fatal("same inputs must give the same name")
	}
	if TemplateName("support-desk", "triage", digestA, "sha256:c", Options{}) == base {
		t.Fatal("another binding must give another template")
	}
	if TemplateName("support-desk", "triage", "sha256:"+strings.Repeat("c", 64), "sha256:b", Options{}) == base {
		t.Fatal("another image must give another template")
	}
	// No model keeps the original formula, so an existing template keeps its name.
	sum := sha256.Sum256([]byte(digestA + "\n" + "sha256:b"))
	if want := "support-desk-triage-" + hex.EncodeToString(sum[:])[:8]; base != want {
		t.Fatalf("name = %q, want %q", base, want)
	}
	if TemplateName("support-desk", "triage", digestA, "sha256:b", Options{Model: "gpt-x"}) == base || TemplateName("support-desk", "triage", digestA, "sha256:b", Options{Memory: true}) == base {
		t.Fatal("a model override must give another template")
	}
	long := TemplateName(strings.Repeat("Suite_", 20), strings.Repeat("agent.", 20), digestA, "sha256:b", Options{})
	if len(long) > 63 || !dnsLabel.MatchString(long) || long[len(long)-8:] != base[len(base)-8:] {
		t.Fatalf("long name = %q (%d chars): want a DNS label of at most 63 that keeps hash8", long, len(long))
	}
}

func TestRenderMapsTheComposition(t *testing.T) {
	comp, err := Load("../suite", "triage", "linux/arm64")
	if err != nil {
		t.Fatal(err)
	}
	b, err := LoadBinding("../binding.json")
	if err != nil {
		t.Fatal(err)
	}
	image := "localhost:5001/agentsuite-harness@" + digestA
	out, err := Render(comp, image, b, Options{})
	if err != nil {
		t.Fatal(err)
	}
	tmpl := out.Template
	name := TemplateName("support-desk", "triage", digestA, b.Digest(), Options{})
	if tmpl.GetMetadata().GetName() != name || tmpl.GetMetadata().GetAtespace() != "agentsuite-demo" {
		t.Fatalf("metadata = %v, want agentsuite-demo/%s", tmpl.GetMetadata(), name)
	}
	c := tmpl.GetContainers()[0]
	if c.GetImage() != image || c.GetWakeupProbe().GetHttpGet().GetPort() != ReadyzPort || c.GetWakeupProbe().GetHttpGet().GetPath() != "/readyz" {
		t.Fatalf("container = %v", c)
	}
	env := map[string]string{}
	for _, e := range c.GetEnv() {
		env[e.GetName()] = e.GetValue()
	}
	if _, ok := env[agent.ModelEnv]; ok {
		t.Fatalf("env = %v: without a model override the harness must keep the agent's model", env)
	}
	if env["HARNESS_ADDR"] != ":80" || env["AGENT_DIR"] != AgentDir {
		t.Fatalf("env = %v, want harnesswire on :80", env)
	}
	for name := range env {
		if strings.Contains(strings.ToUpper(name), "KEY") || strings.Contains(strings.ToUpper(name), "TOKEN") {
			t.Fatalf("model credentials must stay with the host, found env %s", name)
		}
	}
	if tmpl.GetSandboxConfig().GetSandboxClass() != atepb.SandboxClass_SANDBOX_CLASS_GVISOR || tmpl.GetSandboxConfig().GetConfigName() != "gvisor-default" {
		t.Fatalf("sandbox = %v", tmpl.GetSandboxConfig())
	}
	if tmpl.GetSnapshotConfig().GetPreferredFidelity() != atepb.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY ||
		tmpl.GetSnapshotConfig().GetStorageLocation() != "gs://ate-snapshots/agentsuite-demo/"+name+"/" {
		t.Fatalf("snapshot = %v", tmpl.GetSnapshotConfig())
	}
	if tmpl.GetWorkerSelector().GetMatchLabels()["workload"] != "agentsuite-demo-gvisor" {
		t.Fatalf("worker selector = %v", tmpl.GetWorkerSelector())
	}
	sub := out.Registration.GetSpec().GetSubstrate()
	if out.Registration.GetMetadata().GetName() != "triage" || sub.GetAtespace() != "agentsuite-demo" || sub.GetTemplate() != name ||
		out.Registration.GetSpec().GetCapabilities().GetResumability() != v1.Resumability_RESUMABILITY_STATELESS_REPLAY {
		t.Fatalf("registration = %v", out.Registration)
	}
	if out.DaemonFlag != "triage=agentsuite-demo/"+name {
		t.Fatalf("daemon flag = %q", out.DaemonFlag)
	}
	if _, err := Render(comp, "localhost:5001/agentsuite-harness:latest", b, Options{}); err == nil {
		t.Fatal("a tag-only image must be refused: substrate requires a digest-pinned reference")
	}
}

// A real provider serves its own model IDs, not the fixture's. The override reaches the harness
// through the template's env, and the template name changes with it, so a rerun with another model
// creates another template instead of reusing one whose harness requests the old model.
func TestRenderOverridesTheModel(t *testing.T) {
	comp, err := Load("../suite", "triage", "linux/arm64")
	if err != nil {
		t.Fatal(err)
	}
	b, err := LoadBinding("../binding.json")
	if err != nil {
		t.Fatal(err)
	}
	image := "localhost:5001/agentsuite-harness@" + digestA
	out, err := Render(comp, image, b, Options{Model: "gpt-x"})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range out.Template.GetContainers()[0].GetEnv() {
		if e.GetName() == agent.ModelEnv {
			got = append(got, e.GetValue())
		}
	}
	if len(got) != 1 || got[0] != "gpt-x" {
		t.Fatalf("%s in the template env = %v, want [gpt-x]", agent.ModelEnv, got)
	}
	name := TemplateName("support-desk", "triage", digestA, b.Digest(), Options{Model: "gpt-x"})
	if out.Template.GetMetadata().GetName() != name || out.DaemonFlag != "triage=agentsuite-demo/"+name {
		t.Fatalf("template %q, daemon flag %q: want both on %s", out.Template.GetMetadata().GetName(), out.DaemonFlag, name)
	}
}

// The memory tier selects the RAM transcript in the template env, gets its own template name, and is
// registered as REQUIRES_MEMORY_SNAPSHOT under <agent>-memory with the daemon flag's ,memory option.
func TestRenderMemoryTier(t *testing.T) {
	comp, err := Load("../suite", "triage", "linux/amd64")
	if err != nil {
		t.Fatal(err)
	}
	b, err := LoadBinding("../binding.json")
	if err != nil {
		t.Fatal(err)
	}
	image := "localhost:5001/agentsuite-harness@" + digestA
	out, err := Render(comp, image, b, Options{Memory: true})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range out.Template.GetContainers()[0].GetEnv() {
		if e.GetName() == agent.MemoryEnv {
			got = append(got, e.GetValue())
		}
	}
	if len(got) != 1 || got[0] != agent.MemoryRAM {
		t.Fatalf("%s in the template env = %v, want [%s]", agent.MemoryEnv, got, agent.MemoryRAM)
	}
	name := TemplateName("support-desk", "triage", digestA, b.Digest(), Options{Memory: true})
	if name == TemplateName("support-desk", "triage", digestA, b.Digest(), Options{}) {
		t.Fatal("memory and stateless tiers share a template name")
	}
	if out.Template.GetMetadata().GetName() != name || out.DaemonFlag != "triage-memory=agentsuite-demo/"+name+",memory" {
		t.Fatalf("template %q, daemon flag %q", out.Template.GetMetadata().GetName(), out.DaemonFlag)
	}
	reg := out.Registration
	if reg.GetMetadata().GetName() != "triage-memory" || reg.GetSpec().GetDescriptorId() != "triage" ||
		reg.GetSpec().GetCapabilities().GetResumability() != v1.Resumability_RESUMABILITY_REQUIRES_MEMORY_SNAPSHOT {
		t.Fatalf("registration = %v", reg)
	}
}

func TestLoadSelectsAPlatform(t *testing.T) {
	if _, err := Load("../suite", "triage", "linux/s390x"); err == nil || !strings.Contains(err.Error(), "no composition") {
		t.Fatalf("err = %v, want no composition", err)
	}
	if _, err := Load("../suite", "nobody", "linux/amd64"); err == nil {
		t.Fatal("unknown agent must be refused")
	}
}

// The renderer takes kaimahi's own fixtures as they are: the minimal single-agent suite renders, and
// the coordinator, which invokes other agents, is refused by this tool-free v0.
func TestKaimahiFixtures(t *testing.T) {
	out, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}", "github.com/kaimahi-agents/kaimahi").Output()
	if err != nil {
		t.Skipf("locate the kaimahi module: %v", err)
	}
	testdata := filepath.Join(strings.TrimSpace(string(out)), "agentsuite", "testdata")
	if _, err := os.Stat(testdata); err != nil {
		t.Skipf("kaimahi testdata not in the module cache: %v", err)
	}
	comp, err := Load(filepath.Join(testdata, "minimal"), "writer", "linux/amd64")
	if err != nil {
		t.Fatal(err)
	}
	if comp.Suite != "minimal" || string(comp.Instructions) != "Write clearly.\n" {
		t.Fatalf("composition = %+v", comp)
	}
	if coord, err := Load(filepath.Join(testdata, "coordinator-workers"), "coordinator", "linux/amd64"); err != nil || len(coord.Agent.Invokes) != 2 {
		t.Fatalf("coordinator: %+v, %v; want it loaded with its two invokes edges", coord, err)
	}
}

// The vendored coordinator suite renders three templates. The coordinator's carries the approval
// targets in its env and in its name; each daemon flag names its own actor atespace.
func TestRenderCoordinatorSuite(t *testing.T) {
	const dir = "../suites/coordinator-workers"
	b, err := LoadBinding("../binding-microvm.json")
	if err != nil {
		t.Fatal(err)
	}
	image := "localhost:5001/agentsuite-harness@" + digestA
	names := map[string]bool{}
	for _, id := range []string{"coordinator", "writer", "reviewer"} {
		comp, err := Load(dir, id, "linux/amd64")
		if err != nil {
			t.Fatal(err)
		}
		o := Options{Model: "gpt-5.4-mini", ActorAtespace: "agentsuite-" + id}
		if id == "coordinator" {
			o.Approval = []string{"reviewer"}
		}
		out, err := Render(comp, image, b, o)
		if err != nil {
			t.Fatal(err)
		}
		name := out.Template.GetMetadata().GetName()
		names[name] = true
		if want := id + "=" + b.Atespace + "/" + name + ",actors=agentsuite-" + id; out.DaemonFlag != want {
			t.Fatalf("%s: daemon flag = %q, want %q", id, out.DaemonFlag, want)
		}
		var approval string
		for _, e := range out.Template.GetContainers()[0].GetEnv() {
			if e.GetName() == agent.ApprovalEnv {
				approval = e.GetValue()
			}
		}
		if id == "coordinator" && approval != "reviewer" || id != "coordinator" && approval != "" {
			t.Fatalf("%s: %s = %q", id, agent.ApprovalEnv, approval)
		}
	}
	if len(names) != 3 {
		t.Fatalf("template names = %v, want three", names)
	}
	comp, err := Load(dir, "coordinator", "linux/amd64")
	if err != nil {
		t.Fatal(err)
	}
	plain, _ := Render(comp, image, b, Options{Model: "gpt-5.4-mini"})
	gated, _ := Render(comp, image, b, Options{Model: "gpt-5.4-mini", Approval: []string{"reviewer"}})
	if plain.Template.GetMetadata().GetName() == gated.Template.GetMetadata().GetName() {
		t.Fatal("the approval choice must change the template name")
	}
	// The order and repeats of the targets change neither the name nor the env.
	ab, _ := Render(comp, image, b, Options{Approval: []string{"reviewer", "writer"}})
	ba, _ := Render(comp, image, b, Options{Approval: []string{"writer", "reviewer", "writer"}})
	if ab.Template.GetMetadata().GetName() != ba.Template.GetMetadata().GetName() {
		t.Fatalf("template names differ by approval order: %s, %s", ab.Template.GetMetadata().GetName(), ba.Template.GetMetadata().GetName())
	}
	for _, out := range []*Output{ab, ba} {
		var got string
		for _, e := range out.Template.GetContainers()[0].GetEnv() {
			if e.GetName() == agent.ApprovalEnv {
				got = e.GetValue()
			}
		}
		if got != "reviewer,writer" {
			t.Fatalf("%s = %q, want reviewer,writer", agent.ApprovalEnv, got)
		}
	}
	for _, tc := range []struct {
		o    Options
		want string
	}{
		{Options{Approval: []string{"publisher"}}, `approval for "publisher": agent coordinator has no invokes edge to it`},
		{Options{Memory: true}, "delegations need the approval-capable stateless tier"},
		{Options{ActorAtespace: "Bad_Space"}, "is not a DNS label"},
	} {
		if _, err := Render(comp, image, b, tc.o); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("Render(%+v): err = %v, want %q", tc.o, err, tc.want)
		}
	}
	withTools := *comp
	withTools.Agent.ToolProviders = []agentsuite.ToolProviderRequirement{{}}
	if _, err := Render(&withTools, image, b, Options{}); err == nil || !strings.Contains(err.Error(), "tool providers") {
		t.Fatalf("tool providers: err = %v", err)
	}
}

// Each vendored coordinator-workers file has the git blob id and sha256 SOURCE records for kaimahi
// 75f27d6.
func TestVendoredKaimahiCoordinatorWorkers(t *testing.T) {
	const dir = "../suites/coordinator-workers"
	src, err := os.ReadFile(filepath.Join(dir, "SOURCE"))
	if err != nil {
		t.Fatal(err)
	}
	files := 0
	for _, line := range strings.Split(strings.TrimSpace(string(src)), "\n") {
		f := strings.Fields(line)
		if len(f) != 3 || strings.HasPrefix(line, "#") {
			continue
		}
		files++
		b, err := os.ReadFile(filepath.Join(dir, f[2]))
		if err != nil {
			t.Fatal(err)
		}
		blob := sha1.Sum( //nolint:gosec // a git blob id is SHA-1
			append([]byte(fmt.Sprintf("blob %d\x00", len(b))), b...))
		if hex.EncodeToString(blob[:]) != f[0] {
			t.Errorf("%s: git blob id differs from SOURCE", f[2])
		}
		if sum := sha256.Sum256(b); hex.EncodeToString(sum[:]) != f[1] {
			t.Errorf("%s: sha256 differs from SOURCE", f[2])
		}
	}
	if files != 15 {
		t.Fatalf("SOURCE lists %d files, want 15", files)
	}
}

func TestLoadBindingIsStrict(t *testing.T) {
	path := filepath.Join(t.TempDir(), "b.json")
	if err := os.WriteFile(path, []byte(`{"apiVersion":"kmx.kaimahi.dev/v1alpha1","kind":"AgentSessionsBinding","atespace":"a","placement":{"backend":"substrate"},"snapshot":{"storageLocation":"gs://x/"},"secret":"x"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadBinding(path); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("err = %v, want unknown fields refused", err)
	}
}

// The v0 renderer refuses an agent the host cannot serve: a model protocol other than the
// OpenAI-compatible one agentsessionsd's -model speaks, or an id that becomes a harness name the
// daemon reserves for its built-ins. Render checks a hand-built Composition the same way Load does.
func TestRenderRefusesUnsupportedAgents(t *testing.T) {
	comp, err := Load("../suite", "triage", "linux/arm64")
	if err != nil {
		t.Fatal(err)
	}
	b, err := LoadBinding("../binding.json")
	if err != nil {
		t.Fatal(err)
	}
	image := "localhost:5001/agentsuite-harness@" + digestA
	for _, tc := range []struct {
		name    string
		mutate  func(c *Composition)
		wantErr string
	}{
		{"anthropic protocol", func(c *Composition) { c.Agent.Model.Protocol = "anthropic" }, `model protocol "anthropic"`},
		// The AgentSuite schema requires a protocol (minLength 1), so an empty one never validates;
		// the renderer refuses it too rather than assume a default.
		{"empty protocol", func(c *Composition) { c.Agent.Model.Protocol = "" }, `model protocol ""`},
		{"chat id", func(c *Composition) { c.Agent.ID = "chat" }, `agent id "chat" is the name of a harness agentsessionsd serves itself`},
		{"echo id", func(c *Composition) { c.Agent.ID = "echo" }, `agent id "echo" is the name of a harness agentsessionsd serves itself`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := *comp
			tc.mutate(&c)
			if err := supported(c.Agent); err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("supported: err = %v, want %q", err, tc.wantErr)
			}
			if _, err := Render(&c, image, b, Options{}); err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Render: err = %v, want %q", err, tc.wantErr)
			}
		})
	}
	if err := supported(comp.Agent); err != nil {
		t.Fatalf("the demo's triage agent must be supported: %v", err)
	}
}

// The vendored copy of kaimahi's minimal suite loads, and each file still has the sha256 SOURCE
// records for kaimahi 9c5c993 (the demo driver also checks the git blob ids).
func TestVendoredKaimahiMinimal(t *testing.T) {
	const dir = "../suites/kaimahi-minimal"
	comp, err := Load(dir, "writer", "linux/amd64")
	if err != nil {
		t.Fatal(err)
	}
	if comp.Agent.Model.Model != "example-model" {
		t.Fatalf("model = %q, want the fixture's example-model", comp.Agent.Model.Model)
	}
	src, err := os.ReadFile(filepath.Join(dir, "SOURCE"))
	if err != nil {
		t.Fatal(err)
	}
	files := 0
	for _, line := range strings.Split(strings.TrimSpace(string(src)), "\n") {
		f := strings.Fields(line)
		if len(f) != 3 || strings.HasPrefix(line, "#") {
			continue
		}
		files++
		b, err := os.ReadFile(filepath.Join(dir, f[2]))
		if err != nil {
			t.Fatal(err)
		}
		if sum := sha256.Sum256(b); hex.EncodeToString(sum[:]) != f[1] {
			t.Errorf("%s: sha256 differs from SOURCE", f[2])
		}
	}
	if files != 6 {
		t.Fatalf("SOURCE lists %d files, want 6", files)
	}
}
