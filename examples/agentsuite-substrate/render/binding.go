package render

import (
	"encoding/json"
	jsonv2 "encoding/json/v2"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"

	"github.com/kaimahi-agents/kaimahi/agentsuite"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/aramase/agentsessions/examples/agentsuite-substrate/invoke"
)

const (
	// KmxCommit is the kaimahi commit (branch aramase/f/kmx_agentsessions_binding) whose
	// AgentSessionsBinding validation, digest and TemplateName this package was compared against;
	// render_test.go pins the values kmx's code gave there.
	KmxCommit = "e005eb7baad25fa9239b6baceeb1d61a36e547a8"
	// BindingAPIVersion and BindingKind identify an AgentSessionsBinding, as kaimahi's kmx does.
	BindingAPIVersion = "kmx.kaimahi.dev/v1alpha1"
	BindingKind       = "AgentSessionsBinding"
	// ModelProvider is the only model provider a binding names: agentsessionsd serves an
	// OpenAI-compatible endpoint.
	ModelProvider = "openai"
)

// Binding is the per-environment half of a deployment: the AgentSessionsBinding. The suite pins
// the exact model; the binding says where that model is served and where sessions are placed. It
// lives outside the suite, one per environment.
//
// Its fields and refusals follow kmx's AgentSessionsBinding (kaimahi
// internal/kmx/runtime/agentsessions_binding.go on branch aramase/f/kmx_agentsessions_binding, at
// KmxCommit), with one addition this PoC needs: approval (the invokes edges this environment
// gates). Its model is kmx's exactly (name, provider, baseURL, secretRef): an Azure OpenAI
// deployment is served from its v1 endpoint (https://<resource>.openai.azure.com/openai/v1) with
// agentsessionsd's default completions path and no api-version, so the binding needs no path. The
// README section "The binding and kmx" lists every difference.
type Binding struct {
	APIVersion     string                `json:"apiVersion"`
	Kind           string                `json:"kind"`
	Target         BindingTarget         `json:"target"`
	Placement      BindingPlacement      `json:"placement"`
	Atespace       string                `json:"atespace"`
	Sandbox        BindingSandbox        `json:"sandbox"`
	Snapshot       BindingSnapshot       `json:"snapshot"`
	Resources      BindingResources      `json:"resources"`
	WorkerSelector BindingWorkerSelector `json:"workerSelector,omitzero"`
	Model          BindingModel          `json:"model"`
	Approval       BindingApproval       `json:"approval,omitzero"`

	digest string
}

type BindingTarget struct {
	Platform string `json:"platform"`
	ID       string `json:"id"`
}

type BindingPlacement struct {
	Backend string `json:"backend"`
}

type BindingSandbox struct {
	Class      string `json:"class"`
	ConfigName string `json:"configName"`
}

type BindingSnapshot struct {
	Fidelity        string `json:"fidelity"`
	StorageLocation string `json:"storageLocation"`
}

// BindingResources is the actor's compute size, rendered as the ActorTemplate's resources: substrate
// books both limits against worker capacity and sizes the sandbox (a micro-VM's vCPUs and RAM) by
// them, so both are required, as in kmx.
type BindingResources struct {
	CPU    string `json:"cpu"`
	Memory string `json:"memory"`
}

type BindingWorkerSelector struct {
	MatchLabels map[string]string `json:"matchLabels,omitempty"`
}

// BindingModel says where the suite's pinned model is served: Name must equal the model every
// rendered agent pins, BaseURL is agentsessionsd's -model-base-url (the daemon keeps its default
// -model-path, /chat/completions, which it appends to the base URL's path), and SecretRef names the
// Secret key that holds the credential. It cannot hold a credential value. It has kmx's members
// only, so the strict decoder refuses any other (a model.path included).
type BindingModel struct {
	Name      string           `json:"name"`
	Provider  string           `json:"provider"`
	BaseURL   string           `json:"baseURL,omitempty"`
	SecretRef BindingSecretRef `json:"secretRef"`
}

type BindingSecretRef struct {
	Name string `json:"name"`
	Key  string `json:"key"`
}

// BindingApproval is PoC-only: the invokes edges whose calls this environment sends to an operator
// for approval. The suite declares the edges; AgentSuite has no approval field on an edge.
type BindingApproval struct {
	Invokes []invoke.Gate `json:"invokes,omitempty"`
}

var (
	dnsSubdomain = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`)
	secretKey    = regexp.MustCompile(`^[-._a-zA-Z0-9]+$`)
)

// objectName is kmx's Kubernetes object name rule: an RFC 1123 subdomain of at most 253 bytes.
func objectName(s string) bool { return len(s) <= 253 && dnsSubdomain.MatchString(s) }

// validSecretKey is kmx's Secret key rule: 1-253 letters, digits, '-', '_' or '.', not "." and not
// starting with "..".
func validSecretKey(s string) bool {
	return len(s) <= 253 && secretKey.MatchString(s) && s != "." && !strings.HasPrefix(s, "..")
}

// LoadBinding decodes a binding strictly, validates it, and records its digest (see Digest). The
// decoder is encoding/json/v2's, which refuses what kmx's YAML decoder refuses in a JSON document:
// an unknown member, a member name in another case ("Kind" for "kind"), a member given twice in one
// object, invalid UTF-8, and anything after the one document. Its errors name members, never their
// values, so they never repeat model.baseURL.
func LoadBinding(path string) (*Binding, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var b Binding
	if err := jsonv2.Unmarshal(raw, &b, jsonv2.RejectUnknownMembers(true)); err != nil {
		return nil, fmt.Errorf("binding %s: %w", path, err)
	}
	if err := b.validate(); err != nil {
		return nil, fmt.Errorf("binding %s: %w", path, err)
	}
	b = b.normalized()
	enc, err := json.Marshal(b)
	if err != nil {
		return nil, fmt.Errorf("binding %s: %w", path, err)
	}
	if b.digest, err = agentsuite.Digest(enc); err != nil {
		return nil, fmt.Errorf("binding %s: %w", path, err)
	}
	return &b, nil
}

// Digest is "sha256:" plus the SHA-256 of the RFC 8785 (JCS) form of the binding's fields, as kmx
// computes it: key order and whitespace do not change it, and an empty workerSelector or approval
// is the same binding as an omitted one. The approval edges are sorted first, so their order does
// not change it either (kmx has no approval field).
func (b *Binding) Digest() string { return b.digest }

// normalized gives equivalent spellings one form: the approval edges are a set, so they are sorted.
func (b Binding) normalized() Binding {
	if len(b.WorkerSelector.MatchLabels) == 0 {
		b.WorkerSelector.MatchLabels = nil
	}
	if len(b.Approval.Invokes) == 0 {
		b.Approval.Invokes = nil
	}
	b.Approval.Invokes = slices.Clone(b.Approval.Invokes)
	slices.SortFunc(b.Approval.Invokes, func(x, y invoke.Gate) int {
		return strings.Compare(x.Caller+"\x00"+x.Agent, y.Caller+"\x00"+y.Agent)
	})
	return b
}

func (b Binding) validate() error {
	switch {
	case b.APIVersion != BindingAPIVersion || b.Kind != BindingKind:
		return fmt.Errorf("want %s %s, got %q %q", BindingAPIVersion, BindingKind, b.APIVersion, b.Kind)
	case b.Target.Platform != "kubernetes":
		return errors.New("target.platform must be kubernetes")
	case !objectName(b.Target.ID):
		return fmt.Errorf("target.id %q is not a Kubernetes object name", b.Target.ID)
	case b.Placement.Backend != "substrate":
		return fmt.Errorf("placement.backend %q: only substrate is rendered", b.Placement.Backend)
	case !dnsLabel.MatchString(b.Atespace):
		return fmt.Errorf("atespace %q is not a DNS label", b.Atespace)
	case b.Sandbox.Class != "microvm" && b.Sandbox.Class != "gvisor":
		return fmt.Errorf("sandbox.class %q: want microvm or gvisor", b.Sandbox.Class)
	case !objectName(b.Sandbox.ConfigName):
		return fmt.Errorf("sandbox.configName %q is not a Kubernetes object name", b.Sandbox.ConfigName)
	case b.Snapshot.Fidelity != "MEMORY" && b.Snapshot.Fidelity != "ROOTFS" && b.Snapshot.Fidelity != "VOLUMES":
		return errors.New("snapshot.fidelity: want MEMORY, ROOTFS or VOLUMES")
	case b.Snapshot.Fidelity == "ROOTFS" && b.Sandbox.Class != "microvm":
		// Substrate serves root filesystem snapshots on the micro-VM class only (kmx refuses it too).
		return errors.New("snapshot.fidelity ROOTFS requires sandbox.class microvm")
	}
	if err := checkStorageLocation(b.Snapshot.StorageLocation); err != nil {
		return err
	}
	if err := b.Resources.validate(); err != nil {
		return err
	}
	if err := b.WorkerSelector.validate(); err != nil {
		return err
	}
	if err := b.Model.validate(); err != nil {
		return err
	}
	seen := map[invoke.Gate]bool{}
	for _, g := range b.Approval.Invokes {
		if !dnsLabel.MatchString(g.Caller) || !dnsLabel.MatchString(g.Agent) {
			return fmt.Errorf("approval.invokes: %q -> %q: caller and agent must be agent ids", g.Caller, g.Agent)
		}
		if seen[g] {
			return fmt.Errorf("approval.invokes: %s -> %s is given more than once", g.Caller, g.Agent)
		}
		seen[g] = true
	}
	return nil
}

func (m BindingModel) validate() error {
	switch {
	case m.Name == "" || strings.ContainsFunc(m.Name, func(r rune) bool { return r <= ' ' || r == 0x7f }):
		return errors.New("model.name must be one exact model identifier without whitespace")
	case m.Provider != ModelProvider:
		return fmt.Errorf("model.provider %q: want %s (an OpenAI-compatible endpoint)", m.Provider, ModelProvider)
	case !objectName(m.SecretRef.Name):
		return fmt.Errorf("model.secretRef.name %q is not a Secret name", m.SecretRef.Name)
	case !validSecretKey(m.SecretRef.Key):
		return fmt.Errorf("model.secretRef.key %q is not a Secret key", m.SecretRef.Key)
	}
	if m.BaseURL != "" {
		if err := CheckModelBaseURL(m.BaseURL); err != nil {
			return fmt.Errorf("model.baseURL: %w", err)
		}
	}
	return nil
}

const (
	// maxQuantity and maxWorkerLabels are substrate's Limits.quantity and Selector.match_labels
	// bounds, as kmx applies them.
	maxQuantity     = 32
	maxWorkerLabels = 10
)

// resourceBounds are substrate's cpuLimitMax and memoryLimitMax, as kmx applies them.
var resourceBounds = []struct {
	path  string
	max   resource.Quantity
	bound string
}{
	{"resources.cpu", resource.MustParse("999.999"), "999.999 cores"},
	{"resources.memory", resource.MustParse("9223372036854775807"), "9223372036854775807 bytes"},
}

// validate applies kmx's resources rule with the parser substrate uses (apimachinery's
// resource.ParseQuantity): both limits are required, positive and within substrate's bounds.
func (r BindingResources) validate() error {
	for i, value := range []string{r.CPU, r.Memory} {
		b := resourceBounds[i]
		if value == "" {
			return fmt.Errorf("%s is required", b.path)
		}
		q, err := resource.ParseQuantity(value)
		if len(value) > maxQuantity || err != nil || q.Sign() <= 0 || q.Cmp(b.max) > 0 {
			return fmt.Errorf("%s must be a positive Kubernetes quantity of at most %s", b.path, b.bound)
		}
	}
	return nil
}

// validate applies kmx's workerSelector rule: at most ten labels, each a qualified label name with
// a label value.
func (s BindingWorkerSelector) validate() error {
	if len(s.MatchLabels) > maxWorkerLabels {
		return fmt.Errorf("workerSelector.matchLabels has more than %d labels", maxWorkerLabels)
	}
	keys := slices.Sorted(maps.Keys(s.MatchLabels))
	for _, k := range keys {
		if errs := validation.IsQualifiedName(k); len(errs) != 0 {
			return fmt.Errorf("workerSelector.matchLabels key is not a qualified label name: %s", strings.Join(errs, "; "))
		}
		if errs := validation.IsValidLabelValue(s.MatchLabels[k]); len(errs) != 0 {
			return fmt.Errorf("workerSelector.matchLabels value is not a label value: %s", strings.Join(errs, "; "))
		}
	}
	return nil
}

// maxStorageLocation is kmx's limit: it leaves room under substrate's 1024-byte storage_location
// for the "/<template>/" a render appends to a location with no trailing slash (a template name is
// at most 63 bytes).
const maxStorageLocation = 1024 - 65

var uriScheme = regexp.MustCompile(`^[a-z][a-z0-9+.-]*$`)

// checkStorageLocation applies kmx's snapshot.storageLocation rule: an object-storage base URI
// (gs://bucket/prefix/) with a scheme and bucket, no credentials, query or fragment, no '\', and no
// empty path segments or segments of only '.' and ' '. Object keys are not filesystem paths, and
// some storage backends trim dots and spaces from segments or treat '\' as a separator, so these are
// refused rather than resolved; one trailing '/' is allowed. Its errors never contain the location.
func checkStorageLocation(location string) error {
	const msg = "snapshot.storageLocation must be an object-storage URI with a scheme and bucket, no credentials, query or fragment, no '\\', and no empty path segments or segments of only '.' and ' '"
	if location == "" {
		return errors.New("snapshot.storageLocation is required")
	}
	if len(location) > maxStorageLocation || strings.ContainsFunc(location, func(r rune) bool { return r <= ' ' || r == 0x7f }) {
		return errors.New(msg)
	}
	u, err := url.Parse(location)
	if err != nil || !uriScheme.MatchString(u.Scheme) || u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || strings.Contains(location, "#") {
		return errors.New(msg)
	}
	// u.Path is decoded, so percent-encoded dots, spaces and '\' are refused too.
	if strings.Contains(u.Path, `\`) {
		return errors.New(msg)
	}
	if u.Path != "" && u.Path != "/" {
		for _, segment := range strings.Split(strings.TrimSuffix(strings.TrimPrefix(u.Path, "/"), "/"), "/") {
			if strings.Trim(segment, ". ") == "" {
				return errors.New(msg)
			}
		}
	}
	return nil
}

// ErrModelMismatch classifies a binding that serves a model other than the one an agent pins.
var ErrModelMismatch = errors.New("the binding serves a model other than the suite's pinned model")

// ModelMismatchError names both models. Model ids are identities, not credentials.
type ModelMismatchError struct{ Suite, Agent, Pinned, Served string }

func (e *ModelMismatchError) Error() string {
	return fmt.Sprintf("suite %s agent %s pins model %q but the binding serves %q; a different model needs a new suite version and a new evaluation",
		e.Suite, e.Agent, e.Pinned, e.Served)
}

func (e *ModelMismatchError) Is(target error) bool { return target == ErrModelMismatch }

// CheckModel refuses a binding whose model.name is not exactly the model the composition's agent
// pins: no case folding, prefix or version matching, as kmx's eval and lift check it.
func CheckModel(c *Composition, b *Binding) error {
	if b.Model.Name != c.Agent.Model.Model {
		return &ModelMismatchError{Suite: c.Suite, Agent: c.Agent.ID, Pinned: c.Agent.Model.Model, Served: b.Model.Name}
	}
	return nil
}

// Gates returns the binding's approval edges.
func (b *Binding) Gates() []invoke.Gate { return slices.Clone(b.Approval.Invokes) }

// ApprovalTargets returns the agents whose calls by caller the binding gates, sorted.
func (b *Binding) ApprovalTargets(caller string) []string {
	var out []string
	for _, g := range b.Approval.Invokes {
		if g.Caller == caller {
			out = append(out, g.Agent)
		}
	}
	slices.Sort(out)
	return out
}

// DaemonModelFlags are the agentsessionsd flags that serve the binding's model: -model, and
// -model-base-url when the binding sets one (the daemon's default otherwise). The daemon's
// -model-path keeps its default, /chat/completions, which it appends to the base URL's path.
func (b *Binding) DaemonModelFlags() []string {
	flags := []string{"-model=" + b.Model.Name}
	if b.Model.BaseURL != "" {
		flags = append(flags, "-model-base-url="+b.Model.BaseURL)
	}
	return flags
}

// hidden ends every refusal of a value that may hold a credential.
const hidden = " (the value is not shown, since it may hold a credential)"

// CheckModelBaseURL refuses a model base URL that is not http(s) with a host, or that carries
// userinfo, a query or a fragment: it is printed, logged and rendered into the daemon's arguments,
// so a credential in it would bypass the Secret. A path is allowed (Azure OpenAI's v1 endpoint is
// https://<resource>.openai.azure.com/openai/v1). Its errors never contain the URL or any part of
// it: a parse error can quote the userinfo of a URL that does not parse
// ("https://user:secret/x@host" reads "secret" as the port).
func CheckModelBaseURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return errors.New("the base URL is not a valid URL" + hidden)
	}
	switch {
	case u.User != nil:
		return errors.New("the base URL carries userinfo (user:password@); pass the provider's key in MODEL_API_KEY, which up.sh stores in a Secret" + hidden)
	case u.RawQuery != "" || u.ForceQuery:
		return errors.New("the base URL carries a query; a credential there would be printed and rendered into the Deployment, so pass the provider's key in MODEL_API_KEY (Azure OpenAI's v1 endpoint takes no api-version)" + hidden)
	case strings.Contains(raw, "#"):
		return errors.New("the base URL carries a fragment, which the model client drops" + hidden)
	case u.Scheme != "http" && u.Scheme != "https":
		// url.Parse lowercases the scheme. A value like "user:secret@host" parses as scheme "user".
		return errors.New("the base URL is not http or https" + hidden)
	case u.Hostname() == "":
		// Hostname, not Host, as kmx checks it: "https://:443" has a port and no host.
		return errors.New("the base URL has no host (want scheme://host[/path])" + hidden)
	}
	return nil
}
