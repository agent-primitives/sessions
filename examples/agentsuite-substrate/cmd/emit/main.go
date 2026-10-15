// Command emit renders one AgentSuite composition for an AgentSessions host on substrate.
//
//	emit bundle -suite DIR -agent ID -platform os/arch -out DIR
//	    stage the agent manifest and instructions for the harness image (the image's agent layer)
//	emit render -suite DIR -agent ID -platform os/arch -binding FILE -image REF@sha256:... [-model ID] -out DIR
//	    write actortemplate.json, harness-registration.json, agentsessionsd.args and template-name
//	emit validate -suite DIR
//	    run kaimahi's agentsuite validator (the imported module) over the suite and print its version
//	emit check-model -base-url URL -model ID [-path PATH] [-auth-header NAME] [-api-key-set]
//	    refuse model settings agentsessionsd would refuse at startup, a URL that is not http(s), or
//	    a URL or path that could carry a credential
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"strings"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/kaimahi-agents/kaimahi/agentsuite"

	"github.com/aramase/agentsessions/examples/agentsuite-substrate/render"
	"github.com/aramase/agentsessions/internal/modelconfig"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: emit <bundle|render|validate|check-model> [flags]")
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "bundle":
		err = bundle(os.Args[2:])
	case "render":
		err = renderCmd(os.Args[2:])
	case "validate":
		err = validate(os.Args[2:], os.Stdout)
	case "check-model":
		err = checkModel(os.Args[2:])
	default:
		fmt.Fprintln(os.Stderr, "usage: emit <bundle|render|validate|check-model> [flags]")
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "emit:", err)
		os.Exit(1)
	}
}

type common struct{ suite, agent, platform, out string }

func flags(name string) (*flag.FlagSet, *common) {
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	c := &common{}
	fs.StringVar(&c.suite, "suite", "suite", "AgentSuite content directory")
	fs.StringVar(&c.agent, "agent", "", "agent id")
	fs.StringVar(&c.platform, "platform", "linux/amd64", "composition platform, os/arch")
	fs.StringVar(&c.out, "out", "", "output directory")
	return fs, c
}

func bundle(args []string) error {
	fs, c := flags("bundle")
	_ = fs.Parse(args)
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
// a copy) over a suite directory and prints two lines: the suite with its agents and compositions,
// then the kaimahi module version the binary was built against.
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
	var comps []string
	for _, s := range report.CompositionSelections {
		comps = append(comps, fmt.Sprintf("%s %s/%s profile %s", s.Agent, s.Platform.OS, s.Platform.Architecture, s.BuildProfile))
	}
	_, err = fmt.Fprint(w, validLines(report.Name, moduleVersion(kaimahiModule), report.Agents, comps))
	return err
}

// validLines is what validate prints for a valid suite, each line short enough for an 80-column
// terminal for a one-agent suite.
func validLines(suite, version string, agents int, comps []string) string {
	noun := "agents"
	if agents == 1 {
		noun = "agent"
	}
	return fmt.Sprintf("suite %s valid: %d %s; compositions: %s\nvalidated by %s/agentsuite %s\n",
		suite, agents, noun, strings.Join(comps, "; "), kaimahiModule, version)
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

func renderCmd(args []string) error {
	fs, c := flags("render")
	binding := fs.String("binding", "binding.json", "AgentSessionsBinding file")
	image := fs.String("image", "", "digest-pinned harness image, name@sha256:...")
	model := fs.String("model", "", "provider model the harness requests instead of the agent's own; empty keeps the agent's")
	tier := fs.String("tier", "stateless", "stateless (STATELESS_REPLAY, harness <agent>) or memory (REQUIRES_MEMORY_SNAPSHOT, conversation in RAM, harness <agent>-memory)")
	_ = fs.Parse(args)
	comp, err := render.Load(c.suite, c.agent, c.platform)
	if err != nil {
		return err
	}
	b, err := render.LoadBinding(*binding)
	if err != nil {
		return err
	}
	if *tier != "stateless" && *tier != "memory" {
		return fmt.Errorf("-tier %q: want stateless or memory", *tier)
	}
	out, err := render.Render(comp, *image, b, render.Options{Model: *model, Memory: *tier == "memory"})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(c.out, 0o755); err != nil {
		return err
	}
	write := func(name string, m proto.Message) error {
		data, err := protojson.MarshalOptions{Multiline: true, Indent: "  "}.Marshal(m)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(c.out, name), append(data, '\n'), 0o644)
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
	fmt.Printf("ActorTemplate %s/%s (image %s, binding %s)\nharness %s -> -substrate-harness=%s\n",
		out.Template.GetMetadata().GetAtespace(), name, *image, b.Digest(), out.Registration.GetMetadata().GetName(), out.DaemonFlag)
	return nil
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
// may be a date-shaped api-version (what Azure OpenAI pins there); see checkPath. These checks run
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
	if err := checkBaseURL(*baseURL); err != nil {
		return err
	}
	if *path != "" {
		if err := checkPath(*path); err != nil {
			return err
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

// hidden ends every refusal of a value that may hold a credential.
const hidden = " (the value is not shown, since it may hold a credential)"

// headerName is the header names the demo accepts for -model-auth-header: a subset of the HTTP
// token characters, enough for Authorization, api-key and x-api-key.
var headerName = regexp.MustCompile(`^[A-Za-z0-9-]+$`)

// apiVersion is the api-version value the demo accepts in a model path: a date, optionally with
// Azure OpenAI's -preview suffix. Its shape leaves no room for a key.
var apiVersion = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}(-preview)?$`)

// checkBaseURL refuses a base URL the demo will not print or render; see checkModel. Its errors
// never contain the URL or any part of it: a parse error can quote the userinfo of a URL that
// does not parse ("https://user:secret/x@host" reads "secret" as the port).
func checkBaseURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return errors.New("check-model: the base URL is not a valid URL" + hidden)
	}
	switch {
	case u.User != nil:
		return errors.New("check-model: the base URL carries userinfo (user:password@); pass the provider's key in MODEL_API_KEY, which up.sh stores in a Secret" + hidden)
	case u.RawQuery != "" || u.ForceQuery:
		return errors.New("check-model: the base URL carries a query; a credential there would be printed and rendered into the Deployment, so pass the provider's key in MODEL_API_KEY and an api-version in AGENTSUITE_MODEL_PATH" + hidden)
	case strings.Contains(raw, "#"):
		return errors.New("check-model: the base URL carries a fragment, which the model client drops" + hidden)
	case u.Scheme != "http" && u.Scheme != "https":
		// url.Parse lowercases the scheme. A value like "user:secret@host" parses as scheme "user".
		return errors.New("check-model: the base URL is not http or https" + hidden)
	case u.Host == "":
		return errors.New("check-model: the base URL has no host (want scheme://host[/path])" + hidden)
	}
	return nil
}

// checkPath refuses a completions path the demo will not render or let the daemon log; see
// checkModel. The path must be one absolute path ("//host" would read as an authority), with no '@'
// or fragment, and its only query parameter may be a single api-version whose value is a date
// (2024-10-21, 2025-04-01-preview): an Azure OpenAI deployment needs it there, and that shape cannot
// hold a key. Any other parameter (Azure's own api-key, a signature) is refused. Like checkBaseURL,
// its errors never contain the path.
func checkPath(raw string) error {
	switch {
	case !strings.HasPrefix(raw, "/") || strings.HasPrefix(raw, "//"):
		return errors.New("check-model: the model path does not start with a single '/' (want /path[?api-version=YYYY-MM-DD])" + hidden)
	case strings.Contains(raw, "#"):
		return errors.New("check-model: the model path carries a fragment, which the model client drops" + hidden)
	case strings.Contains(raw, "@"):
		return errors.New("check-model: the model path contains '@', which could be read as userinfo" + hidden)
	case strings.HasSuffix(raw, ":"):
		// up.sh renders the path into a YAML plain scalar, where a trailing ':' makes a map key.
		return errors.New("check-model: the model path ends with ':'" + hidden)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return errors.New("check-model: the model path is not a valid URL path" + hidden)
	}
	if u.ForceQuery && u.RawQuery == "" {
		return errors.New("check-model: the model path ends with an empty query" + hidden)
	}
	q, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return errors.New("check-model: the model path's query does not parse" + hidden)
	}
	for k, vs := range q {
		if k != "api-version" {
			return errors.New("check-model: the model path carries a query parameter other than api-version; pass the provider's key in MODEL_API_KEY" + hidden)
		}
		if len(vs) != 1 || !apiVersion.MatchString(vs[0]) {
			return errors.New("check-model: the model path's api-version is not one date (YYYY-MM-DD, optionally -preview)" + hidden)
		}
	}
	return nil
}
