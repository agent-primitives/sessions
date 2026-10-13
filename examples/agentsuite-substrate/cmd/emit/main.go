// Command emit renders one AgentSuite composition for an AgentSessions host on substrate.
//
//	emit bundle -suite DIR -agent ID -platform os/arch -out DIR
//	    stage the agent manifest and instructions for the harness image (the image's agent layer)
//	emit render -suite DIR -agent ID -platform os/arch -binding FILE -image REF@sha256:... -out DIR
//	    write actortemplate.json, harness-registration.json, agentsessionsd.args and template-name
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/aramase/agentsessions/examples/agentsuite-substrate/render"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: emit <bundle|render> [flags]")
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "bundle":
		err = bundle(os.Args[2:])
	case "render":
		err = renderCmd(os.Args[2:])
	default:
		fmt.Fprintln(os.Stderr, "usage: emit <bundle|render> [flags]")
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
	_ = fs.Parse(args)
	comp, err := render.Load(c.suite, c.agent, c.platform)
	if err != nil {
		return err
	}
	b, err := render.LoadBinding(*binding)
	if err != nil {
		return err
	}
	out, err := render.Render(comp, *image, b)
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
