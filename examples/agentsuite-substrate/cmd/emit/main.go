// Command emit renders one AgentSuite composition for an AgentSessions host on substrate.
//
//	emit bundle -suite DIR -agent ID -platform os/arch -out DIR
//	    stage the agent manifest and instructions for the harness image (the image's agent layer)
//	emit render -suite DIR -agent ID -platform os/arch -binding FILE -image REF@sha256:... [-model ID] -out DIR
//	    write actortemplate.json, harness-registration.json, agentsessionsd.args and template-name
//	emit check-model -base-url URL -model ID
//	    refuse model settings agentsessionsd would refuse at startup, or a URL that is not http(s)
package main

import (
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/aramase/agentsessions/examples/agentsuite-substrate/render"
	"github.com/aramase/agentsessions/internal/modelconfig"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: emit <bundle|render|check-model> [flags]")
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "bundle":
		err = bundle(os.Args[2:])
	case "render":
		err = renderCmd(os.Args[2:])
	case "check-model":
		err = checkModel(os.Args[2:])
	default:
		fmt.Fprintln(os.Stderr, "usage: emit <bundle|render|check-model> [flags]")
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

func renderCmd(args []string) error {
	fs, c := flags("render")
	binding := fs.String("binding", "binding.json", "AgentSessionsBinding file")
	image := fs.String("image", "", "digest-pinned harness image, name@sha256:...")
	model := fs.String("model", "", "provider model the harness requests instead of the agent's own; empty keeps the agent's")
	_ = fs.Parse(args)
	comp, err := render.Load(c.suite, c.agent, c.platform)
	if err != nil {
		return err
	}
	b, err := render.LoadBinding(*binding)
	if err != nil {
		return err
	}
	out, err := render.Render(comp, *image, b, *model)
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
		out.Template.GetMetadata().GetAtespace(), name, *image, b.Digest(), comp.Agent.ID, out.DaemonFlag)
	return nil
}

// checkModel refuses model settings agentsessionsd would refuse at startup, so up.sh can stop before
// the Deployment replaces the running pod and its emptyDir journal with one that never starts. It
// builds the client with the constructor the daemon uses (internal/modelconfig), with the daemon's
// default -model-path, which deploy/agentsessionsd.yaml does not override, so it refuses exactly the
// base URLs and models the daemon would.
//
// The demo also requires an http or https URL with a host, which the client itself does not, and
// refuses one with userinfo, a query or a fragment: up.sh prints the URL and renders it into the
// Deployment's arguments and a manifest on disk, so a credential in it would bypass the Secret that
// holds MODEL_API_KEY. These checks run before the client is built, and their errors do not repeat
// the URL, since it may hold a credential.
func checkModel(args []string) error {
	fs := flag.NewFlagSet("check-model", flag.ContinueOnError)
	baseURL := fs.String("base-url", "", "the -model-base-url agentsessionsd will be given")
	model := fs.String("model", "", "the -model agentsessionsd will be given")
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
	if _, err := modelconfig.New(modelconfig.Config{Model: *model, BaseURL: *baseURL}); err != nil {
		return err
	}
	return nil
}

// checkBaseURL refuses a base URL the demo will not print or render; see checkModel. Its errors
// never contain the URL or any part of it: a parse error can quote the userinfo of a URL that
// does not parse ("https://user:secret/x@host" reads "secret" as the port).
func checkBaseURL(raw string) error {
	const hidden = " (the value is not shown, since it may hold a credential)"
	u, err := url.Parse(raw)
	if err != nil {
		return errors.New("check-model: the base URL is not a valid URL" + hidden)
	}
	switch {
	case u.User != nil:
		return errors.New("check-model: the base URL carries userinfo (user:password@); pass the provider's key in MODEL_API_KEY, which up.sh stores in a Secret" + hidden)
	case u.RawQuery != "" || u.ForceQuery:
		return errors.New("check-model: the base URL carries a query; a credential there would be printed and rendered into the Deployment, so pass the provider's key in MODEL_API_KEY" + hidden)
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
