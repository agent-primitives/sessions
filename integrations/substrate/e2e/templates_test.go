package e2e

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	atepb "go.ate.dev/substrate/pkg/proto/ateapipb"
)

// ActorTemplates are substrate API resources living in an atespace, not Kubernetes objects, so the
// suite creates the ones it needs through Control instead of the workflow applying manifests. The
// WorkerPools they select are still Kubernetes objects (deploy/substrate/*-workerpool.yaml).
//
// Both templates run the same harnessnode image (HARNESS_IMAGE, pinned by digest as substrate
// requires); HARNESS_KIND picks the echo or the counter harness inside it.

// harnessContainer is the harnessnode container every template runs.
func harnessContainer(kind string) *atepb.Container {
	c := &atepb.Container{
		Name:    "harness",
		Image:   env("HARNESS_IMAGE", ""),
		Command: []string{"/ko-app/harnessnode"},
		// ResumeActor blocks on this probe, so a placed actor's harness is already serving. It also
		// lets the template controller take the golden snapshot as soon as the harness is ready.
		WakeupProbe: &atepb.WakeupProbe{
			HttpGet:        &atepb.HttpGetAction{Path: "/readyz", Port: 8081},
			TimeoutSeconds: 120,
		},
	}
	if kind != "" {
		c.Env = []*atepb.EnvVar{{Name: "HARNESS_KIND", Value: kind}}
	}
	return c
}

// templateResources is every template's compute size. substrate v0.6.0 requires both limits on a
// template: the scheduler books them against worker capacity and boots a micro-VM at that size.
var templateResources = &atepb.ResourceRequirements{Limits: &atepb.Resources{
	CpuMillicores: 1000,
	MemoryBytes:   512 << 20,
}}

// tierSandbox is where one tier's template runs: the sandbox class ("microvm" or "gvisor") and the
// `workload` label of the WorkerPool that serves it (deploy/substrate/<tier>-<class>-workerpool.yaml).
// substrate v0.6.0 has no per-template sandbox config: the class is whatever the selected pool's
// worker runs, so the pool label is what pins it.
type tierSandbox struct {
	class string
	pool  string
}

// sandboxFor resolves a tier's sandbox from <PREFIX>_SANDBOX_CLASS. Both tiers default to the
// micro-VM class, so the suite exercises the same isolation class end to end. gvisor is an explicit
// opt-in for hosts without KVM; it selects the tier's gVisor pool. Any other value is an error rather
// than a silent fallback, so a typo cannot run a tier on a class the caller did not ask for.
// <PREFIX>_SANDBOX_CONFIG, which named the template's SandboxConfig before v0.6.0, is refused for the
// same reason: the template cannot carry it, so the tier would run on the pool's config instead.
func sandboxFor(prefix, tier string) (tierSandbox, error) {
	if v := os.Getenv(prefix + "_SANDBOX_CONFIG"); v != "" {
		return tierSandbox{}, fmt.Errorf("%s_SANDBOX_CONFIG=%q: substrate v0.6.0 templates name no SandboxConfig; unset it and set configRef on the tier's WorkerPool sandboxClasses entry instead", prefix, v)
	}
	key := prefix + "_SANDBOX_CLASS"
	switch v := env(key, "microvm"); v {
	case "microvm", "gvisor":
		return tierSandbox{class: v, pool: "agentsessions-" + tier + "-" + v}, nil
	default:
		return tierSandbox{}, fmt.Errorf("%s=%q: want microvm or gvisor", key, v)
	}
}

// templateSpec builds a tier's ActorTemplate on the sandbox sandboxFor resolves. Every template
// prefers MEMORY fidelity, so a suspend captures the guest's RAM and the golden snapshot new actors
// start from is a running harness rather than a cold boot.
func templateSpec(t *testing.T, atespace, name, prefix, tier, harnessKind string) *atepb.ActorTemplate {
	t.Helper()
	sb, err := sandboxFor(prefix, tier)
	if err != nil {
		t.Fatal(err)
	}
	return &atepb.ActorTemplate{
		Metadata:       &atepb.ResourceMetadata{Atespace: atespace, Name: name},
		WorkerSelector: &atepb.Selector{MatchLabels: map[string]string{"workload": sb.pool}},
		Containers:     []*atepb.Container{harnessContainer(harnessKind)},
		SnapshotConfig: &atepb.SnapshotConfig{
			PreferredFidelity: atepb.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY,
			// A preview field at v0.6.0 (PerTemplateSnapshotLocation): accepted but ignored while the
			// gate is off, so snapshots go under the cluster's --snapshot-location.
			StorageLocation: env("SNAPSHOT_LOCATION", "gs://ate-snapshots/agentsessions/") + name + "/",
		},
		Resources: templateResources,
	}
}

// echoTemplateSpec is the stateless tier: the echo harness. A STATELESS_REPLAY harness keeps nothing
// in RAM the journal lacks, so the snapshot scope barely matters. It runs on a micro-VM unless
// ECHO_SANDBOX_CLASS=gvisor.
func echoTemplateSpec(t *testing.T, atespace string) *atepb.ActorTemplate {
	return templateSpec(t, atespace, echoTemplate, "ECHO", "echo", "")
}

// counterTemplateSpec is the stateful tier: the in-RAM counter. MEMORY fidelity is load-bearing here:
// the count exists only in guest RAM, so only a memory snapshot carries it across suspend and fork.
// It runs on a micro-VM unless COUNTER_SANDBOX_CLASS=gvisor, whose checkpoint also captures
// memory.
func counterTemplateSpec(t *testing.T, atespace string) *atepb.ActorTemplate {
	return templateSpec(t, atespace, counterTemplate, "COUNTER", "counter", "counter")
}

var templateOnce sync.Map // template name -> *sync.Once, so each binary builds a template once

// ensureTemplate makes sure the template exists with this run's harness image and sandbox, that its
// golden snapshot is built, and returns its name. Waiting for the golden matters for more than speed:
// the template controller builds it on a temporary actor that holds one of the pool's workers until
// it is done.
func (f *fixture) ensureTemplate(t *testing.T, spec func(*testing.T, string) *atepb.ActorTemplate) string {
	t.Helper()
	want := spec(t, f.tmplAtespace)
	once, _ := templateOnce.LoadOrStore(want.GetMetadata().GetName(), &sync.Once{})
	var failure string
	once.(*sync.Once).Do(func() { failure = f.createTemplate(t, want) })
	if failure != "" {
		t.Fatal(failure)
	}
	return want.GetMetadata().GetName()
}

func (f *fixture) createTemplate(t *testing.T, want *atepb.ActorTemplate) string {
	if want.GetContainers()[0].GetImage() == "" {
		return "HARNESS_IMAGE is not set; the workflow passes the digest-pinned harnessnode image it built"
	}
	ctl := atepb.NewControlClient(f.conn)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	if err := ensureAtespace(ctx, ctl, f.tmplAtespace); err != nil {
		return "ensure template atespace: " + err.Error()
	}
	ref := &atepb.ObjectRef{Atespace: f.tmplAtespace, Name: want.GetMetadata().GetName()}
	got, err := ctl.GetActorTemplate(ctx, &atepb.GetActorTemplateRequest{ActorTemplate: ref})
	switch {
	case status.Code(err) == codes.NotFound:
		if _, err := ctl.CreateActorTemplate(ctx, &atepb.CreateActorTemplateRequest{ActorTemplate: want}); err != nil {
			return "create template " + ref.GetName() + ": " + err.Error()
		}
		t.Logf("created ActorTemplate %s/%s (image %s)", ref.GetAtespace(), ref.GetName(), want.GetContainers()[0].GetImage())
	case err != nil:
		return "get template " + ref.GetName() + ": " + err.Error()
	case got.GetContainers()[0].GetImage() != want.GetContainers()[0].GetImage():
		// Templates are immutable. Running against a stale image would test the wrong harness.
		return "ActorTemplate " + ref.GetAtespace() + "/" + ref.GetName() + " exists with image " +
			got.GetContainers()[0].GetImage() + ", not this run's " + want.GetContainers()[0].GetImage() +
			"; delete it (or use a fresh ACTORTEMPLATE_ATESPACE) and rerun"
	case got.GetWorkerSelector().GetMatchLabels()["workload"] != want.GetWorkerSelector().GetMatchLabels()["workload"]:
		// Same reason: a template left by a run on another pool would silently test that pool's
		// sandbox class.
		return "ActorTemplate " + ref.GetAtespace() + "/" + ref.GetName() + " exists on pool " +
			got.GetWorkerSelector().GetMatchLabels()["workload"] + ", not this run's " +
			want.GetWorkerSelector().GetMatchLabels()["workload"] +
			"; delete it (or use a fresh ACTORTEMPLATE_ATESPACE) and rerun"
	}
	for {
		got, err := ctl.GetActorTemplate(ctx, &atepb.GetActorTemplateRequest{ActorTemplate: ref})
		if err != nil {
			return "poll template " + ref.GetName() + ": " + err.Error()
		}
		golden := got.GetStatus().GetGoldenSnapshotStatus()
		if msg := golden.GetErrorMessage(); msg != "" {
			return "template " + ref.GetName() + " golden snapshot failed: " + msg
		}
		if golden.GetGoldenTag().GetName() != "" {
			t.Logf("ActorTemplate %s/%s golden snapshot ready (tag %s/%s)", ref.GetAtespace(), ref.GetName(),
				golden.GetGoldenTag().GetAtespace(), golden.GetGoldenTag().GetName())
			return ""
		}
		select {
		case <-ctx.Done():
			return "timed out waiting for template " + ref.GetName() + " golden snapshot"
		case <-time.After(5 * time.Second):
		}
	}
}

// TestSandboxFor pins the tier sandbox selection without a cluster: micro-VM by default, gVisor only
// when asked for, and an unknown class or a SandboxConfig override refused rather than dropped.
func TestSandboxFor(t *testing.T) {
	for _, tc := range []struct {
		name, class, config string
		want                tierSandbox
		wantErr             bool
	}{
		{name: "default", want: tierSandbox{"microvm", "agentsessions-echo-microvm"}},
		{name: "microvm", class: "microvm", want: tierSandbox{"microvm", "agentsessions-echo-microvm"}},
		{name: "gvisor opt-in", class: "gvisor", want: tierSandbox{"gvisor", "agentsessions-echo-gvisor"}},
		{name: "wrong case", class: "gVisor", wantErr: true},
		{name: "unknown", class: "kata", wantErr: true},
		{name: "config override", config: "microvm", wantErr: true},
		{name: "config override on gvisor", class: "gvisor", config: "gvisor-default", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("ECHO_SANDBOX_CLASS", tc.class)
			t.Setenv("ECHO_SANDBOX_CONFIG", tc.config)
			got, err := sandboxFor("ECHO", "echo")
			if tc.wantErr {
				if err == nil {
					t.Fatalf("sandboxFor = %+v, want an error", got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("sandboxFor = %+v, want %+v", got, tc.want)
			}
		})
	}
}
