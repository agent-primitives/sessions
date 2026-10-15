// Command emit renders one AgentSuite composition for an AgentSessions host on substrate.
//
//	emit bundle -suite DIR -agent ID -platform os/arch -out DIR
//	    stage the agent manifest and instructions for the harness image (the image's agent layer)
//	emit render -suite DIR -agent ID -platform os/arch -binding FILE -image REF@sha256:... -out DIR
//	    write actortemplate.json, harness-registration.json, agentsessionsd.args,
//	    agentsessionsd.model-args and template-name; refuse a binding that serves another model
//	    than the agent pins
//	emit check-binding -suite DIR [-agent ID] -binding FILE
//	    what render checks of the binding, before any image exists: the served model against each
//	    agent's pinned model (every agent when -agent is empty) and the approval edges against the
//	    suite's invokes edges
//	emit invokes -suite DIR -binding FILE
//	    print the suite's invokes edges, gated by the binding's approval edges, as AGENTSUITE_INVOKES
//	emit validate -suite DIR
//	    run kaimahi's agentsuite validator (the imported module) over the suite and print the suite's
//	    digest and the validator's version
//	emit check-model -base-url URL -model ID [-path PATH] [-auth-header NAME] [-api-key-set]
//	    refuse model settings agentsessionsd would refuse at startup, a URL that is not http(s), or
//	    a URL or path that could carry a credential
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"slices"
	"strings"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/kaimahi-agents/kaimahi/agentsuite"

	"github.com/aramase/agentsessions/examples/agentsuite-substrate/invoke"
	"github.com/aramase/agentsessions/examples/agentsuite-substrate/render"
	"github.com/aramase/agentsessions/internal/modelconfig"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: emit <bundle|render|check-binding|invokes|validate|check-model> [flags]")
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "bundle":
		err = bundle(os.Args[2:])
	case "render":
		err = renderCmd(os.Args[2:])
	case "invokes":
		err = invokesCmd(os.Args[2:], os.Stdout)
	case "check-binding":
		err = checkBinding(os.Args[2:], os.Stdout)
	case "validate":
		err = validate(os.Args[2:], os.Stdout)
	case "check-model":
		err = checkModel(os.Args[2:])
	default:
		fmt.Fprintln(os.Stderr, "usage: emit <bundle|render|check-binding|invokes|validate|check-model> [flags]")
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "emit:", err)
		os.Exit(1)
	}
}

type common struct{ suite, agent, platform, out string }

func flags(name string) (*flag.FlagSet, *common) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	c := &common{}
	fs.StringVar(&c.suite, "suite", "suite", "AgentSuite content directory")
	fs.StringVar(&c.agent, "agent", "", "agent id")
	fs.StringVar(&c.platform, "platform", "linux/amd64", "composition platform, os/arch")
	fs.StringVar(&c.out, "out", "", "output directory")
	return fs, c
}

// parse parses args into fs and refuses positional arguments; the flag package has printed the
// reason for a parse error.
func parse(fs *flag.FlagSet, args []string) error {
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("%s: unexpected arguments %q", fs.Name(), fs.Args())
	}
	return nil
}

func bundle(args []string) error {
	fs, c := flags("bundle")
	if err := parse(fs, args); err != nil {
		return err
	}
	comp, err := render.Load(c.suite, c.agent, c.platform)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(c.out, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(c.out, "agent.json"), comp.AgentJSON, 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(c.out, "instructions.md"), comp.Instructions, 0o644); err != nil {
		return err
	}
	fmt.Printf("staged agent %s of suite %s (%s, composition %s) in %s\n", comp.Agent.ID, comp.Suite, c.platform, comp.Digest, c.out)
	return nil
}

// kaimahiModule is the module that provides the agentsuite package.
const kaimahiModule = "github.com/kaimahi-agents/kaimahi"

// validate runs kaimahi's own validator (agentsuite.ValidateDirectory, from the imported module, not
// a copy) over a suite directory and prints three lines: the suite with its agents and compositions,
// the suite's digest, then the kaimahi module version the binary was built against. The digest is
// the JCS digest of agentsuite.json, which commits to every manifest under it: AgentSuite manifests
// have no version field, so it is the suite version.
func validate(args []string, w io.Writer) error {
	fs := flag.NewFlagSet("validate", flag.ContinueOnError)
	suite := fs.String("suite", "suite", "AgentSuite content directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("validate: unexpected arguments %q", fs.Args())
	}
	report, err := agentsuite.ValidateDirectory(*suite)
	if err != nil {
		return fmt.Errorf("validate suite %s: %w", *suite, err)
	}
	digest, err := suiteDigest(*suite)
	if err != nil {
		return err
	}
	var comps []string
	for _, s := range report.CompositionSelections {
		comps = append(comps, fmt.Sprintf("%s %s/%s profile %s", s.Agent, s.Platform.OS, s.Platform.Architecture, s.BuildProfile))
	}
	_, err = fmt.Fprint(w, validLines(report.Name, digest, moduleVersion(kaimahiModule), report.Agents, comps))
	return err
}

// suiteDigest is the JCS digest of dir's agentsuite.json.
func suiteDigest(dir string) (string, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "agentsuite.json"))
	if err != nil {
		return "", err
	}
	return agentsuite.Digest(raw)
}

// validLines is what validate prints for a valid suite, each line short enough for an 80-column
// terminal for a one-agent suite.
func validLines(suite, digest, version string, agents int, comps []string) string {
	noun := "agents"
	if agents == 1 {
		noun = "agent"
	}
	return fmt.Sprintf("suite %s valid: %d %s; compositions: %s\ndigest %s\nvalidated by %s/agentsuite %s\n",
		suite, agents, noun, strings.Join(comps, "; "), digest, kaimahiModule, version)
}

// moduleVersion is the version of module path this binary was built with, or "(unknown)".
func moduleVersion(path string) string {
	if bi, ok := debug.ReadBuildInfo(); ok {
		return depVersion(bi.Deps, path)
	}
	return "(unknown)"
}

// depVersion is the version of module path in deps: its replacement's version when it is replaced
// by another module version, "(local replace of <version>)" when it is replaced by a directory
// (which has no version), and "(unknown)" when deps do not list it.
func depVersion(deps []*debug.Module, path string) string {
	for _, d := range deps {
		if d.Path != path {
			continue
		}
		if d.Replace == nil {
			return d.Version
		}
		if d.Replace.Version != "" {
			return d.Replace.Version
		}
		return "(local replace of " + d.Version + ")"
	}
	return "(unknown)"
}

// invokesCmd prints the suite's invocation edges as agentsessionsd's AGENTSUITE_INVOKES value:
// caller=target:maxConcurrent:maxDepth[:approval], comma-separated, sorted. The binding's
// approval.invokes choose the gated edges; a gate on an edge the suite does not declare is refused.
func invokesCmd(args []string, w io.Writer) error {
	fs := flag.NewFlagSet("invokes", flag.ContinueOnError)
	suite := fs.String("suite", "suite", "AgentSuite content directory")
	binding := fs.String("binding", "", "AgentSessionsBinding file whose approval.invokes gate edges")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 || *binding == "" {
		return errors.New("invokes: want -suite DIR -binding FILE")
	}
	b, err := render.LoadBinding(*binding)
	if err != nil {
		return err
	}
	policy, err := suitePolicy(*suite, b)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(w, strings.Join(policy.Flag(), ","))
	return err
}

// suitePolicy is the suite's invocation policy with the binding's approval edges, which must all be
// edges the suite declares.
func suitePolicy(suite string, b *render.Binding) (invoke.Policy, error) {
	agents, err := render.Agents(suite)
	if err != nil {
		return nil, err
	}
	policy, err := invoke.FromAgents(agents, b.Gates())
	if err != nil {
		return nil, fmt.Errorf("binding: %w", err)
	}
	return policy, nil
}

// checkBinding checks what render checks of a binding, before an image exists: the binding's
// served model against the pinned model of the agent (every agent of the suite when -agent is
// empty) and its approval edges against the suite's invokes edges.
func checkBinding(args []string, w io.Writer) error {
	fs := flag.NewFlagSet("check-binding", flag.ContinueOnError)
	suite := fs.String("suite", "suite", "AgentSuite content directory")
	agentID := fs.String("agent", "", "agent id; empty checks every agent of the suite")
	platform := fs.String("platform", "linux/amd64", "composition platform, os/arch")
	binding := fs.String("binding", "", "AgentSessionsBinding file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 || *binding == "" {
		return errors.New("check-binding: want -suite DIR [-agent ID] -binding FILE")
	}
	b, err := render.LoadBinding(*binding)
	if err != nil {
		return err
	}
	if _, err := suitePolicy(*suite, b); err != nil {
		return err
	}
	ids := []string{*agentID}
	if *agentID == "" {
		agents, err := render.Agents(*suite)
		if err != nil {
			return err
		}
		ids = ids[:0]
		for _, a := range agents {
			ids = append(ids, a.ID)
		}
	}
	for _, id := range ids {
		comp, err := render.Load(*suite, id, *platform)
		if err != nil {
			return err
		}
		if err := render.CheckModel(comp, b); err != nil {
			return err
		}
		for _, t := range b.ApprovalTargets(id) {
			if !slices.ContainsFunc(comp.Agent.Invokes, func(inv agentsuite.AgentInvoke) bool { return inv.Agent == t }) {
				return fmt.Errorf("approval for %q: agent %s has no invokes edge to it", t, id)
			}
		}
	}
	_, err = fmt.Fprintf(w, "binding %s serves %s for %s of suite %s\n", b.Digest(), b.Model.Name, strings.Join(ids, ", "), *suite)
	return err
}

func renderCmd(args []string) error {
	fs, c := flags("render")
	binding := fs.String("binding", "binding.json", "AgentSessionsBinding file")
	image := fs.String("image", "", "digest-pinned harness image, name@sha256:...")
	tier := fs.String("tier", "stateless", "stateless (STATELESS_REPLAY, harness <agent>) or memory (REQUIRES_MEMORY_SNAPSHOT, conversation in RAM, harness <agent>-memory)")
	actors := fs.String("actors", "", "atespace agentsessionsd creates this harness's session actors in (,actors= on the daemon flag); empty uses the daemon's -substrate-atespace")
	if err := parse(fs, args); err != nil {
		return err
	}
	comp, err := render.Load(c.suite, c.agent, c.platform)
	if err != nil {
		return err
	}
	b, err := render.LoadBinding(*binding)
	if err != nil {
		return err
	}
	// The approval edges of other agents are checked against the suite too, so a binding is
	// refused by every render, not only by the one of the agent a bad edge names.
	if _, err := suitePolicy(c.suite, b); err != nil {
		return err
	}
	if *tier != "stateless" && *tier != "memory" {
		return fmt.Errorf("-tier %q: want stateless or memory", *tier)
	}
	out, err := render.Render(comp, *image, b, render.Options{Memory: *tier == "memory", ActorAtespace: *actors})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(c.out, 0o755); err != nil {
		return err
	}
	write := func(name string, m proto.Message) error {
		data, err := stableJSON(m)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(c.out, name), data, 0o644)
	}
	if err := write("actortemplate.json", out.Template); err != nil {
		return err
	}
	if err := write("harness-registration.json", out.Registration); err != nil {
		return err
	}
	name := out.Template.GetMetadata().GetName()
	if err := os.WriteFile(filepath.Join(c.out, "template-name"), []byte(name+"\n"), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(c.out, "agentsessionsd.args"), []byte("-substrate-harness="+out.DaemonFlag+"\n"), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(c.out, "agentsessionsd.model-args"), []byte(strings.Join(out.ModelFlags, "\n")+"\n"), 0o644); err != nil {
		return err
	}
	fmt.Printf("ActorTemplate %s/%s (image %s, binding %s)\nharness %s -> -substrate-harness=%s\nmodel %s, pinned by the suite and served by the binding\n",
		out.Template.GetMetadata().GetAtespace(), name, *image, b.Digest(), out.Registration.GetMetadata().GetName(), out.DaemonFlag, b.Model.Name)
	return nil
}

// stableJSON is m as protojson writes it (field order, names and values), re-indented by
// encoding/json with two spaces and a trailing newline. protojson deliberately varies its whitespace
// between builds, so its own Multiline output differs from run to run; this form is the same bytes
// for the same message, which lets a rendered file be diffed, compared and cited by line.
func stableJSON(m proto.Message) ([]byte, error) {
	data, err := protojson.Marshal(m)
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	if err := json.Indent(&out, data, "", "  "); err != nil {
		return nil, err
	}
	out.WriteByte('\n')
	return out.Bytes(), nil
}

// checkModel refuses model settings agentsessionsd would refuse at startup, so up.sh can stop before
// the Deployment replaces the running pod and its emptyDir journal with one that never starts. It
// builds the client with the constructor the daemon uses (internal/modelconfig), with the same base
// URL, model, path and auth header up.sh renders into deploy/agentsessionsd.yaml, so it refuses
// exactly the settings the daemon would. -api-key-set says MODEL_API_KEY is set, without passing
// the key: the constructor then refuses a plain-http URL to a host that is not loopback, localhost
// or an in-cluster service, as the daemon does with the key from the Secret.
//
// The demo also requires an http or https URL with a host, which the client itself does not, and
// refuses one with userinfo, a query or a fragment: up.sh prints the URL and renders it into the
// Deployment's arguments and a manifest on disk, so a credential in it would bypass the Secret that
// holds MODEL_API_KEY. The path is rendered the same way and the daemon logs it, so its only query
// may be a date-shaped api-version (what Azure OpenAI pins there); see render.CheckModelPath. These checks run
// before the client is built, and their errors do not repeat the URL or the path, since either may
// hold a credential.
func checkModel(args []string) error {
	fs := flag.NewFlagSet("check-model", flag.ContinueOnError)
	baseURL := fs.String("base-url", "", "the -model-base-url agentsessionsd will be given")
	model := fs.String("model", "", "the -model agentsessionsd will be given")
	path := fs.String("path", "", "the -model-path agentsessionsd will be given; empty is the daemon's default")
	authHeader := fs.String("auth-header", "Authorization", "the -model-auth-header agentsessionsd will be given")
	keySet := fs.Bool("api-key-set", false, "agentsessionsd will be given a MODEL_API_KEY")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("check-model: unexpected arguments %q", fs.Args())
	}
	if *baseURL == "" {
		return errors.New("check-model: -base-url is required")
	}
	if err := render.CheckModelBaseURL(*baseURL); err != nil {
		return fmt.Errorf("check-model: %w", err)
	}
	if *path != "" {
		if err := render.CheckModelPath(*path); err != nil {
			return fmt.Errorf("check-model: %w", err)
		}
	}
	if !headerName.MatchString(*authHeader) {
		return errors.New("check-model: the auth header is not an HTTP header name (letters, digits and '-')")
	}
	cfg := modelconfig.Config{Model: *model, BaseURL: *baseURL, Path: *path, AuthHeader: *authHeader}
	if *keySet {
		cfg.APIKey = "set" // a stand-in: the constructor only checks that one is present
	}
	if _, err := modelconfig.New(cfg); err != nil {
		return err
	}
	return nil
}

// headerName is the header names the demo accepts for -model-auth-header: a subset of the HTTP
// token characters, enough for Authorization, api-key and x-api-key.
var headerName = regexp.MustCompile(`^[A-Za-z0-9-]+$`)
