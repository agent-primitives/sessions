package substratekind_test

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fixture(t *testing.T, scripts map[string]string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	for name, body := range scripts {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/usr/bin/env bash\nset -euo pipefail\n"+body), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("TEST_DIR", dir)
	t.Setenv("CLUSTER", "recipe-test")
	t.Setenv("SUBSTRATE_KIND_WORK", dir)
	t.Setenv("SUBSTRATE_DIR", filepath.Join(dir, "custom-substrate"))
	t.Setenv("SUBSTRATE_KIND_KUBECONFIG", filepath.Join(dir, "kubeconfig"))
	state := filepath.Join(dir, "recipe-test")
	if err := os.Mkdir(state, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir, state
}

func prerequisites(t *testing.T) {
	t.Helper()
	for _, command := range []string{"jq", "timeout"} {
		if _, err := exec.LookPath(command); err != nil {
			if command == "timeout" {
				if _, err := exec.LookPath("gtimeout"); err == nil {
					continue
				}
			}
			if os.Getenv("CI") != "" {
				t.Fatalf("%s is required in CI", command)
			}
			t.Skipf("%s is not installed", command)
		}
	}
}

const fakeATE = `echo "$*" >> "$TEST_DIR/calls"
case "$*" in
  *"get atespace"*)
    case "$TEST_MODE" in
      transport) echo 'rpc error: code = Unavailable desc = unavailable' >&2; exit 1 ;;
      ambiguous) echo 'rpc error: code = Unavailable desc = nested rpc error: code = NotFound desc = absent' >&2; exit 1 ;;
      auth) echo 'rpc error: code = PermissionDenied desc = denied' >&2; exit 1 ;;
      null) echo null; exit ;;
      stall) sleep 5 ;;
    esac
    echo '{"metadata":{"name":"sessions-example"}}'; exit ;;
  *"create actor-template"*) cp "${@: -1}" "$TEST_DIR/current" ;;
  *"get actor-template"*)
    if [[ ! -f "$TEST_DIR/current" ]]; then
      echo 'rpc error: code = NotFound desc = absent' >&2; exit 1
    fi ;;
  *) exit 1 ;;
esac
jq --arg mode "$TEST_MODE" '
  .status.goldenSnapshotStatus.goldenTag.name = "ready" |
  if $mode == "containers" then .containers[0].env += [{name:"UNEXPECTED",value:"changed"}]
  elif $mode == "resources" then .resources.limits[0].quantity = "2"
  elif $mode == "sandboxConfig" then .sandboxConfig.configName = "changed"
  elif $mode == "snapshotConfig" then .snapshotConfig.storageLocation = "changed"
  elif $mode == "workerSelector" then .workerSelector.matchLabels.unexpected = "changed"
  elif $mode == "identity" then .metadata.name = "wrong"
  elif $mode == "golden-error" then .status.goldenSnapshotStatus.errorMessage = "snapshot refused"
  elif $mode == "golden-pending" then .status = {}
  else . end
' "$TEST_DIR/current"`

func templateFixture(t *testing.T) (string, string) {
	t.Helper()
	prerequisites(t)
	dir, state := fixture(t, nil)
	if err := os.WriteFile(filepath.Join(state, "kubectl-ate"),
		[]byte("#!/usr/bin/env bash\nset -euo pipefail\n"+fakeATE), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TEST_MODE", "")
	t.Setenv("HARNESS_IMAGE", "example/harness@sha256:"+strings.Repeat("a", 64))
	t.Setenv("MODEL_NAME", `model-with-"quotes"`)
	t.Setenv("ACTOR_TEMPLATE", "chat-harness")
	t.Setenv("SANDBOX_CLASS", "microvm")
	t.Setenv("SANDBOX_CONFIG", "microvm")
	t.Setenv("PROVISION_TIMEOUT_SECONDS", "600")
	return dir, state
}

func TestProgressKeepsCommandOutputSeparate(t *testing.T) {
	fixture(t, nil)
	cmd := exec.CommandContext(t.Context(), "bash", "-c", `
source ./common.sh
progress "Preparing resources (100% local)."
printf '{"ready":true}\n'
`, "up.sh")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("progress: %v\n%s", err, stderr.String())
	}
	if stdout.String() != "{\"ready\":true}\n" {
		t.Fatalf("progress contaminated command output: %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "==> [substrate-kind/up.sh] Preparing resources (100% local).") {
		t.Fatalf("missing labeled progress on stderr: %q", stderr.String())
	}
}

func TestProvisionCreatesAndReusesDeclarativeTemplate(t *testing.T) {
	dir, _ := templateFixture(t)
	for range 2 {
		if out, err := exec.CommandContext(t.Context(), "bash", "provision.sh").CombinedOutput(); err != nil {
			t.Fatalf("provision: %v\n%s", err, out)
		}
	}
	calls, err := os.ReadFile(filepath.Join(dir, "calls"))
	if err != nil || strings.Count(string(calls), "create actor-template") != 1 {
		t.Fatalf("matching template recreated: %s %v", calls, err)
	}
	cmd := exec.CommandContext(t.Context(), "jq", "-e", `any(.containers[0].env[]; .name == "HARNESS_MODEL" and .value == "model-with-\"quotes\"")`, filepath.Join(dir, "current"))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("JSON parameter escaping failed: %v\n%s", err, out)
	}
}

func TestProvisionFailsClosed(t *testing.T) {
	for _, mode := range []string{
		"containers", "resources", "sandboxConfig", "snapshotConfig", "workerSelector",
		"identity", "golden-error", "golden-pending", "transport", "ambiguous", "auth", "null", "stall",
	} {
		t.Run(mode, func(t *testing.T) {
			dir, _ := templateFixture(t)
			if out, err := exec.CommandContext(t.Context(), "bash", "provision.sh").CombinedOutput(); err != nil {
				t.Fatalf("initial provision: %v\n%s", err, out)
			}
			if err := os.WriteFile(filepath.Join(dir, "calls"), nil, 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("TEST_MODE", mode)
			if mode == "golden-pending" || mode == "stall" {
				t.Setenv("PROVISION_TIMEOUT_SECONDS", "1")
			}
			ctx, cancel := context.WithTimeout(t.Context(), 4*time.Second)
			defer cancel()
			if out, err := exec.CommandContext(ctx, "bash", "provision.sh").CombinedOutput(); err == nil || ctx.Err() != nil {
				t.Fatalf("did not refuse within its own deadline: %v\n%s", err, out)
			}
			calls, err := os.ReadFile(filepath.Join(dir, "calls"))
			if err != nil || strings.Contains(string(calls), "create ") {
				t.Fatalf("failure caused a create: %s %v", calls, err)
			}
		})
	}
}

func TestProvisionPreflightDoesNotCreate(t *testing.T) {
	dir, _ := templateFixture(t)
	if out, err := exec.CommandContext(t.Context(), "bash", "provision.sh", "--check-only").CombinedOutput(); err != nil {
		t.Fatalf("preflight: %v\n%s", err, out)
	}
	calls, err := os.ReadFile(filepath.Join(dir, "calls"))
	if err != nil || strings.Contains(string(calls), "create ") {
		t.Fatalf("preflight created a resource: %s %v", calls, err)
	}
}

func TestTeardownRequiresExactOwnership(t *testing.T) {
	for _, tc := range []struct {
		name, owner, node, listed, failure string
		fail                               bool
	}{
		{name: "owned", owner: "node installed\n", node: "node", listed: "recipe-test"},
		{name: "partial", owner: "node creating\n", node: "node", listed: "recipe-test"},
		{name: "foreign", node: "node", listed: "recipe-test", fail: true},
		{name: "replaced", owner: "old installed\n", node: "node", listed: "recipe-test", fail: true},
		{name: "unknown state", owner: "node unknown\n", node: "node", listed: "recipe-test", fail: true},
		{name: "unlisted node", owner: "node installed\n", node: "node", fail: true},
		{name: "docker failure", owner: "node installed\n", node: "node", listed: "recipe-test", failure: "docker", fail: true},
		{name: "kind failure", owner: "node installed\n", node: "node", listed: "recipe-test", failure: "kind", fail: true},
		{name: "delete failure", owner: "node installed\n", node: "node", listed: "recipe-test", failure: "delete", fail: true},
		{name: "already absent", owner: "old installed\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, state := fixture(t, map[string]string{
				"kind": `echo "$*" >> "$TEST_DIR/kind-calls"
case "$*" in
  "get clusters") [[ "$TEST_FAILURE" != kind ]]; cat "$TEST_DIR/cluster" ;;
  "delete cluster --name recipe-test") [[ "$TEST_FAILURE" != delete ]]; : > "$TEST_DIR/cluster"; : > "$TEST_DIR/node" ;;
  *) exit 1 ;;
esac`,
				"docker": `[[ "$1" == ps && "$TEST_FAILURE" != docker ]]; cat "$TEST_DIR/node"`,
			})
			t.Setenv("TEST_FAILURE", tc.failure)
			for name, value := range map[string]string{"cluster": tc.listed + "\n", "node": tc.node + "\n"} {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(value), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if tc.owner != "" {
				if err := os.WriteFile(filepath.Join(state, "owner"), []byte(tc.owner), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if out, err := exec.CommandContext(t.Context(), "bash", "down.sh").CombinedOutput(); (err != nil) != tc.fail {
				t.Fatalf("teardown: %v\n%s", err, out)
			}
			if tc.fail {
				if tc.owner != "" {
					owner, err := os.ReadFile(filepath.Join(state, "owner"))
					if err != nil || string(owner) != tc.owner {
						t.Fatalf("ownership lost on failure: %s %v", owner, err)
					}
				}
				if tc.failure != "delete" {
					calls, _ := os.ReadFile(filepath.Join(dir, "kind-calls"))
					if strings.Contains(string(calls), "delete cluster") {
						t.Fatal("uncertain ownership triggered deletion")
					}
				}
			}
		})
	}
}
