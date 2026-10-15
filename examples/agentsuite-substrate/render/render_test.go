package render

import (
	"bytes"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
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

const imageA = "localhost:5001/agentsuite-harness@" + digestA

func TestTemplateNameIsContentAddressed(t *testing.T) {
	base := TemplateName("support-desk", "triage", imageA, "sha256:b", Options{})
	if !strings.HasPrefix(base, "support-desk-triage-") || len(base) != len("support-desk-triage-")+8 {
		t.Fatalf("name = %q", base)
	}
	if TemplateName("support-desk", "triage", imageA, "sha256:b", Options{}) != base {
		t.Fatal("same inputs must give the same name")
	}
	if TemplateName("support-desk", "triage", imageA, "sha256:c", Options{}) == base {
		t.Fatal("another binding must give another template")
	}
	if TemplateName("support-desk", "triage", "localhost:5001/agentsuite-harness@sha256:"+strings.Repeat("c", 64), "sha256:b", Options{}) == base {
		t.Fatal("another image must give another template")
	}
	// The image reference, not only its digest, is a spec input: a mirror of the same bits is another
	// template, so a render never reuses a template that pulls from the old registry.
	if TemplateName("support-desk", "triage", "mirror.example/agentsuite-harness@"+digestA, "sha256:b", Options{}) == base {
		t.Fatal("the same digest under another registry must give another template")
	}
	// The formula is kmx's: sha256(renderer version + "\n" + image reference + "\n" + binding digest).
	sum := sha256.Sum256([]byte(RendererVersion + "\n" + imageA + "\n" + "sha256:b"))
	if want := "support-desk-triage-" + hex.EncodeToString(sum[:])[:8]; base != want {
		t.Fatalf("name = %q, want %q", base, want)
	}
	if templateName("agentsessions-poc.actortemplate/v1", "support-desk", "triage", imageA, "sha256:b", Options{}) == base {
		t.Fatal("another renderer version must give another template")
	}
	// Given kmx's renderer version, the formula gives the names kmx's TemplateName gives: these
	// values came from kaimahi internal/kmx/agentsessions.TemplateName at KmxCommit on these inputs.
	digestB := "sha256:" + strings.Repeat("b", 64)
	for image, want := range map[string]string{
		imageA: "support-desk-triage-5b6c8065",
		"mirror.example/agentsuite-harness@" + digestA: "support-desk-triage-539aba30",
	} {
		if got := templateName("kmx.agentsessions-actortemplate/v2", "support-desk", "triage", image, digestB, Options{}); got != want {
			t.Errorf("with kmx's renderer version, image %s: name = %q, want kmx's %s", image, got, want)
		}
	}
	if TemplateName("support-desk", "triage", imageA, "sha256:b", Options{Memory: true}) == base {
		t.Fatal("the memory tier must give another template")
	}
	long := TemplateName(strings.Repeat("Suite_", 20), strings.Repeat("agent.", 20), imageA, "sha256:b", Options{})
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
	image := imageA
	out, err := Render(comp, image, b, Options{})
	if err != nil {
		t.Fatal(err)
	}
	tmpl := out.Template
	name := TemplateName("support-desk", "triage", image, b.Digest(), Options{})
	if tmpl.GetMetadata().GetName() != name || tmpl.GetMetadata().GetAtespace() != "agentsuite-demo" {
		t.Fatalf("metadata = %v, want agentsuite-demo/%s", tmpl.GetMetadata(), name)
	}
	// The binding's resources size the actor: template-level limits, as kmx renders them, which
	// substrate books and sizes the sandbox by; the container sets none.
	limits := map[string]string{}
	for _, l := range tmpl.GetResources().GetLimits() {
		limits[l.GetName()] = l.GetQuantity()
	}
	if len(limits) != 2 || limits["cpu"] != b.Resources.CPU || limits["memory"] != b.Resources.Memory {
		t.Fatalf("template resources = %v, want the binding's cpu %s and memory %s", tmpl.GetResources(), b.Resources.CPU, b.Resources.Memory)
	}
	if r := tmpl.GetContainers()[0].GetResources(); r != nil {
		t.Fatalf("container resources = %v, want none", r)
	}
	c := tmpl.GetContainers()[0]
	if c.GetImage() != image || c.GetWakeupProbe().GetHttpGet().GetPort() != ReadyzPort || c.GetWakeupProbe().GetHttpGet().GetPath() != "/readyz" {
		t.Fatalf("container = %v", c)
	}
	env := map[string]string{}
	for _, e := range c.GetEnv() {
		env[e.GetName()] = e.GetValue()
	}
	if _, ok := env[agent.RetiredModelEnv]; ok {
		t.Fatalf("env = %v: the harness requests the model its agent pins, with no override", env)
	}
	if got := strings.Join(out.ModelFlags, " "); got != "-model=triage-model -model-base-url=http://fakemodel.agentsuite-demo.svc:8080/v1" {
		t.Fatalf("model flags = %q, want the binding's model and base URL", got)
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
	long := "localhost:5001/" + strings.Repeat("a", maxImageReference) + "@" + digestA
	for _, image := range []string{
		long, // over substrate's 512 bytes
		"localhost:5001/agentsuite-harness:v1@" + digestA, // a tag beside the digest
		"localhost:5001/Agentsuite-Harness@" + digestA,    // not a reference: uppercase repository
		"localhost:5001/agentsuite-harness@sha512:" + strings.Repeat("a", 128),
		" " + imageA,
	} {
		if _, err := Render(comp, image, b, Options{}); err == nil || !strings.Contains(err.Error(), "digest-pinned reference (name@sha256:<64 hex>) without a tag, of at most 512 bytes") {
			t.Fatalf("image %q: err = %v, want refused as kmx refuses it", image, err)
		} else if strings.Contains(err.Error(), strings.TrimSpace(image)) {
			t.Fatalf("err = %q repeats the image, which may hold a credential", err)
		}
	}
}

// writeBinding writes the binding at src with edit applied to its JSON object, and returns the path.
func writeBinding(t *testing.T, src string, edit func(m map[string]any)) string {
	t.Helper()
	raw, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	edit(m)
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "binding.json")
	if err := os.WriteFile(path, out, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// The suite pins the model and the binding says where it is served: a render refuses a binding
// that serves any other model, compared exactly. The coordinator binding serves gpt-5.4-mini, which
// the new suite version pins and kaimahi's original (example-model) does not.
func TestRenderRefusesAModelTheBindingDoesNotServe(t *testing.T) {
	image := "localhost:5001/agentsuite-harness@" + digestA
	b, err := LoadBinding("../binding-coordinator.json")
	if err != nil {
		t.Fatal(err)
	}
	pinned, err := Load("../suites/coordinator-workers-v2", "writer", "linux/amd64")
	if err != nil {
		t.Fatal(err)
	}
	out, err := Render(pinned, image, b, Options{})
	if err != nil {
		t.Fatalf("the suite pins gpt-5.4-mini and the binding serves it: %v", err)
	}
	if got := strings.Join(out.ModelFlags, " "); got != "-model=gpt-5.4-mini -model-base-url=https://example.openai.azure.com -model-path=/openai/deployments/gpt-5.4-mini/chat/completions?api-version=2025-04-01-preview" {
		t.Fatalf("model flags = %q", got)
	}
	for _, e := range out.Template.GetContainers()[0].GetEnv() {
		if e.GetName() == agent.RetiredModelEnv {
			t.Fatalf("the template sets %s; the harness must request the pinned model", agent.RetiredModelEnv)
		}
	}

	original, err := Load("../suites/coordinator-workers", "writer", "linux/amd64")
	if err != nil {
		t.Fatal(err)
	}
	_, err = Render(original, image, b, Options{})
	var mismatch *ModelMismatchError
	if !errors.Is(err, ErrModelMismatch) || !errors.As(err, &mismatch) || mismatch.Pinned != "example-model" || mismatch.Served != "gpt-5.4-mini" {
		t.Fatalf("err = %v, want the model mismatch naming both models", err)
	}
	if want := `suite coordinator-workers agent writer pins model "example-model" but the binding serves "gpt-5.4-mini"; a different model needs a new suite version and a new evaluation`; err.Error() != want {
		t.Fatalf("err = %q, want %q", err, want)
	}
	// No case folding, prefix or version matching.
	for _, served := range []string{"GPT-5.4-mini", "gpt-5.4", "gpt-5.4-mini-2026-01-01"} {
		other, err := LoadBinding(writeBinding(t, "../binding-coordinator.json", func(m map[string]any) { m["model"].(map[string]any)["name"] = served }))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := Render(pinned, image, other, Options{}); !errors.Is(err, ErrModelMismatch) {
			t.Fatalf("served %q: err = %v, want the model mismatch", served, err)
		}
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
	name := TemplateName("support-desk", "triage", image, b.Digest(), Options{Memory: true})
	if name == TemplateName("support-desk", "triage", image, b.Digest(), Options{}) {
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

// The memory tier keeps the conversation only in RAM, and substrate cold-boots a ROOTFS or VOLUMES
// snapshot on resume, so the memory tier refuses either fidelity, which the stateless tier (rebuilt
// from the journal) takes.
func TestRenderMemoryTierNeedsMemorySnapshots(t *testing.T) {
	comp, err := Load("../suite", "triage", "linux/amd64")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ file, fidelity string }{
		{"../binding-microvm.json", "ROOTFS"},
		{"../binding-microvm.json", "VOLUMES"},
		{"../binding.json", "VOLUMES"},
	} {
		t.Run(tc.fidelity+" "+filepath.Base(tc.file), func(t *testing.T) {
			b, err := LoadBinding(writeBinding(t, tc.file, func(m map[string]any) { m["snapshot"].(map[string]any)["fidelity"] = tc.fidelity }))
			if err != nil {
				t.Fatalf("the binding is valid: %v", err)
			}
			out, err := Render(comp, imageA, b, Options{Memory: true})
			if want := "the memory tier needs snapshot.fidelity MEMORY, the binding gives " + tc.fidelity; err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("memory tier: out %v, err = %v, want %q", out, err, want)
			}
			out, err = Render(comp, imageA, b, Options{})
			if err != nil {
				t.Fatalf("stateless tier: %v", err)
			}
			if got, want := out.Template.GetSnapshotConfig().GetPreferredFidelity().String(), "SNAPSHOT_FIDELITY_"+tc.fidelity; got != want {
				t.Fatalf("stateless tier fidelity = %s, want %s", got, want)
			}
		})
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

// The pinned coordinator suite renders three templates from one binding. The binding's approval edge
// coordinator -> reviewer reaches only the coordinator's template env; each daemon flag names its
// own actor atespace.
func TestRenderCoordinatorSuite(t *testing.T) {
	const dir = "../suites/coordinator-workers-v2"
	b, err := LoadBinding("../binding-coordinator.json")
	if err != nil {
		t.Fatal(err)
	}
	image := "localhost:5001/agentsuite-harness@" + digestA
	approvalEnv := func(out *Output) string {
		for _, e := range out.Template.GetContainers()[0].GetEnv() {
			if e.GetName() == agent.ApprovalEnv {
				return e.GetValue()
			}
		}
		return ""
	}
	names := map[string]bool{}
	for _, id := range []string{"coordinator", "writer", "reviewer"} {
		comp, err := Load(dir, id, "linux/amd64")
		if err != nil {
			t.Fatal(err)
		}
		out, err := Render(comp, image, b, Options{ActorAtespace: "agentsuite-" + id})
		if err != nil {
			t.Fatal(err)
		}
		name := out.Template.GetMetadata().GetName()
		names[name] = true
		if want := id + "=" + b.Atespace + "/" + name + ",actors=agentsuite-" + id; out.DaemonFlag != want {
			t.Fatalf("%s: daemon flag = %q, want %q", id, out.DaemonFlag, want)
		}
		if approval := approvalEnv(out); id == "coordinator" && approval != "reviewer" || id != "coordinator" && approval != "" {
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
	gates := func(edges ...[2]string) func(m map[string]any) {
		return func(m map[string]any) {
			var list []any
			for _, e := range edges {
				list = append(list, map[string]any{"caller": e[0], "agent": e[1]})
			}
			if list == nil {
				delete(m, "approval")
				return
			}
			m["approval"] = map[string]any{"invokes": list}
		}
	}
	render := func(edit func(m map[string]any)) (*Output, *Binding) {
		t.Helper()
		bb, err := LoadBinding(writeBinding(t, "../binding-coordinator.json", edit))
		if err != nil {
			t.Fatal(err)
		}
		out, err := Render(comp, image, bb, Options{})
		if err != nil {
			t.Fatal(err)
		}
		return out, bb
	}
	plain, _ := render(gates())
	gated, _ := render(gates([2]string{"coordinator", "reviewer"}))
	if approvalEnv(plain) != "" || plain.Template.GetMetadata().GetName() == gated.Template.GetMetadata().GetName() {
		t.Fatal("the binding's approval edges must reach the template env and change the template name")
	}
	// The edges are a set: their order changes neither the binding digest nor the env.
	ab, bab := render(gates([2]string{"coordinator", "reviewer"}, [2]string{"coordinator", "writer"}))
	ba, bba := render(gates([2]string{"coordinator", "writer"}, [2]string{"coordinator", "reviewer"}))
	if bab.Digest() != bba.Digest() || ab.Template.GetMetadata().GetName() != ba.Template.GetMetadata().GetName() {
		t.Fatalf("binding digests differ by approval order: %s, %s", bab.Digest(), bba.Digest())
	}
	if approvalEnv(ab) != "reviewer,writer" || approvalEnv(ba) != "reviewer,writer" {
		t.Fatalf("%s = %q and %q, want reviewer,writer", agent.ApprovalEnv, approvalEnv(ab), approvalEnv(ba))
	}
	// A gate on another caller's edge does not reach this agent's template.
	other, _ := render(gates([2]string{"writer", "reviewer"}))
	if approvalEnv(other) != "" {
		t.Fatalf("%s = %q for a gate whose caller is writer", agent.ApprovalEnv, approvalEnv(other))
	}
	publisher, err := LoadBinding(writeBinding(t, "../binding-coordinator.json", gates([2]string{"coordinator", "publisher"})))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		b    *Binding
		o    Options
		want string
	}{
		{publisher, Options{}, `approval for "publisher": agent coordinator has no invokes edge to it`},
		{b, Options{Memory: true}, "delegations need the approval-capable stateless tier"},
		{b, Options{ActorAtespace: "Bad_Space"}, "is not a DNS label"},
	} {
		if _, err := Render(comp, image, tc.b, tc.o); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("Render(%+v): err = %v, want %q", tc.o, err, tc.want)
		}
	}
	withTools := *comp
	withTools.Agent.ToolProviders = []agentsuite.ToolProviderRequirement{{}}
	if _, err := Render(&withTools, image, b, Options{}); err == nil || !strings.Contains(err.Error(), "tool providers") {
		t.Fatalf("tool providers: err = %v", err)
	}
}

// The new suite version differs from kaimahi 75f27d6 only as its README says: the three agents pin
// gpt-5.4-mini instead of example-model, and agentsuite.json carries their recomputed digests.
// SOURCE records each file's git blob id and sha256.
func TestCoordinatorWorkersV2(t *testing.T) {
	const v1, v2 = "../suites/coordinator-workers", "../suites/coordinator-workers-v2"
	checkSource(t, v2, 15)
	changed := map[string]bool{"agents/coordinator.json": true, "agents/writer.json": true, "agents/reviewer.json": true, "agentsuite.json": true}
	digests := map[string]string{}
	for _, id := range []string{"coordinator", "writer", "reviewer"} {
		for _, dir := range []string{v1, v2} {
			raw, err := os.ReadFile(filepath.Join(dir, "agents", id+".json"))
			if err != nil {
				t.Fatal(err)
			}
			d, err := agentsuite.Digest(raw)
			if err != nil {
				t.Fatal(err)
			}
			digests[dir+id] = d
		}
	}
	err := filepath.WalkDir(v1, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || d.Name() == "SOURCE" {
			return err
		}
		rel, _ := filepath.Rel(v1, path)
		old, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		cur, err := os.ReadFile(filepath.Join(v2, rel))
		if err != nil {
			return err
		}
		if !changed[filepath.ToSlash(rel)] {
			if string(cur) != string(old) {
				t.Errorf("%s differs from 75f27d6", rel)
			}
			return nil
		}
		back := strings.ReplaceAll(string(cur), `"model": "gpt-5.4-mini"`, `"model": "example-model"`)
		for _, id := range []string{"coordinator", "writer", "reviewer"} {
			back = strings.ReplaceAll(back, digests[v2+id], digests[v1+id])
		}
		if back == string(cur) || back != string(old) {
			t.Errorf("%s: want 75f27d6's bytes with only the model and the agent digests changed", rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// And it adds no file but its SOURCE and README.md.
	err = filepath.WalkDir(v2, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || d.Name() == "SOURCE" || d.Name() == "README.md" {
			return err
		}
		rel, _ := filepath.Rel(v2, path)
		if _, err := os.Stat(filepath.Join(v1, rel)); err != nil {
			t.Errorf("%s is not in 75f27d6", rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"coordinator", "writer", "reviewer"} {
		comp, err := Load(v2, id, "linux/amd64")
		if err != nil {
			t.Fatal(err)
		}
		if comp.Suite != "coordinator-workers" || comp.Agent.Model.Model != "gpt-5.4-mini" || comp.Agent.Model.Protocol != ModelProtocol {
			t.Fatalf("%s: suite %q, model %+v", id, comp.Suite, comp.Agent.Model)
		}
	}
}

// checkSource checks every "<git blob id> <sha256> <path>" line of dir/SOURCE against the file and
// that it lists want files.
func checkSource(t *testing.T, dir string, want int) {
	t.Helper()
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
	if files != want {
		t.Fatalf("%s/SOURCE lists %d files, want %d", dir, files, want)
	}
}

// Each vendored coordinator-workers file has the git blob id and sha256 SOURCE records for kaimahi
// 75f27d6.
func TestVendoredKaimahiCoordinatorWorkers(t *testing.T) {
	checkSource(t, "../suites/coordinator-workers", 15)
}

func TestLoadBindingIsStrict(t *testing.T) {
	path := filepath.Join(t.TempDir(), "b.json")
	if err := os.WriteFile(path, []byte(`{"apiVersion":"kmx.kaimahi.dev/v1alpha1","kind":"AgentSessionsBinding","atespace":"a","placement":{"backend":"substrate"},"snapshot":{"storageLocation":"gs://x/"},"secret":"x"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadBinding(path); err == nil || !strings.Contains(err.Error(), `unknown object member name "secret"`) {
		t.Fatalf("err = %v, want unknown fields refused", err)
	}
	const secretish = "s3cr3t-value"
	for _, tc := range []struct {
		name string
		edit func(m map[string]any)
		want string
	}{
		{"no model", func(m map[string]any) { delete(m, "model") }, "model.name must be one exact model identifier"},
		{"model name with a space", func(m map[string]any) { model(m)["name"] = "gpt 5" }, "model.name must be one exact model identifier"},
		{"another provider", func(m map[string]any) { model(m)["provider"] = "anthropic" }, `model.provider "anthropic": want openai`},
		{"a key in the base URL query", func(m map[string]any) { model(m)["baseURL"] = "https://h.example?api-key=" + secretish }, "model.baseURL: the base URL carries a query"},
		{"userinfo in the base URL", func(m map[string]any) { model(m)["baseURL"] = "https://u:" + secretish + "@h.example" }, "model.baseURL: the base URL carries userinfo"},
		{"a key in the path", func(m map[string]any) { model(m)["path"] = "/chat?api-key=" + secretish }, "model.path: the model path carries a query parameter other than api-version"},
		{"no secretRef", func(m map[string]any) { delete(model(m), "secretRef") }, "model.secretRef.name"},
		{"a dot secret key", func(m map[string]any) { model(m)["secretRef"].(map[string]any)["key"] = "." }, "model.secretRef.key"},
		{"a dot-dot secret key", func(m map[string]any) { model(m)["secretRef"].(map[string]any)["key"] = "..k" }, "model.secretRef.key"},
		{"a base URL with a port and no host", func(m map[string]any) { model(m)["baseURL"] = "https://:443" }, "model.baseURL: the base URL has no host"},
		{"a target id over 253 bytes", func(m map[string]any) { m["target"].(map[string]any)["id"] = "aa" + strings.Repeat(".a", 126) }, "is not a Kubernetes object name"},
		{"a credential field", func(m map[string]any) { model(m)["apiKey"] = secretish }, `unknown object member name "apiKey" within "/model"`},
		{"an empty sandbox config", func(m map[string]any) { m["sandbox"].(map[string]any)["configName"] = "" }, "sandbox.configName"},
		{"no fidelity", func(m map[string]any) { delete(m["snapshot"].(map[string]any), "fidelity") }, "snapshot.fidelity"},
		{"ROOTFS on gvisor", func(m map[string]any) {
			m["sandbox"].(map[string]any)["class"] = "gvisor"
			m["snapshot"].(map[string]any)["fidelity"] = "ROOTFS"
		}, "snapshot.fidelity ROOTFS requires sandbox.class microvm"},
		{"no storage location", func(m map[string]any) { m["snapshot"].(map[string]any)["storageLocation"] = "" }, "snapshot.storageLocation is required"},
		{"a dot-dot storage segment", func(m map[string]any) { m["snapshot"].(map[string]any)["storageLocation"] = "gs://b/x/../" + secretish }, "snapshot.storageLocation must be an object-storage URI"},
		{"an encoded dot-dot storage segment", func(m map[string]any) {
			m["snapshot"].(map[string]any)["storageLocation"] = "gs://b/%2e%2e/" + secretish
		}, "snapshot.storageLocation must be an object-storage URI"},
		{"an empty storage segment", func(m map[string]any) { m["snapshot"].(map[string]any)["storageLocation"] = "gs://b//" + secretish }, "snapshot.storageLocation must be an object-storage URI"},
		{"a storage query", func(m map[string]any) {
			m["snapshot"].(map[string]any)["storageLocation"] = "gs://b/x?sig=" + secretish
		}, "snapshot.storageLocation must be an object-storage URI"},
		{"storage userinfo", func(m map[string]any) {
			m["snapshot"].(map[string]any)["storageLocation"] = "gs://u:" + secretish + "@b/x/"
		}, "snapshot.storageLocation must be an object-storage URI"},
		{"storage with no bucket", func(m map[string]any) { m["snapshot"].(map[string]any)["storageLocation"] = "gs:///" + secretish }, "snapshot.storageLocation must be an object-storage URI"},
		{"a storage path, not a URI", func(m map[string]any) { m["snapshot"].(map[string]any)["storageLocation"] = "/var/" + secretish }, "snapshot.storageLocation must be an object-storage URI"},
		{"a backslash in storage", func(m map[string]any) {
			m["snapshot"].(map[string]any)["storageLocation"] = `gs://b/x\` + secretish + "/"
		}, "snapshot.storageLocation must be an object-storage URI"},
		{"an encoded backslash in storage", func(m map[string]any) {
			m["snapshot"].(map[string]any)["storageLocation"] = "gs://b/x%5C" + secretish + "/"
		}, "snapshot.storageLocation must be an object-storage URI"},
		{"a dots-only storage segment", func(m map[string]any) { m["snapshot"].(map[string]any)["storageLocation"] = "gs://b/.../" + secretish }, "snapshot.storageLocation must be an object-storage URI"},
		{"a dot-and-space storage segment", func(m map[string]any) {
			m["snapshot"].(map[string]any)["storageLocation"] = "gs://b/.%20./" + secretish
		}, "snapshot.storageLocation must be an object-storage URI"},
		// 960 bytes with no trailing slash, plus "/<63-byte template>/", would pass substrate's 1024.
		{"a 960-byte storage location", func(m map[string]any) { m["snapshot"].(map[string]any)["storageLocation"] = storageOf(960) }, "snapshot.storageLocation must be an object-storage URI"},
		{"no resources", func(m map[string]any) { delete(m, "resources") }, "resources.cpu is required"},
		{"no cpu", func(m map[string]any) { delete(m["resources"].(map[string]any), "cpu") }, "resources.cpu is required"},
		{"no memory", func(m map[string]any) { delete(m["resources"].(map[string]any), "memory") }, "resources.memory is required"},
		{"zero cpu", func(m map[string]any) { m["resources"].(map[string]any)["cpu"] = "0" }, "resources.cpu must be a positive Kubernetes quantity of at most 999.999 cores"},
		{"negative cpu", func(m map[string]any) { m["resources"].(map[string]any)["cpu"] = "-1" }, "resources.cpu must be a positive Kubernetes quantity"},
		{"cpu over substrate's bound", func(m map[string]any) { m["resources"].(map[string]any)["cpu"] = "1000" }, "resources.cpu must be a positive Kubernetes quantity"},
		{"cpu not a quantity", func(m map[string]any) { m["resources"].(map[string]any)["cpu"] = "one" }, "resources.cpu must be a positive Kubernetes quantity"},
		{"memory over 32 bytes", func(m map[string]any) { m["resources"].(map[string]any)["memory"] = strings.Repeat("0", 30) + "1Mi" }, "resources.memory must be a positive Kubernetes quantity of at most 9223372036854775807 bytes"},
		{"eleven worker labels", func(m map[string]any) {
			labels := map[string]any{}
			for i := range 11 {
				labels[fmt.Sprintf("k%d", i)] = "v"
			}
			m["workerSelector"] = map[string]any{"matchLabels": labels}
		}, "workerSelector.matchLabels has more than 10 labels"},
		{"a worker label key", func(m map[string]any) { m["workerSelector"] = map[string]any{"matchLabels": map[string]any{"-k": "v"}} }, "workerSelector.matchLabels key is not a qualified label name"},
		{"a worker label value", func(m map[string]any) {
			m["workerSelector"] = map[string]any{"matchLabels": map[string]any{"k": "a b"}}
		}, "workerSelector.matchLabels value is not a label value"},
		{"a repeated gate", func(m map[string]any) {
			m["approval"] = map[string]any{"invokes": []any{map[string]any{"caller": "coordinator", "agent": "reviewer"}, map[string]any{"caller": "coordinator", "agent": "reviewer"}}}
		}, "coordinator -> reviewer is given more than once"},
		{"a gate with no caller", func(m map[string]any) {
			m["approval"] = map[string]any{"invokes": []any{map[string]any{"agent": "reviewer"}}}
		}, "caller and agent must be agent ids"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadBinding(writeBinding(t, "../binding-coordinator.json", tc.edit))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			if strings.Contains(err.Error(), secretish) {
				t.Fatalf("err = %q repeats the value", err)
			}
		})
	}
	for _, ok := range []func(m map[string]any){
		func(m map[string]any) { m["snapshot"].(map[string]any)["fidelity"] = "ROOTFS" }, // on microvm
		func(m map[string]any) { m["snapshot"].(map[string]any)["storageLocation"] = "gs://bucket" },
		func(m map[string]any) { m["snapshot"].(map[string]any)["storageLocation"] = "s3://bucket/a/b" },
		func(m map[string]any) { m["snapshot"].(map[string]any)["storageLocation"] = "gs://bucket/a.b/c..d/" },
		func(m map[string]any) { m["snapshot"].(map[string]any)["storageLocation"] = storageOf(959) },
		func(m map[string]any) {
			m["resources"] = map[string]any{"cpu": "999.999", "memory": strings.Repeat("0", 29) + "1Mi"}
		},
		func(m map[string]any) { m["resources"] = map[string]any{"cpu": "500m", "memory": "1Gi"} },
		func(m map[string]any) {
			labels := map[string]any{"ate.dev/pool": "x", "empty": ""}
			for i := range 8 {
				labels[fmt.Sprintf("k%d", i)] = "v"
			}
			m["workerSelector"] = map[string]any{"matchLabels": labels}
		},
	} {
		if _, err := LoadBinding(writeBinding(t, "../binding-coordinator.json", ok)); err != nil {
			t.Errorf("a binding kmx accepts was refused: %v", err)
		}
	}
	trailing := filepath.Join(t.TempDir(), "b.json")
	raw, _ := os.ReadFile("../binding-coordinator.json")
	if err := os.WriteFile(trailing, append(raw, []byte(`{}`)...), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadBinding(trailing); err == nil || !strings.Contains(err.Error(), "after top-level value") {
		t.Fatalf("a second document: err = %v", err)
	}
	// What kmx's YAML decoder refuses in the same JSON text (kaimahi
	// internal/kmx/runtime.ParseAgentSessionsBinding at KmxCommit refused these edits of
	// binding-coordinator.json, with its PoC-only fields removed, all but the model given twice): a
	// member given twice, in the binding or in a map; a member name in another case, which
	// encoding/json v1 would have matched; invalid UTF-8.
	for _, tc := range []struct {
		name, old, new, want string
	}{
		{"a member given twice", `"kind": "AgentSessionsBinding"`, `"kind": "AgentSessionsBinding", "kind": "AgentSessionsBinding"`, `duplicate object member name "kind"`},
		{"a model given twice", `"approval"`, `"model": {"name": "` + secretish + `"}, "approval"`, `duplicate object member name "model"`},
		{"a label given twice", `{"workload": "agentsuite-demo-microvm"}`, `{"workload": "x", "workload": "y"}`, `duplicate object member name "workload" within "/workerSelector/matchLabels"`},
		{"a member in another case", `"kind":`, `"Kind":`, `unknown object member name "Kind"`},
		{"a model member in another case", `"name": "gpt-5.4-mini"`, `"Name": "gpt-5.4-mini"`, `unknown object member name "Name" within "/model"`},
		{"invalid UTF-8", `"configName": "microvm"`, "\"configName\": \"micro\xffvm\"", `invalid UTF-8 within "/sandbox/configName"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !bytes.Contains(raw, []byte(tc.old)) {
				t.Fatalf("binding-coordinator.json has no %s", tc.old)
			}
			path := filepath.Join(t.TempDir(), "b.json")
			if err := os.WriteFile(path, bytes.Replace(raw, []byte(tc.old), []byte(tc.new), 1), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := LoadBinding(path)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			if strings.Contains(err.Error(), secretish) {
				t.Fatalf("err = %q repeats a value", err)
			}
		})
	}
}

func model(m map[string]any) map[string]any { return m["model"].(map[string]any) }

// storageOf is a gs:// storage location of n bytes with no trailing slash.
func storageOf(n int) string { return "gs://b/" + strings.Repeat("x", n-len("gs://b/")) }

// A render's storage location stays within substrate's 1024 bytes for the longest location the
// binding accepts and the longest template name.
func TestStorageLocationFitsSubstrate(t *testing.T) {
	comp, err := Load("../suite", "triage", "linux/amd64")
	if err != nil {
		t.Fatal(err)
	}
	comp.Suite = strings.Repeat("s", 80)
	b, err := LoadBinding(writeBinding(t, "../binding-microvm.json", func(m map[string]any) {
		m["snapshot"].(map[string]any)["storageLocation"] = storageOf(maxStorageLocation)
	}))
	if err != nil {
		t.Fatal(err)
	}
	out, err := Render(comp, imageA, b, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if n, loc := len(out.Template.GetMetadata().GetName()), out.Template.GetSnapshotConfig().GetStorageLocation(); n != 63 || len(loc) != 1024 {
		t.Fatalf("template name %d bytes, storage location %d bytes: want 63 and exactly substrate's 1024", n, len(loc))
	}
}

// The binding digest is kmx's: the JCS digest of the binding's fields, so key order, whitespace and
// an empty workerSelector do not change it, and every field does.
func TestBindingDigest(t *testing.T) {
	base, err := LoadBinding("../binding-microvm.json")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile("../binding-microvm.json")
	if d, _ := agentsuite.Digest(raw); d != base.Digest() {
		t.Fatalf("digest %s, want the JCS digest of the file %s (it spells every field)", base.Digest(), d)
	}
	same, err := LoadBinding(writeBinding(t, "../binding-microvm.json", func(map[string]any) {}))
	if err != nil {
		t.Fatal(err)
	}
	if same.Digest() != base.Digest() {
		t.Fatal("re-encoding the binding changed its digest")
	}
	noSel, _ := LoadBinding(writeBinding(t, "../binding-microvm.json", func(m map[string]any) { delete(m, "workerSelector") }))
	emptySel, _ := LoadBinding(writeBinding(t, "../binding-microvm.json", func(m map[string]any) { m["workerSelector"] = map[string]any{"matchLabels": map[string]any{}} }))
	if noSel.Digest() != emptySel.Digest() || noSel.Digest() == base.Digest() {
		t.Fatal("an empty workerSelector must be the omitted one, and dropping the selector must change the digest")
	}
	served, _ := LoadBinding(writeBinding(t, "../binding-microvm.json", func(m map[string]any) { model(m)["name"] = "other" }))
	if served.Digest() == base.Digest() {
		t.Fatal("the served model must change the digest")
	}
	// kmx computes the same digest: these values came from kaimahi
	// internal/kmx/runtime.AgentSessionsBindingDigest at KmxCommit on the committed files.
	for file, want := range map[string]string{
		"../binding-microvm.json": "sha256:a8ce35e86590c6e1708907fd054718f8d5fc5a1b7ff838df0f065bc0c4b2ffa4",
		"../binding.json":         "sha256:dea63034f38fb9f8b597c889143bc414e29c013368ca35c713019d733eb7863a",
	} {
		b, err := LoadBinding(file)
		if err != nil {
			t.Fatal(err)
		}
		if b.Digest() != want {
			t.Errorf("%s: digest %s, want kmx's %s", file, b.Digest(), want)
		}
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
