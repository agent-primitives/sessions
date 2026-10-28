package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	atepb "go.ate.dev/substrate/pkg/proto/ateapipb"
	"google.golang.org/protobuf/encoding/protojson"
)

// The volumes lane: the memory-tier harness, unchanged, served by a third daemon from a template
// that only gives VOLUMES fidelity, so a suspend is refused with FAILED_PRECONDITION naming the
// session, the actor and both fidelities. `emit render -tier memory` refuses a non-MEMORY fidelity
// by design, so the template is derived from the memory tier's rendered one with three changes:
//
//   - snapshotConfig.preferredFidelity is VOLUMES, so a suspend completes a VOLUMES snapshot;
//   - one durableDir volume is mounted in the container: at substrate v0.6.0 ateworker-microvm
//     refuses a VOLUMES checkpoint with no durable-dir or CSI volume (cmd/ateworker-microvm/
//     checkpoint.go), and ate-api then marks the actor CRASHED (cmd/ateapi/internal/controlapi/
//     crash.go, handleAteletError), which would show a substrate crash rather than the sessions
//     refusal;
//   - the name (and the storage location under it) gets volumesSuffix, so it never collides with
//     the memory tier's template.
//
// Golden snapshots stay on: a golden actor suspends at MEMORY, so a session's first placement
// passes the fidelity check and only its first real suspend records VOLUMES. That is the order the
// page shows: a turn, then the refused suspend.
const (
	volumesSuffix     = "-vol"
	volumesVolume     = "lifecycle-data"
	volumesMountPath  = "/var/lib/lifecycle-data"
	volumesDeployment = "agentsessionsd-volumes"
	memoryDeployment  = "agentsessionsd-memory"
	// microVMPool is the `workload` label of the micro-VM WorkerPool (deploy/workerpool-microvm.yaml)
	// that binding-microvm.json selects. A template carries no sandbox class at substrate v0.6.0,
	// so the pool it selects is what says it runs on a micro-VM.
	microVMPool = "agentsuite-demo-microvm"
)

// runInject writes, from up.sh's rendered memory tier in -state, the VOLUMES template
// (actortemplate.json and template-name) and the volumes daemon's manifest
// (agentsessionsd-volumes.yaml) into -out. It changes nothing in the cluster; ui.sh applies them.
func runInject(args []string) error {
	fs := flag.NewFlagSet("inject", flag.ContinueOnError)
	state := fs.String("state", "", "up.sh's cluster state directory (<work>/cluster-<name>)")
	out := fs.String("out", "", "directory to write the template and the manifest to")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *state == "" || *out == "" || fs.NArg() != 0 {
		return errors.New("usage: lifecycle-ui inject -state DIR -out DIR")
	}
	mem := filepath.Join(*state, "render-memory")
	tmpl, err := os.ReadFile(filepath.Join(mem, "actortemplate.json"))
	if err != nil {
		return fmt.Errorf("%w (run up.sh first)", err)
	}
	daemonFlag, err := os.ReadFile(filepath.Join(mem, "agentsessionsd.args"))
	if err != nil {
		return err
	}
	manifest, err := os.ReadFile(filepath.Join(*state, "agentsessionsd-memory.yaml"))
	if err != nil {
		return fmt.Errorf("%w (run up.sh first)", err)
	}
	t, err := volumesTemplate(tmpl, string(daemonFlag))
	if err != nil {
		return err
	}
	m, err := volumesManifest(string(manifest), string(daemonFlag), t.flag)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		return err
	}
	for name, data := range map[string][]byte{
		"actortemplate.json":          t.template,
		"template-name":               []byte(t.name + "\n"),
		"agentsessionsd-volumes.yaml": []byte(m),
	} {
		if err := os.WriteFile(filepath.Join(*out, name), data, 0o644); err != nil {
			return err
		}
	}
	fmt.Printf("template %s (VOLUMES), daemon flag %s\n", t.name, t.flag)
	return nil
}

type derived struct {
	name     string
	template []byte
	flag     string
}

// volumesTemplate derives the VOLUMES template from the memory tier's rendered actortemplate.json
// and its agentsessionsd.args line, refusing anything but the micro-VM memory tier it expects.
func volumesTemplate(memoryTemplate []byte, memoryFlag string) (*derived, error) {
	var t atepb.ActorTemplate
	if err := protojson.Unmarshal(memoryTemplate, &t); err != nil {
		return nil, fmt.Errorf("memory-tier template: %w", err)
	}
	if got := t.GetSnapshotConfig().GetPreferredFidelity(); got != atepb.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY {
		return nil, fmt.Errorf("memory-tier template prefers %s, want SNAPSHOT_FIDELITY_MEMORY", got)
	}
	if got := t.GetWorkerSelector().GetMatchLabels()["workload"]; got != microVMPool {
		return nil, fmt.Errorf("memory-tier template selects pool %q, not the micro-VM pool %q; the volumes lane is derived for micro-VMs only", got, microVMPool)
	}
	if len(t.GetContainers()) != 1 {
		return nil, fmt.Errorf("memory-tier template has %d containers, want 1", len(t.GetContainers()))
	}
	if len(t.GetVolumes()) != 0 {
		return nil, errors.New("memory-tier template already declares volumes")
	}
	old := t.GetMetadata().GetName()
	name := suffixName(old, volumesSuffix)
	harness, atespace, err := parseMemoryFlag(memoryFlag, t.GetMetadata().GetAtespace(), old)
	if err != nil {
		return nil, err
	}
	loc := t.GetSnapshotConfig().GetStorageLocation()
	if !strings.HasSuffix(loc, "/"+old+"/") {
		return nil, fmt.Errorf("memory-tier storage location does not end in /%s/", old)
	}
	t.Metadata.Name = name
	t.SnapshotConfig.PreferredFidelity = atepb.SnapshotFidelity_SNAPSHOT_FIDELITY_VOLUMES
	t.SnapshotConfig.StorageLocation = strings.TrimSuffix(loc, old+"/") + name + "/"
	t.Volumes = []*atepb.Volume{{Name: volumesVolume, DurableDir: &atepb.DurableDirVolumeSource{}}}
	c := t.Containers[0]
	c.VolumeMounts = append(c.VolumeMounts, &atepb.VolumeMount{Name: volumesVolume, MountPath: volumesMountPath})
	data, err := protojson.Marshal(&t)
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	if err := json.Indent(&out, data, "", "  "); err != nil {
		return nil, err
	}
	out.WriteByte('\n')
	return &derived{
		name:     name,
		template: out.Bytes(),
		flag:     fmt.Sprintf("-substrate-harness=%s=%s/%s,memory", harness, atespace, name),
	}, nil
}

// suffixName is name with suffix, trimmed to a 63-character DNS label.
func suffixName(name, suffix string) string {
	const max = 63
	if len(name)+len(suffix) > max {
		name = strings.TrimRight(name[:max-len(suffix)], "-")
	}
	return name + suffix
}

// parseMemoryFlag checks the memory tier's -substrate-harness line serves this template with
// ,memory, and returns its harness name and atespace.
func parseMemoryFlag(line, atespace, template string) (harness, space string, err error) {
	v, ok := strings.CutPrefix(strings.TrimSpace(line), "-substrate-harness=")
	if !ok {
		return "", "", fmt.Errorf("memory-tier daemon flag %q: want -substrate-harness=<name>=<atespace>/<template>,memory", line)
	}
	harness, target, ok := strings.Cut(v, "=")
	if !ok || harness == "" {
		return "", "", fmt.Errorf("memory-tier daemon flag %q: want name=atespace/template,memory", line)
	}
	if want := atespace + "/" + template + ",memory"; target != want {
		return "", "", fmt.Errorf("memory-tier daemon flag serves %q, want %q", target, want)
	}
	return harness, atespace, nil
}

// volumesManifest derives the volumes daemon from up.sh's rendered agentsessionsd-memory.yaml: the
// same image, model and settings, named agentsessionsd-volumes, serving the memory-tier harness
// with flag. It refuses a manifest without exactly one -substrate-harness argument naming
// memoryFlag.
func volumesManifest(memoryManifest, memoryFlag, flag string) (string, error) {
	arg := "- " + strings.TrimSpace(memoryFlag)
	if n := strings.Count(memoryManifest, arg+"\n"); n != 1 {
		return "", fmt.Errorf("memory-tier manifest holds %q %d times, want once", arg, n)
	}
	if strings.Contains(memoryManifest, volumesDeployment) {
		return "", fmt.Errorf("memory-tier manifest already names %s", volumesDeployment)
	}
	if !strings.Contains(memoryManifest, "name: "+memoryDeployment+"\n") {
		return "", fmt.Errorf("memory-tier manifest does not name %s; the derived daemon would keep another daemon's name", memoryDeployment)
	}
	m := strings.Replace(memoryManifest, arg+"\n", "- "+flag+"\n", 1)
	m = strings.ReplaceAll(m, memoryDeployment, volumesDeployment)
	if strings.Count(m, "-substrate-harness=") != 1 {
		return "", errors.New("volumes manifest serves more than one harness")
	}
	return m, nil
}
