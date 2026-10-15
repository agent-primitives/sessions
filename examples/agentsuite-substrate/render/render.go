// Package render maps one AgentSuite composition (agent, platform) onto the two objects a deploy
// needs: a substrate ActorTemplate whose image is the agent's harness, and the AgentSessions harness
// registration that sends sessions to it.
//
// The field mapping follows the AgentSuite to ActorTemplate design (v0): the template name is
// content-addressed, <suite>-<agent>-<hash8> with hash8 the first 8 hex of
// sha256(renderer version + "\n" + image reference + "\n" + binding digest), so the same image under
// another binding, registry or renderer is another template; the harness speaks harnesswire on port
// 80 with a /readyz wakeup probe on 8081; the sandbox, snapshot, template resources, worker selector
// and approval gates come from the binding. The harness requests the model its agent pins, and the
// binding must serve exactly that model. Model credentials are host configuration and never
// rendered.
package render

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	atepb "github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"github.com/distribution/reference"
	"github.com/kaimahi-agents/kaimahi/agentsuite"

	v1 "github.com/aramase/agentsessions/api/genpb"
	"github.com/aramase/agentsessions/examples/agentsuite-substrate/agent"
)

// HarnessPort is the port the harness serves harnesswire on; the atenet-router forwards only to 80.
const HarnessPort = 80

// ReadyzPort is the port of the harness's HTTP /readyz, the template's wakeup probe.
const ReadyzPort = 8081

// AgentDir is where the harness image carries the agent manifest and its instructions.
const AgentDir = "/agent"

// Composition is one validated (agent, platform) selection from a suite.
type Composition struct {
	Suite        string
	Agent        agentsuite.Agent
	Platform     agentsuite.Platform
	Digest       string // the composition manifest's digest
	AgentJSON    []byte // the agent manifest as stored in the suite
	Instructions []byte
}

// Load validates the suite directory with the AgentSuite reference validator and selects one agent's
// composition for platform ("os/arch").
func Load(dir, agentID, platform string) (*Composition, error) {
	report, err := agentsuite.ValidateDirectory(dir)
	if err != nil {
		return nil, fmt.Errorf("validate suite %s: %w", dir, err)
	}
	osName, arch, ok := strings.Cut(platform, "/")
	if !ok {
		return nil, fmt.Errorf("platform %q: want os/arch", platform)
	}
	var sel *agentsuite.CompositionSelection
	for i, s := range report.CompositionSelections {
		if s.Agent == agentID && s.Platform.OS == osName && s.Platform.Architecture == arch {
			sel = &report.CompositionSelections[i]
		}
	}
	if sel == nil {
		return nil, fmt.Errorf("suite %s has no composition for agent %q on %s", report.Name, agentID, platform)
	}
	var suite agentsuite.Suite
	if err := readStrict(filepath.Join(dir, "agentsuite.json"), &suite); err != nil {
		return nil, err
	}
	var agentPath string
	for _, ref := range suite.Agents {
		if ref.ID == agentID {
			agentPath = ref.Path
		}
	}
	agentJSON, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(agentPath)))
	if err != nil {
		return nil, err
	}
	var agent agentsuite.Agent
	if err := agentsuite.Unmarshal(agentJSON, &agent); err != nil {
		return nil, fmt.Errorf("agent %s: %w", agentID, err)
	}
	if err := supported(agent); err != nil {
		return nil, err
	}
	instructions, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(agent.Instructions.Path)))
	if err != nil {
		return nil, err
	}
	return &Composition{
		Suite:        suite.Name,
		Agent:        agent,
		Platform:     sel.Platform,
		Digest:       sel.Digest,
		AgentJSON:    agentJSON,
		Instructions: instructions,
	}, nil
}

// ModelProtocol is the only model protocol an agent can declare: agentsessionsd answers the
// harness's model calls through an OpenAI-compatible endpoint (the binding's model).
const ModelProtocol = "openai-compatible"

// reservedHarnesses are the harness names agentsessionsd serves itself (echo, and chat with -model);
// it refuses to start with a -substrate-harness of either name.
var reservedHarnesses = []string{"chat", "echo"}

// supported reports whether this v0 renderer can deploy the agent.
func supported(a agentsuite.Agent) error {
	// v0 renders agents without tool providers: the harness protocol has no tool parts wired to
	// suite tools yet. Invokes edges are served by the coordinator harness and the host's
	// agentsuite.invoke executor (package invoke).
	if len(a.ToolProviders) > 0 {
		return fmt.Errorf("agent %s declares tool providers; this v0 renderer supports agents without tool providers only", a.ID)
	}
	if a.Model.Protocol != ModelProtocol {
		return fmt.Errorf("agent %s: model protocol %q: the host serves an %s model only", a.ID, a.Model.Protocol, ModelProtocol)
	}
	// The agent id becomes the harness name.
	if slices.Contains(reservedHarnesses, a.ID) {
		return fmt.Errorf("agent id %q is the name of a harness agentsessionsd serves itself (%s); rename the agent", a.ID, strings.Join(reservedHarnesses, ", "))
	}
	return nil
}

func readStrict(path string, out any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := agentsuite.Unmarshal(b, out); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

var (
	dnsLabel = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)
	notLabel = regexp.MustCompile(`[^a-z0-9-]+`)
)

// maxImageReference is substrate's Container.image bound.
const maxImageReference = 512

// checkPinnedImage applies kmx's harness image rule (HarnessImageDigest): a reference of at most
// substrate's 512 bytes that parses as an OCI reference pinned by a sha256 digest, without a tag.
// Unlike kmx's, it does not scan the reference for credential shapes; like kmx's, its error does
// not repeat the reference, which may be one.
func checkPinnedImage(image string) error {
	err := fmt.Errorf("the harness image is not a digest-pinned reference (name@sha256:<64 hex>) without a tag, of at most %d bytes (the value is not shown, since it may hold a credential)", maxImageReference)
	if image == "" || len(image) > maxImageReference || strings.TrimSpace(image) != image {
		return err
	}
	ref, perr := reference.Parse(image)
	if perr != nil {
		return err
	}
	digested, ok := ref.(reference.Canonical)
	if _, tagged := ref.(reference.Tagged); !ok || tagged || digested.Digest().Algorithm() != "sha256" || !agentsuite.IsValidDigest(digested.Digest().String()) {
		return err
	}
	return nil
}

// Options are what a render sets beyond the composition, image and binding.
type Options struct {
	// Memory renders the memory tier: the harness keeps the conversation in RAM
	// (agent.MemoryEnv=agent.MemoryRAM) and is registered as REQUIRES_MEMORY_SNAPSHOT under the name
	// <agent>-memory.
	Memory bool
	// ActorAtespace, when set, is where agentsessionsd creates this harness's session actors: the
	// daemon flag gets ,actors=<atespace>, so several agents can share one daemon.
	ActorAtespace string
}

// RendererVersion identifies this PoC's binding-to-ActorTemplate mapping and is part of hash8, as
// kmx's RendererVersion is part of its own: a mapping change names a new template instead of reusing
// a name whose rendered spec differs. v2 hashes the full image reference and sizes the actor from
// the template's resources (v1 hashed the image digest and set container limits). It is not kmx's
// value, because this renderer's template is not kmx's (its env sets AGENT_DIR, AGENTSUITE_APPROVAL
// and the memory tier; kmx's sets HARNESS_KIND and HARNESS_MODEL), so the PoC and kmx never give
// one name two specs.
const RendererVersion = "agentsessions-poc.actortemplate/v2"

// TemplateName is <suite>-<agent>-<hash8>: hash8 is the first 8 hex of
// sha256(RendererVersion + "\n" + image + "\n" + bindingDigest), kmx's formula, where image is the
// full digest-pinned reference the template runs (the same digest under another registry or
// repository is another template), with "\nmemory" appended when o.Memory is set (the template then
// selects the memory tier in its env; kmx has no memory tier). The binding digest covers the served
// model, resources and the approval gates, so changing any of them names another template. The
// result is a DNS label of at most 63 characters; suite and agent are lowercased, sanitized and
// truncated to fit, never hash8.
func TemplateName(suite, agent, image, bindingDigest string, o Options) string {
	return templateName(RendererVersion, suite, agent, image, bindingDigest, o)
}

func templateName(renderer, suite, agent, image, bindingDigest string, o Options) string {
	content := renderer + "\n" + image + "\n" + bindingDigest
	if o.Memory {
		content += "\nmemory"
	}
	sum := sha256.Sum256([]byte(content))
	hash8 := hex.EncodeToString(sum[:])[:8]
	clean := func(s string) string {
		return strings.Trim(notLabel.ReplaceAllString(strings.ToLower(s), "-"), "-")
	}
	s, a := clean(suite), clean(agent)
	// 63 = len(s) + 1 + len(a) + 1 + 8. Split the 53 characters between suite and agent, giving
	// either one the other's unused share.
	const budget = 63 - 2 - 8
	for len(s)+len(a) > budget {
		if len(s) >= len(a) {
			s = s[:len(s)-1]
		} else {
			a = a[:len(a)-1]
		}
	}
	s, a = strings.Trim(s, "-"), strings.Trim(a, "-")
	return s + "-" + a + "-" + hash8
}

// Output is what Render produces for one composition.
type Output struct {
	Template     *atepb.ActorTemplate
	Registration *v1.HarnessRegistration
	// DaemonFlag is how agentsessionsd registers the harness: -substrate-harness
	// name=atespace/template[,memory][,actors=<atespace>]. The HarnessRegistry API does not take a
	// substrate placement on this base (internal/agentsessionsd registeredPlacer refuses one).
	DaemonFlag string
	// ModelFlags are the agentsessionsd flags that serve the binding's model (-model and, when the
	// binding sets one, -model-base-url; never -model-path, whose default the daemon keeps).
	ModelFlags []string
}

// Agents validates the suite directory and returns every agent manifest it lists, in suite order.
func Agents(dir string) ([]agentsuite.Agent, error) {
	if _, err := agentsuite.ValidateDirectory(dir); err != nil {
		return nil, fmt.Errorf("validate suite %s: %w", dir, err)
	}
	var suite agentsuite.Suite
	if err := readStrict(filepath.Join(dir, "agentsuite.json"), &suite); err != nil {
		return nil, err
	}
	agents := make([]agentsuite.Agent, 0, len(suite.Agents))
	for _, ref := range suite.Agents {
		var a agentsuite.Agent
		if err := readStrict(filepath.Join(dir, filepath.FromSlash(ref.Path)), &a); err != nil {
			return nil, err
		}
		agents = append(agents, a)
	}
	return agents, nil
}

// Render maps a composition, its digest-pinned harness image and a binding onto an ActorTemplate and
// a harness registration. The harness registration's name is the agent id, or <agent>-memory for the
// memory tier (AGENTSUITE_MEMORY=ram, folded into the template name), which needs the binding's
// MEMORY snapshot fidelity. Render refuses a binding that serves a model other than the one the
// agent pins (ErrModelMismatch). The binding's approval edges whose caller is this agent are set in
// the template's env (AGENTSUITE_APPROVAL); each must be one of the agent's invokes edges.
func Render(c *Composition, image string, b *Binding, o Options) (*Output, error) {
	// Load checked the agent; check again for a Composition built by hand.
	if err := supported(c.Agent); err != nil {
		return nil, err
	}
	if err := CheckModel(c, b); err != nil {
		return nil, err
	}
	targets := b.ApprovalTargets(c.Agent.ID)
	for _, t := range targets {
		if !slices.ContainsFunc(c.Agent.Invokes, func(inv agentsuite.AgentInvoke) bool { return inv.Agent == t }) {
			return nil, fmt.Errorf("approval for %q: agent %s has no invokes edge to it", t, c.Agent.ID)
		}
	}
	if len(c.Agent.Invokes) > 0 && o.Memory {
		return nil, fmt.Errorf("agent %s has invokes edges: delegations need the approval-capable stateless tier", c.Agent.ID)
	}
	// The memory tier keeps the conversation only in RAM and is registered as
	// REQUIRES_MEMORY_SNAPSHOT; substrate cold-boots a ROOTFS or VOLUMES snapshot on resume, without
	// process memory, so a resumed or forked session would come back empty with nothing to say so.
	if o.Memory && b.Snapshot.Fidelity != "MEMORY" {
		return nil, fmt.Errorf("the memory tier needs snapshot.fidelity MEMORY, the binding gives %s: substrate cold-boots that fidelity on resume, without the conversation the harness keeps in RAM", b.Snapshot.Fidelity)
	}
	if o.ActorAtespace != "" && !dnsLabel.MatchString(o.ActorAtespace) {
		return nil, fmt.Errorf("actor atespace %q is not a DNS label", o.ActorAtespace)
	}
	if err := checkPinnedImage(image); err != nil {
		return nil, err
	}
	class, err := sandboxClass(b.Sandbox.Class)
	if err != nil {
		return nil, err
	}
	fidelity, err := snapshotFidelity(b.Snapshot.Fidelity)
	if err != nil {
		return nil, err
	}
	name := TemplateName(c.Suite, c.Agent.ID, image, b.Digest(), o)
	container := &atepb.Container{
		Name:  "harness",
		Image: image,
		Env: []*atepb.EnvVar{
			{Name: "HARNESS_ADDR", Value: fmt.Sprintf(":%d", HarnessPort)},
			{Name: "HARNESS_READYZ", Value: fmt.Sprintf(":%d", ReadyzPort)},
			{Name: "AGENT_DIR", Value: AgentDir},
		},
		WakeupProbe: &atepb.ContainerWakeupProbe{
			HttpGet:        &atepb.HTTPGetAction{Path: "/readyz", Port: ReadyzPort},
			TimeoutSeconds: 120,
		},
		// Add no capabilities: the harness needs none, and the template is the only place to say so.
		SecurityContext: &atepb.SecurityContext{Capabilities: &atepb.Capabilities{}},
	}
	harness, resumability, tier := c.Agent.ID, v1.Resumability_RESUMABILITY_STATELESS_REPLAY, ""
	if o.Memory {
		container.Env = append(container.Env, &atepb.EnvVar{Name: agent.MemoryEnv, Value: agent.MemoryRAM})
		harness, resumability, tier = c.Agent.ID+"-memory", v1.Resumability_RESUMABILITY_REQUIRES_MEMORY_SNAPSHOT, ",memory"
	}
	if len(targets) > 0 {
		container.Env = append(container.Env, &atepb.EnvVar{Name: agent.ApprovalEnv, Value: strings.Join(targets, ",")})
	}
	if o.ActorAtespace != "" {
		tier += ",actors=" + o.ActorAtespace
	}
	tmpl := &atepb.ActorTemplate{
		Metadata:   &atepb.ResourceMetadata{Atespace: b.Atespace, Name: name},
		Containers: []*atepb.Container{container},
		SnapshotConfig: &atepb.SnapshotConfig{
			PreferredFidelity: fidelity,
			StorageLocation:   strings.TrimSuffix(b.Snapshot.StorageLocation, "/") + "/" + name + "/",
		},
		SandboxConfig: &atepb.SandboxConfig{SandboxClass: class, ConfigName: b.Sandbox.ConfigName},
		// The actor's size, as kmx renders it: substrate books these limits against worker capacity
		// and sizes the sandbox by them (a micro-VM's vCPUs, and its RAM less the VMM reserve). No
		// container limits: inside a micro-VM they would have to fit under that reduced guest RAM.
		Resources: &atepb.Resources{Limits: []*atepb.Limits{
			{Name: "cpu", Quantity: b.Resources.CPU},
			{Name: "memory", Quantity: b.Resources.Memory},
		}},
	}
	if len(b.WorkerSelector.MatchLabels) > 0 {
		tmpl.WorkerSelector = &atepb.Selector{MatchLabels: b.WorkerSelector.MatchLabels}
	}
	reg := &v1.HarnessRegistration{
		Metadata: &v1.ResourceMetadata{Name: harness},
		Spec: &v1.HarnessSpec{
			Placement: &v1.HarnessSpec_Substrate{Substrate: &v1.SubstratePlacement{Atespace: b.Atespace, Template: name}},
			Capabilities: &v1.Capabilities{
				Resumability: resumability,
			},
			DescriptorId: c.Agent.ID,
		},
	}
	return &Output{
		Template:     tmpl,
		Registration: reg,
		DaemonFlag:   fmt.Sprintf("%s=%s/%s%s", harness, b.Atespace, name, tier),
		ModelFlags:   b.DaemonModelFlags(),
	}, nil
}

func sandboxClass(s string) (atepb.SandboxClass, error) {
	switch s {
	case "microvm":
		return atepb.SandboxClass_SANDBOX_CLASS_MICROVM, nil
	case "gvisor":
		return atepb.SandboxClass_SANDBOX_CLASS_GVISOR, nil
	}
	return 0, fmt.Errorf("sandbox.class %q: want microvm or gvisor", s)
}

func snapshotFidelity(s string) (atepb.SnapshotFidelity, error) {
	switch s {
	case "", "MEMORY":
		return atepb.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY, nil
	case "ROOTFS":
		return atepb.SnapshotFidelity_SNAPSHOT_FIDELITY_ROOTFS, nil
	case "VOLUMES":
		return atepb.SnapshotFidelity_SNAPSHOT_FIDELITY_VOLUMES, nil
	}
	return 0, errors.New("snapshot.fidelity: want MEMORY, ROOTFS or VOLUMES")
}
