package main

import (
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/agent-primitives/sessions/api"
	v1 "github.com/agent-primitives/sessions/api/genpb"
	"github.com/agent-primitives/sessions/client"
	"github.com/agent-primitives/sessions/harness/echoagent"
	"github.com/agent-primitives/sessions/placement"
	"github.com/agent-primitives/sessions/runtime/substrate"
	"github.com/agent-primitives/sessions/session"
	"github.com/agent-primitives/sessions/sqlitelog"
)

// volumesTemplate is a substrate control plane whose actor template only gives VOLUMES snapshots.
type volumesTemplate struct {
	mu     sync.Mutex
	actors map[string]substrate.ActorStatus
	// captures counts each actor's captures, for CaptureLineage and NoCapture.
	captures map[string]int
}

// info is the actor's state as substrate reports it; v.mu must be held. Its CaptureLineage names
// the capture a suspend would take, or the one a suspended actor holds.
func (v *volumesTemplate) info(name string) substrate.ActorInfo {
	n := v.captures[name]
	if v.actors[name] != substrate.StatusSuspended {
		n++
	}
	info := substrate.ActorInfo{Status: v.actors[name], Worker: "w", UID: "uid-" + name, CaptureLineage: fmt.Sprintf("capture-%d", n), NoCapture: v.captures[name] == 0}
	if info.Status == substrate.StatusSuspended {
		info.Snapshot, info.SnapshotFidelity = "gs://snap/"+name, substrate.SnapshotFidelityVolumes
	}
	return info
}

func (v *volumesTemplate) CreateActor(_ context.Context, a substrate.ActorRef, _ substrate.ObjectRef) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if _, ok := v.actors[a.Name]; ok {
		return fmt.Errorf("%w: %q", substrate.ErrActorExists, a.Name)
	}
	v.actors[a.Name] = substrate.StatusUnknown
	return nil
}

func (v *volumesTemplate) ResumeActor(_ context.Context, a substrate.ActorRef) (substrate.ActorInfo, error) {
	return v.transition(a.Name, substrate.StatusRunning)
}

func (v *volumesTemplate) SuspendActor(_ context.Context, a substrate.ActorRef) (substrate.ActorInfo, error) {
	return v.transition(a.Name, substrate.StatusSuspended)
}

func (v *volumesTemplate) transition(name string, st substrate.ActorStatus) (substrate.ActorInfo, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if _, ok := v.actors[name]; !ok {
		return substrate.ActorInfo{}, substrate.ErrActorNotFound
	}
	if st == substrate.StatusSuspended && v.actors[name] != substrate.StatusSuspended {
		if v.captures == nil {
			v.captures = map[string]int{}
		}
		v.captures[name]++
	}
	v.actors[name] = st
	return v.info(name), nil
}

func (v *volumesTemplate) DeleteActor(_ context.Context, a substrate.ActorRef) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	delete(v.actors, a.Name)
	return nil
}

func (v *volumesTemplate) GetActor(_ context.Context, a substrate.ActorRef) (substrate.ActorInfo, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if _, ok := v.actors[a.Name]; !ok {
		return substrate.ActorInfo{}, substrate.ErrActorNotFound
	}
	return v.info(a.Name), nil
}

// Developer journey A4: `agentctl suspend` and `agentctl resume` of a memory-tier session whose
// template only gives VOLUMES fidelity fail with FAILED_PRECONDITION, and the error agentctl prints
// names the session, the actor, and the required and observed fidelity, but not the snapshot URI.
func TestLifecycleFidelityRefusalNamesSessionActorAndFidelity(t *testing.T) {
	desc, err := echoagent.Harness{}.Describe(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	desc.ID = "mem"
	desc.Capabilities.Resumability = api.ResumabilityRequiresMemorySnapshot
	backend := substrate.New(&volumesTemplate{actors: map[string]substrate.ActorStatus{}}, "space",
		substrate.ObjectRef{Atespace: "space", Name: "mem"}, desc)
	dial := placement.WithDialer(func(api.Incarnation) (api.Harness, func() error, error) {
		return echoagent.Harness{}, func() error { return nil }, nil
	})
	registry, err := placement.NewRegistry("mem", map[string]*placement.Placer{"mem": placement.New(backend, echoagent.Model, dial)})
	if err != nil {
		t.Fatal(err)
	}
	store, err := sqlitelog.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer()
	v1.RegisterSessionsServer(srv, session.NewService(store, registry))
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	c, err := client.Dial(lis.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	turn, err := c.Exec(t.Context(), client.ExecOptions{Inputs: []string{"hi"}})
	if err != nil {
		t.Fatal(err)
	}
	sess := turn.Session.GetMetadata().GetUid()

	for _, which := range []string{"suspend", "resume"} {
		err := lifecycle([]string{"--server", lis.Addr().String(), "--session", sess}, which)
		if status.Code(err) != codes.FailedPrecondition {
			t.Fatalf("agentctl %s: %v, want FAILED_PRECONDITION", which, err)
		}
		var stderr strings.Builder
		printError(&stderr, err) // the line main prints before exiting 1
		out := stderr.String()
		t.Logf("agentctl %s --session %s\n%s", which, sess, out)
		for _, want := range []string{`session "` + sess + `"`, "actor space/" + sess, "required MEMORY", "observed VOLUMES"} {
			if !strings.Contains(out, want) {
				t.Errorf("agentctl %s output %q does not name %q", which, out, want)
			}
		}
		if strings.Contains(out, "gs://snap/") {
			t.Errorf("agentctl %s output exposes the snapshot URI: %q", which, out)
		}
	}
}
