package render

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	atepb "github.com/agent-substrate/substrate/pkg/proto/ateapipb"

	v1 "github.com/aramase/agentsessions/api/genpb"
	"github.com/aramase/agentsessions/examples/agentsuite-substrate/agent"
)

const digestA = "sha256:" + "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func TestTemplateNameIsContentAddressed(t *testing.T) {
	base := TemplateName("support-desk", "triage", digestA, "sha256:b", "")
	if !strings.HasPrefix(base, "support-desk-triage-") || len(base) != len("support-desk-triage-")+8 {
		t.Fatalf("name = %q", base)
	}
	if TemplateName("support-desk", "triage", digestA, "sha256:b", "") != base {
		t.Fatal("same inputs must give the same name")
	}
	if TemplateName("support-desk", "triage", digestA, "sha256:c", "") == base {
		t.Fatal("another binding must give another template")
	}
	if TemplateName("support-desk", "triage", "sha256:"+strings.Repeat("c", 64), "sha256:b", "") == base {
		t.Fatal("another image must give another template")
	}
	// No model keeps the original formula, so an existing template keeps its name.
	sum := sha256.Sum256([]byte(digestA + "\n" + "sha256:b"))
	if want := "support-desk-triage-" + hex.EncodeToString(sum[:])[:8]; base != want {
		t.Fatalf("name = %q, want %q", base, want)
	}
	if TemplateName("support-desk", "triage", digestA, "sha256:b", "gpt-x") == base {
		t.Fatal("a model override must give another template")
	}
	long := TemplateName(strings.Repeat("Suite_", 20), strings.Repeat("agent.", 20), digestA, "sha256:b", "")
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
	out, err := Render(comp, image, b, "")
	if err != nil {
		t.Fatal(err)
	}
	tmpl := out.Template
	name := TemplateName("support-desk", "triage", digestA, b.Digest(), "")
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
	if _, err := Render(comp, "localhost:5001/agentsuite-harness:latest", b, ""); err == nil {
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
	out, err := Render(comp, image, b, "gpt-x")
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
	name := TemplateName("support-desk", "triage", digestA, b.Digest(), "gpt-x")
	if out.Template.GetMetadata().GetName() != name || out.DaemonFlag != "triage=agentsuite-demo/"+name {
		t.Fatalf("template %q, daemon flag %q: want both on %s", out.Template.GetMetadata().GetName(), out.DaemonFlag, name)
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
	if _, err := Load(filepath.Join(testdata, "coordinator-workers"), "coordinator", "linux/amd64"); err == nil || !strings.Contains(err.Error(), "tool-free") {
		t.Fatalf("coordinator: err = %v, want the v0 refusal", err)
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
