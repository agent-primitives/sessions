// Command lifecycle-ui serves the session lifecycle demo page: a browser view of one session's
// create, exec, suspend, resume, fork and replay against the agentsessionsd Deployments up.sh brings
// up, with the request's path through substrate drawn and lit from the components' own logs.
//
// It is a loopback bridge, not a service: the browser calls it over HTTP, and it calls the Sessions
// gRPC API of each daemon (through the port-forwards ui.sh starts), reads the journal over Replay,
// and reads actor state and logs with kubectl and the kubectl-ate plugin, verbs get and logs only.
// It authenticates nobody. See examples/session-lifecycle-ui/README.md.
//
//	lifecycle-ui [flags]                       serve the page
//	lifecycle-ui inject -state DIR -out DIR    derive the VOLUMES-template daemon (see inject.go)
//	lifecycle-ui check -url URL                drive a running page through every call (see check.go)
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	if len(os.Args) > 1 {
		sub := map[string]func([]string) error{"inject": runInject, "check": runCheck}[os.Args[1]]
		if sub != nil {
			if err := sub(os.Args[2:]); err != nil {
				fmt.Fprintf(os.Stderr, "lifecycle-ui %s: %v\n", os.Args[1], err)
				os.Exit(1)
			}
			return
		}
	}
	cfg, err := parseFlags(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "lifecycle-ui:", err)
		os.Exit(2)
	}
	if err := serve(cfg); err != nil {
		fmt.Fprintln(os.Stderr, "lifecycle-ui:", err)
		os.Exit(1)
	}
}

// config is everything the bridge is started with.
type config struct {
	Listen    string
	Project   string
	Atespace  string
	Namespace string
	// One Sessions API address per lane; an empty address leaves that lane off.
	Stateless, Memory, Volumes string
	// Harness is the stateless lane's harness; the memory and volumes lanes serve MemoryHarness.
	Harness, MemoryHarness string
	// Router is the atenet-router address replay drives the session's actor through; ModelURL is
	// where replay's model client points (it must never be called), ModelStats the fake model's
	// call counter (empty with a real provider).
	Router, ModelURL, ModelStats string
	// The cluster reads: kubectl and the kubectl-ate plugin binary, with the demo's kubeconfig and
	// context. An empty Kubectl turns them off.
	Kubectl, KubectlAte, Kubeconfig, Context string
}

func parseFlags(args []string) (config, error) {
	var c config
	fs := flag.NewFlagSet("lifecycle-ui", flag.ContinueOnError)
	fs.StringVar(&c.Listen, "listen", "127.0.0.1:8090", "loopback address to serve the page on")
	fs.StringVar(&c.Project, "project", "lifecycle-ui", "project the page creates and lists sessions in")
	fs.StringVar(&c.Atespace, "atespace", "agentsuite-demo", "atespace of the session actors")
	fs.StringVar(&c.Namespace, "namespace", "agentsuite-demo", "namespace of the agentsessionsd Deployments and worker pods")
	fs.StringVar(&c.Stateless, "stateless", "", "Sessions API address of agentsessionsd (the stateless tier)")
	fs.StringVar(&c.Memory, "memory", "", "Sessions API address of agentsessionsd-memory (the memory tier)")
	fs.StringVar(&c.Volumes, "volumes", "", "Sessions API address of agentsessionsd-volumes (the memory tier on a VOLUMES template)")
	fs.StringVar(&c.Harness, "harness", "triage", "harness the stateless tier serves")
	fs.StringVar(&c.MemoryHarness, "memory-harness", "", "harness the memory and volumes tiers serve (default <harness>-memory)")
	fs.StringVar(&c.Router, "router", "", "atenet-router address for replay; empty turns replay off")
	fs.StringVar(&c.ModelURL, "model-base-url", "http://127.0.0.1:9/v1", "model endpoint replay's client points at; replay fails if it is called")
	fs.StringVar(&c.ModelStats, "model-stats", "", "fake model call counter URL; empty relies on the controller's own count")
	fs.StringVar(&c.Kubectl, "kubectl", "kubectl", "kubectl binary; empty turns the cluster reads off")
	fs.StringVar(&c.KubectlAte, "kubectl-ate", "", "kubectl-ate plugin binary (up.sh builds it into <work>/bin)")
	fs.StringVar(&c.Kubeconfig, "kubeconfig", "", "kubeconfig of the demo cluster")
	fs.StringVar(&c.Context, "context", "", "kubeconfig context of the demo cluster")
	if err := fs.Parse(args); err != nil {
		return c, err
	}
	if fs.NArg() != 0 {
		return c, fmt.Errorf("unexpected arguments %q", fs.Args())
	}
	if c.MemoryHarness == "" {
		c.MemoryHarness = c.Harness + "-memory"
	}
	if err := checkLoopback(c.Listen); err != nil {
		return c, err
	}
	if c.Stateless == "" && c.Memory == "" && c.Volumes == "" {
		return c, errors.New("no Sessions API address: set -stateless, -memory or -volumes")
	}
	for _, v := range []struct{ name, value string }{
		{"-atespace", c.Atespace}, {"-namespace", c.Namespace}, {"-harness", c.Harness},
		{"-memory-harness", c.MemoryHarness}, {"-project", c.Project},
	} {
		if !safeName.MatchString(v.value) {
			return c, fmt.Errorf("%s %q: want letters, digits, '.', '_' or '-', starting with a letter or digit", v.name, v.value)
		}
	}
	return c, nil
}

// checkLoopback refuses a listen address that is not a loopback IP or localhost: the page drives
// sessions and spends model calls, and nothing in it authenticates the caller.
func checkLoopback(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("-listen %q: %w", addr, err)
	}
	if host == "localhost" {
		return nil
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return nil
	}
	return fmt.Errorf("-listen %q: want a loopback address (127.0.0.1, ::1 or localhost); the page authenticates no one", addr)
}

func serve(cfg config) error {
	s, err := newServer(cfg)
	if err != nil {
		return err
	}
	defer s.close()
	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: s.routes(), ReadHeaderTimeout: 5 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	log.Printf("session lifecycle page: http://%s", ln.Addr())
	for _, l := range s.lanes {
		log.Printf("lane %s: harness %s, Sessions API %s", l.Name, l.Harness, l.Addr)
	}
	if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
