package substratekind_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestSandboxSelectionAndForeignSetupRefusal(t *testing.T) {
	for _, tc := range []struct {
		name, class, kvm, want string
		probed                 bool
	}{
		{"automatic microvm", "", "available", "Sandbox: microvm", true},
		{"automatic fallback", "", "unavailable", "Falling back to gVisor", true},
		{"automatic fallback with legacy Docker status", "", "unavailable-legacy", "Falling back to gVisor", true},
		{"explicit microvm", "microvm", "available", "Sandbox: microvm", true},
		{"explicit microvm unavailable", "microvm", "unavailable", "Explicit microvm requires /dev/kvm access", true},
		{"explicit gvisor", "gvisor", "unavailable", "Sandbox: gvisor", false},
		{"Docker failure", "", "docker-failed", "refusing to fall back", true},
		{"image pull failure", "", "pull-failed", "image pull refused", false},
		{"invalid class", "typo", "available", "SANDBOX_CLASS must be", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, _ := fixture(t, map[string]string{
				"docker": `echo "$*" >> "$TEST_DIR/docker-calls"
case "$1" in
  info) echo x86_64 ;;
  pull) if [[ "$TEST_KVM" == pull-failed ]]; then echo 'image pull refused' >&2; exit 1; fi ;;
  run)
    [[ "$*" == "run --rm --device /dev/kvm "* && "${@: -1}" == true ]] || exit 126
    case "$TEST_KVM" in
      available) exit 0 ;;
      unavailable | unavailable-legacy)
        echo 'docker: Error response from daemon: error gathering device information while adding custom device "/dev/kvm": no such file or directory' >&2
        if [[ "$TEST_KVM" == unavailable-legacy ]]; then exit 125; fi
        exit 127 ;;
      *) echo 'docker: Error response from daemon: container startup failed' >&2 ;;
    esac
    exit 125 ;;
  ps) echo node ;;
  *) exit 1 ;;
esac`,
				"git":  `case "$*" in *"rev-parse HEAD") echo "$TEST_PIN" ;; *"status --porcelain") exit 0 ;; *) exit 1 ;; esac`,
				"kind": `echo "$*" >> "$TEST_DIR/kind-calls"; [[ "$*" == "get clusters" ]]; echo recipe-test`,
				"go":   `echo unexpected-go-invocation >&2; exit 1`,
				"ko":   `echo unexpected-ko-invocation >&2; exit 1`,
			})
			prerequisites(t)
			workflow, err := os.ReadFile("../../.github/workflows/substrate-e2e.yml")
			if err != nil {
				t.Fatal(err)
			}

			for _, line := range strings.Split(string(workflow), "\n") {
				if pin, ok := strings.CutPrefix(line, "  SUBSTRATE_REF: "); ok {
					t.Setenv("TEST_PIN", pin)
				}
			}
			t.Setenv("TEST_KVM", tc.kvm)
			t.Setenv("SANDBOX_CLASS", tc.class)
			t.Setenv("MODEL_SOURCE", "stub")
			out, err := exec.CommandContext(t.Context(), "bash", "up.sh").CombinedOutput()
			if err == nil || !strings.Contains(string(out), tc.want) {
				t.Fatalf("selection/refusal = %v\n%s; want %q", err, out, tc.want)
			}
			if strings.Contains(string(out), "Sandbox:") && !strings.Contains(string(out), "Refusing foreign") {
				t.Fatalf("setup did not protect the foreign cluster:\n%s", out)
			}
			calls, _ := os.ReadFile(filepath.Join(dir, "docker-calls"))
			if strings.Contains(string(calls), "run ") != tc.probed {
				t.Fatalf("unexpected Docker probe calls: %s", calls)
			}
			if tc.class == "gvisor" && strings.Contains(string(calls), "pull ") {
				t.Fatalf("explicit gvisor pulled the probe image: %s", calls)
			}
			kindCalls, _ := os.ReadFile(filepath.Join(dir, "kind-calls"))
			if strings.Contains(string(kindCalls), "delete") || strings.Contains(string(kindCalls), "export") {
				t.Fatalf("foreign cluster modified: %s", kindCalls)
			}
		})
	}
}

func TestSetupChecksCreatedAndReusedNodeKVMMount(t *testing.T) {
	const writable = `[{"Source":"/dev/kvm","Destination":"/dev/kvm","RW":true}]`
	for _, tc := range []struct {
		name, mounts, want string
		reuse              bool
	}{
		{"creation helper probe failure", "[]", "lacks the required writable /dev/kvm mount", false},
		{"created writable mount", writable, "reached-install", false},
		{"reused missing mount", "[]", "lacks the required writable /dev/kvm mount", true},
		{"reused read-only mount", `[{"Source":"/dev/kvm","Destination":"/dev/kvm","RW":false}]`, "lacks the required writable /dev/kvm mount", true},
		{"inspection failure", "error", "Cannot inspect the kind node's mounts", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prerequisites(t)
			dir, state := fixture(t, map[string]string{
				"docker": `case "$1" in
  info) echo x86_64 ;;
  pull) exit 0 ;;
  run)
    if [[ "$*" == *" busybox true" ]]; then [[ "$TEST_MOUNTS" != "[]" ]]; else [[ "$*" == *" busybox:1.37@sha256:"* ]]; fi ;;
  ps) if [[ -f "$TEST_DIR/node" ]]; then cat "$TEST_DIR/node"; fi ;;
  inspect) [[ "$TEST_MOUNTS" != error ]]; echo "$TEST_MOUNTS" ;;
  *) exit 1 ;;
esac`,
				"git": `case "$*" in *"rev-parse HEAD") echo "$TEST_PIN" ;; *"status --porcelain") exit 0 ;; *) exit 1 ;; esac`,
				"kind": `case "$*" in
  "get clusters") if [[ -f "$TEST_DIR/node" ]]; then echo recipe-test; fi ;;
  "export kubeconfig"*) exit 0 ;;
  *) exit 1 ;;
esac`,
				"go":      `echo reached-install >&2; exit 1`,
				"ko":      `exit 1`,
				"kubectl": `exit 1`,
			})
			hack := filepath.Join(dir, "custom-substrate", "hack")
			if err := os.MkdirAll(hack, 0o700); err != nil {
				t.Fatal(err)
			}
			for name, body := range map[string]string{
				"create-kind-cluster.sh": `#!/usr/bin/env bash
set -euo pipefail
# Reproduce the pinned upstream helper's independent, failure-tolerant probe.
if docker run --rm --device /dev/kvm busybox true; then :; fi
echo node > "$TEST_DIR/node"
`,
				"install-microvm-deps.sh": "#!/usr/bin/env bash\necho reached-install >&2\nexit 1\n",
			} {
				if err := os.WriteFile(filepath.Join(hack, name), []byte(body), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			if tc.reuse {
				for path, contents := range map[string]string{
					filepath.Join(dir, "node"):    "node\n",
					filepath.Join(state, "owner"): "node installed\n",
				} {
					if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
						t.Fatal(err)
					}
				}
			}
			workflow, err := os.ReadFile("../../.github/workflows/substrate-e2e.yml")
			if err != nil {
				t.Fatal(err)
			}
			for _, line := range strings.Split(string(workflow), "\n") {
				if pin, ok := strings.CutPrefix(line, "  SUBSTRATE_REF: "); ok {
					t.Setenv("TEST_PIN", pin)
				}
			}
			t.Setenv("TEST_MOUNTS", tc.mounts)
			t.Setenv("SANDBOX_CLASS", "microvm")
			t.Setenv("MODEL_SOURCE", "stub")
			out, err := exec.CommandContext(t.Context(), "bash", "up.sh").CombinedOutput()
			if err == nil || !strings.Contains(string(out), tc.want) {
				t.Fatalf("node mount check: %v\n%s; want %q", err, out, tc.want)
			}
			if tc.want != "reached-install" && strings.Contains(string(out), "reached-install") {
				t.Fatalf("incompatible node reached installation:\n%s", out)
			}
			owner, err := os.ReadFile(filepath.Join(state, "owner"))
			if err != nil || !strings.HasPrefix(string(owner), "node ") {
				t.Fatalf("node ownership was not retained for explicit teardown: %s %v", owner, err)
			}
		})
	}
}
