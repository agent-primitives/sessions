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
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	realGo, err := exec.LookPath("go")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("TEST_REAL_GO", realGo)
	const checkModel = `if [[ "$#" == 4 && "$1" == -C && "$3" == run && "$4" == ./hack/check-model ]]; then
  exec "$TEST_REAL_GO" "$@"
fi
`
	for name, body := range scripts {
		if name == "go" {
			body = checkModel + body
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/usr/bin/env bash\nset -euo pipefail\n"+body), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"go", "kubectl", "ko"} {
		if _, ok := scripts[name]; ok {
			continue
		}
		body := "echo unexpected-" + name + "-invocation >&2\nexit 1\n"
		if name == "go" {
			body = checkModel + body
		}
		if err := os.WriteFile(filepath.Join(dir, name),
			[]byte("#!/usr/bin/env bash\nset -euo pipefail\n"+body), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("TEST_DIR", dir)
	t.Setenv("CLUSTER", "recipe-test")
	t.Setenv("SUBSTRATE_KIND_WORK", dir)
	t.Setenv("SUBSTRATE_DIR", filepath.Join(dir, "custom-substrate"))
	t.Setenv("SUBSTRATE_KIND_KUBECONFIG", filepath.Join(dir, "kubeconfig"))
	t.Setenv("MODEL_API_KEY", "")
	t.Setenv("MODEL_PATH", "")
	t.Setenv("MODEL_AUTH_HEADER", "")
	t.Setenv("SANDBOX_CONFIG", "")
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
  *"get access-policy -a sessions-example")
    if [[ "$TEST_MODE" == policy-transport ]]; then
      echo 'rpc error: code = Unavailable desc = unavailable' >&2; exit 1
    fi
    # A missing policy is a successful get with no output, as kubectl-ate reports it.
    if [[ -f "$TEST_DIR/policy" ]]; then cat "$TEST_DIR/policy"; else echo 'atespace "sessions-example" has no access policy' >&2; fi
    exit ;;
  *"create access-policy -a sessions-example -f -")
    manifest="$(cat)"
    [[ "$manifest" == "$(printf 'bindings:\n- role: editor\n  members:\n  - %s' "$TEST_PRINCIPAL")" ]]
    jq -n --arg member "$TEST_PRINCIPAL" '{bindings:[{role:"editor",members:[$member]}]}' > "$TEST_DIR/policy"
    if [[ "$TEST_MODE" == policy-race ]]; then
      echo 'rpc error: code = AlreadyExists desc = exists' >&2; exit 1
    fi
    cat "$TEST_DIR/policy"; exit ;;
  *"create actor-template"*)
    jq -e --arg class "$SANDBOX_CLASS" '
      (has("sandboxConfig") | not) and
      .resources == {limits:{cpuMillicores:"1000",memoryBytes:"805306368"}} and
      .workerSelector.matchLabels.workload == ("sessions-example-" + $class)
    ' "${@: -1}" >/dev/null
    cp "${@: -1}" "$TEST_DIR/current" ;;
  *"get actor-template"*)
    if [[ ! -f "$TEST_DIR/current" ]]; then
      echo 'rpc error: code = NotFound desc = absent' >&2; exit 1
    fi ;;
  *) exit 1 ;;
esac
jq --arg mode "$TEST_MODE" '
  .status.goldenSnapshotStatus.goldenTag.name = "ready" |
  if $mode == "containers" then .containers[0].env += [{name:"UNEXPECTED",value:"changed"}]
  elif $mode == "resources" then .resources.limits.cpuMillicores = "2000"
  elif $mode == "sandboxConfig" then .sandboxConfig = {sandboxClass:"microvm",configName:"microvm"}
  elif $mode == "snapshotConfig" then .snapshotConfig.storageLocation = "changed"
  elif $mode == "workerSelector" then .workerSelector.matchLabels.unexpected = "changed"
  elif $mode == "durable-volume" then .volumes = [{name:"extra",durableDir:{}}]
  elif $mode == "external-volume" then .volumes = [{name:"extra",externalVolumeTemplate:{capacity:"10Gi",storageClassName:"example"}}]
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
	t.Setenv("TEST_PRINCIPAL", daemonPrincipal)
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
		"containers", "resources", "sandboxConfig", "snapshotConfig", "workerSelector", "durable-volume", "external-volume",
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
			if mode == "durable-volume" || mode == "external-volume" {
				out, err := exec.CommandContext(t.Context(), "bash", "provision.sh", "--check-only").CombinedOutput()
				if err == nil || !strings.Contains(string(out), "Immutable template mismatch") {
					t.Fatalf("volume mismatch passed preflight: %v\n%s", err, out)
				}
			}
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
	out, err := exec.CommandContext(t.Context(), "bash", "provision.sh", "--check-only").CombinedOutput()
	if err != nil {
		t.Fatalf("preflight: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "Access policy is absent") {
		t.Fatalf("preflight did not report the absent access policy:\n%s", out)
	}
	calls, err := os.ReadFile(filepath.Join(dir, "calls"))
	if err != nil || strings.Contains(string(calls), "create ") {
		t.Fatalf("preflight created a resource: %s %v", calls, err)
	}
}

// daemonPrincipal is deploy/agentsessionsd.yaml's ServiceAccount as substrate's authorizer names it.
const daemonPrincipal = "user:system:serviceaccount:sessions-example:agentsessions"

func writePolicy(t *testing.T, dir, policy string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "policy"), []byte(policy), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestProvisionGrantsTheDaemonEditorOnce(t *testing.T) {
	for _, mode := range []string{"", "policy-race"} {
		t.Run("mode="+mode, func(t *testing.T) {
			dir, _ := templateFixture(t)
			t.Setenv("TEST_MODE", mode)
			for range 2 {
				if out, err := exec.CommandContext(t.Context(), "bash", "provision.sh").CombinedOutput(); err != nil {
					t.Fatalf("provision: %v\n%s", err, out)
				}
			}
			calls, err := os.ReadFile(filepath.Join(dir, "calls"))
			if err != nil || strings.Count(string(calls), "create access-policy") != 1 {
				t.Fatalf("access policy not created exactly once: %v\n%s", err, calls)
			}
			cmd := exec.CommandContext(t.Context(), "jq", "-e", "--arg", "m", daemonPrincipal,
				`.bindings == [{role: "editor", members: [$m]}]`, filepath.Join(dir, "policy"))
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("policy does not grant only the daemon editor: %v\n%s", err, out)
			}
		})
	}
}

func TestProvisionKeepsAnExistingGrant(t *testing.T) {
	for name, policy := range map[string]string{
		"editor": `{"bindings":[{"role":"viewer","members":["user:someone"]},{"role":"editor","members":["user:other","` + daemonPrincipal + `"]}]}`,
		"owner":  `{"bindings":[{"role":"owner","members":["` + daemonPrincipal + `"]}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			dir, _ := templateFixture(t)
			writePolicy(t, dir, policy)
			for _, args := range [][]string{{"provision.sh", "--check-only"}, {"provision.sh"}} {
				if out, err := exec.CommandContext(t.Context(), "bash", args...).CombinedOutput(); err != nil {
					t.Fatalf("%v: %v\n%s", args, err, out)
				}
			}
			calls, err := os.ReadFile(filepath.Join(dir, "calls"))
			if err != nil || strings.Contains(string(calls), "access-policy -a sessions-example -f") {
				t.Fatalf("an existing grant was rewritten: %v\n%s", err, calls)
			}
			if got, err := os.ReadFile(filepath.Join(dir, "policy")); err != nil || string(got) != policy {
				t.Fatalf("policy changed: %v\n%s", err, got)
			}
		})
	}
}

func TestProvisionRefusesAPolicyWithoutTheDaemon(t *testing.T) {
	for name, policy := range map[string]string{
		"other member": `{"bindings":[{"role":"editor","members":["user:other"]}]}`,
		"viewer only":  `{"bindings":[{"role":"viewer","members":["` + daemonPrincipal + `"]}]}`,
		"no bindings":  `{"metadata":{"name":"default"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			dir, _ := templateFixture(t)
			writePolicy(t, dir, policy)
			for _, args := range [][]string{{"provision.sh", "--check-only"}, {"provision.sh"}} {
				out, err := exec.CommandContext(t.Context(), "bash", args...).CombinedOutput()
				if err == nil || !strings.Contains(string(out),
					"kubectl ate update access-policy -a sessions-example -f policy.yaml") {
					t.Fatalf("%v accepted a policy without the daemon or omitted the remedy: %v\n%s", args, err, out)
				}
			}
			calls, err := os.ReadFile(filepath.Join(dir, "calls"))
			if err != nil || strings.Contains(string(calls), "create ") || strings.Contains(string(calls), "update ") {
				t.Fatalf("refusal wrote to substrate: %v\n%s", err, calls)
			}
		})
	}
}

func TestProvisionPolicyReadErrorFailsClosed(t *testing.T) {
	dir, _ := templateFixture(t)
	t.Setenv("TEST_MODE", "policy-transport")
	if out, err := exec.CommandContext(t.Context(), "bash", "provision.sh").CombinedOutput(); err == nil {
		t.Fatalf("provision passed a failed policy read:\n%s", out)
	}
	calls, err := os.ReadFile(filepath.Join(dir, "calls"))
	if err != nil || strings.Contains(string(calls), "create ") {
		t.Fatalf("policy read failure caused a create: %v\n%s", err, calls)
	}
}

func deploymentFixture(t *testing.T) string {
	t.Helper()
	prerequisites(t)
	dir, state := fixture(t, map[string]string{
		"sleep": `exit 0`,
		"timeout": `echo "$*" >> "$TEST_DIR/timeout-calls"
if [[ "$*" == *" -- ollama pull "* && "$TEST_PULL" == timeout ]]; then exit 124; fi
shift 2
exec "$@"`,
		"docker": `echo docker >> "$TEST_DIR/cluster-calls"; exit 1`,
		"kind":   `echo kind >> "$TEST_DIR/cluster-calls"; exit 1`,
		"git":    `echo git >> "$TEST_DIR/cluster-calls"; exit 1`,
		"ko":     `echo ko >> "$TEST_DIR/cluster-calls"; exit 1`,
		"kubectl": `echo "$*" >> "$TEST_DIR/kube-calls"
case "$*" in
  *"apply -f -"*)
    manifest="$(cat)"
    if [[ "$manifest" == *"kind: WorkerPool"* ]]; then
      pool="$(awk '/^  name: chat-harness/ {print $2}' <<< "$manifest")"
      class="$(awk '/^    - name:/ {print $3}' <<< "$manifest")"
      [[ -n "$pool" && -n "$class" ]]
      file="$TEST_DIR/pool-$pool"
      if [[ -f "$file" && "$(cat "$file")" != "$class" ]]; then
        echo 'sandboxClasses names are immutable' >&2; exit 1
      fi
      echo "$class" > "$file"
    fi ;;
  *"apply -f "* | *" -- ollama pull "*) exit 0 ;;
  *"get deployment/"*) echo ready ;;
  *"get pods"*) echo 'worker=10.0.0.1' ;;
  *"rollout status"*) exit 0 ;;
  *"create configmap"*) echo '{}' ;;
  *"get configmap"*) echo 1 ;;
  *"create secret"*) cat >/dev/null; touch "$TEST_DIR/secret"; echo '{}' ;;
  *"get secret"*) if [[ -f "$TEST_DIR/secret" ]]; then echo 2; fi ;;
  *"delete secret"*) rm -f "$TEST_DIR/secret" ;;
  *) exit 1 ;;
esac`,
	})
	ate := strings.ReplaceAll(fakeATE, "$TEST_DIR/current", "$TEST_DIR/current-$ACTOR_TEMPLATE")
	if err := os.WriteFile(filepath.Join(state, "kubectl-ate"),
		[]byte("#!/usr/bin/env bash\nset -euo pipefail\n"+ate), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TEST_MODE", "")
	t.Setenv("HARNESS_IMAGE", "example/harness@sha256:"+strings.Repeat("a", 64))
	t.Setenv("DAEMON_IMAGE", "example/daemon@sha256:"+strings.Repeat("b", 64))
	t.Setenv("MODEL_SOURCE", "external")
	t.Setenv("MODEL_NAME", "fixture-model")
	t.Setenv("MODEL_BASE_URL", "https://model.example")
	t.Setenv("MODEL_API_KEY", "")
	t.Setenv("TEST_PRINCIPAL", daemonPrincipal)
	t.Setenv("SANDBOX_CLASS", "gvisor")
	t.Setenv("ACTOR_TEMPLATE", "chat-gvisor")
	t.Setenv("WORKER_IMAGE", "example/worker@sha256:"+strings.Repeat("c", 64))
	t.Setenv("TEST_PULL", "success")
	return dir
}

func TestExternalCredentialSwitchRefusesLocalProvider(t *testing.T) {
	for _, source := range []string{"stub", "ollama"} {
		t.Run(source, func(t *testing.T) {
			dir := deploymentFixture(t)
			t.Setenv("MODEL_API_KEY", "fixture-only")
			if out, err := exec.CommandContext(t.Context(), "bash", "deploy.sh").CombinedOutput(); err != nil {
				t.Fatalf("initial external deployment: %v\n%s", err, out)
			}
			t.Setenv("MODEL_SOURCE", source)
			for _, script := range []string{"up.sh", "deploy.sh"} {
				for _, file := range []string{"kube-calls", "calls"} {
					if err := os.WriteFile(filepath.Join(dir, file), nil, 0o600); err != nil {
						t.Fatal(err)
					}
				}
				out, err := exec.CommandContext(t.Context(), "bash", script).CombinedOutput()
				if err == nil || !strings.Contains(string(out), "unset MODEL_API_KEY") ||
					strings.Contains(string(out), "fixture-only") {
					t.Fatalf("%s did not privately refuse leftover credentials: %v\n%s", script, err, out)
				}
				for _, file := range []string{"kube-calls", "calls", "cluster-calls"} {
					calls, err := os.ReadFile(filepath.Join(dir, file))
					if err != nil && !os.IsNotExist(err) {
						t.Fatal(err)
					}
					if len(calls) != 0 {
						t.Fatalf("%s contacted the cluster before refusal:\n%s", script, calls)
					}
				}
				if _, err := os.Stat(filepath.Join(dir, "secret")); err != nil {
					t.Fatalf("refusal changed the working deployment's Secret: %v", err)
				}
			}
			t.Setenv("MODEL_API_KEY", "")
			t.Setenv("MODELSTUB_IMAGE", "example/stub@sha256:"+strings.Repeat("d", 64))
			if out, err := exec.CommandContext(t.Context(), "bash", "deploy.sh").CombinedOutput(); err != nil {
				t.Fatalf("explicitly unkeyed local deployment: %v\n%s", err, out)
			}
			if _, err := os.Stat(filepath.Join(dir, "secret")); !os.IsNotExist(err) {
				t.Fatalf("local deployment retained the external credential Secret: %v", err)
			}
		})
	}
}

func TestModelRouteRefusalPreservesDeployment(t *testing.T) {
	for _, script := range []string{"up.sh", "deploy.sh"} {
		t.Run(script, func(t *testing.T) {
			dir := deploymentFixture(t)
			if out, err := exec.CommandContext(t.Context(), "bash", "deploy.sh").CombinedOutput(); err != nil {
				t.Fatalf("initial deployment: %v\n%s", err, out)
			}
			for _, tc := range []struct{ name, base, path, header string }{
				{"URL typo", "not-a-url", "/chat/completions", "Authorization"},
				{"credential-bearing URL", "https://fixture-marker@api.example", "/chat/completions", "Authorization"},
				{"invalid path", "https://api.example", "//fixture-marker.example/chat", "Authorization"},
				{"invalid header", "https://api.example", "/chat/completions", "fixture-marker\n"},
			} {
				t.Run(tc.name, func(t *testing.T) {
					t.Setenv("MODEL_BASE_URL", tc.base)
					t.Setenv("MODEL_PATH", tc.path)
					t.Setenv("MODEL_AUTH_HEADER", tc.header)
					before := make(map[string][]byte)
					for _, file := range []string{"kube-calls", "calls"} {
						var err error
						before[file], err = os.ReadFile(filepath.Join(dir, file))
						if err != nil {
							t.Fatal(err)
						}
					}
					out, err := exec.CommandContext(t.Context(), "bash", script).CombinedOutput()
					if err == nil || !strings.Contains(string(out), "Model provider preflight failed") ||
						strings.Contains(string(out), "fixture-marker") {
						t.Fatalf("route was not refused privately: %v\n%s", err, out)
					}
					for file, previous := range before {
						calls, err := os.ReadFile(filepath.Join(dir, file))
						if err != nil || !bytes.Equal(calls, previous) {
							t.Fatalf("refused route contacted the cluster (%s): %v\n%s", file, err, calls)
						}
					}
					if calls, err := os.ReadFile(filepath.Join(dir, "cluster-calls")); !os.IsNotExist(err) {
						t.Fatalf("refused route reached setup commands: %v\n%s", err, calls)
					}
				})
			}
		})
	}
}

// SANDBOX_CONFIG used to name the template's SandboxConfig. v0.6.0 templates name none and the
// rendered WorkerPool sets no configRef, so a leftover value must be refused before any cluster
// call rather than ignored in favor of the class default.
func TestSandboxConfigRefusedBeforeClusterChanges(t *testing.T) {
	for _, script := range []string{"up.sh", "deploy.sh", "provision.sh"} {
		t.Run(script, func(t *testing.T) {
			dir := deploymentFixture(t)
			if out, err := exec.CommandContext(t.Context(), "bash", "deploy.sh").CombinedOutput(); err != nil {
				t.Fatalf("initial deployment: %v\n%s", err, out)
			}
			before := make(map[string][]byte)
			for _, file := range []string{"kube-calls", "calls"} {
				var err error
				before[file], err = os.ReadFile(filepath.Join(dir, file))
				if err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("SANDBOX_CONFIG", "custom")
			out, err := exec.CommandContext(t.Context(), "bash", script).CombinedOutput()
			if err == nil || !strings.Contains(string(out), "SANDBOX_CONFIG is not supported") ||
				!strings.Contains(string(out), "configRef") {
				t.Fatalf("SANDBOX_CONFIG was not refused: %v\n%s", err, out)
			}
			for file, previous := range before {
				calls, err := os.ReadFile(filepath.Join(dir, file))
				if err != nil || !bytes.Equal(calls, previous) {
					t.Fatalf("refused SANDBOX_CONFIG contacted the cluster (%s): %v\n%s", file, err, calls)
				}
			}
			if calls, err := os.ReadFile(filepath.Join(dir, "cluster-calls")); !os.IsNotExist(err) {
				t.Fatalf("refused SANDBOX_CONFIG reached setup commands: %v\n%s", err, calls)
			}
		})
	}
}

func TestDeploySandboxChangePreservesExistingWorkerPool(t *testing.T) {
	dir := deploymentFixture(t)
	for _, class := range []string{"gvisor", "microvm"} {
		t.Setenv("SANDBOX_CLASS", class)
		t.Setenv("ACTOR_TEMPLATE", "chat-"+class)
		t.Setenv("WORKER_IMAGE", "example/worker-"+class+"@sha256:"+strings.Repeat("c", 64))
		if out, err := exec.CommandContext(t.Context(), "bash", "deploy.sh").CombinedOutput(); err != nil {
			t.Fatalf("%s deployment: %v\n%s", class, err, out)
		}
	}
	calls, err := os.ReadFile(filepath.Join(dir, "kube-calls"))
	if err != nil {
		t.Fatal(err)
	}
	for _, class := range []string{"gvisor", "microvm"} {
		pool := "chat-harness-" + class
		contents, err := os.ReadFile(filepath.Join(dir, "pool-"+pool))
		if err != nil || strings.TrimSpace(string(contents)) != class {
			t.Fatalf("pool %s not preserved: %q %v", pool, contents, err)
		}
		if !strings.Contains(string(calls), "rollout status deployment/"+pool+" ") {
			t.Fatalf("deployment did not wait for %s:\n%s", pool, calls)
		}
	}
}

func TestOllamaPullDeadline(t *testing.T) {
	for _, mode := range []string{"success", "timeout"} {
		t.Run(mode, func(t *testing.T) {
			dir := deploymentFixture(t)
			t.Setenv("MODEL_SOURCE", "ollama")
			t.Setenv("MODEL_BASE_URL", "")
			t.Setenv("TEST_PULL", mode)
			out, err := exec.CommandContext(t.Context(), "bash", "deploy.sh").CombinedOutput()
			if mode == "success" {
				if err != nil || !strings.Contains(string(out), "Sessions application deployment is ready") {
					t.Fatalf("Ollama deployment: %v\n%s", err, out)
				}
			} else {
				if err == nil || !strings.Contains(string(out), "Ollama model download failed or exceeded") {
					t.Fatalf("pull timeout did not refuse deployment: %v\n%s", err, out)
				}
				calls, err := os.ReadFile(filepath.Join(dir, "kube-calls"))
				if err != nil || strings.Contains(string(calls), "create configmap") {
					t.Fatalf("pull timeout changed daemon configuration: %v\n%s", err, calls)
				}
			}
			calls, err := os.ReadFile(filepath.Join(dir, "timeout-calls"))
			if err != nil {
				t.Fatal(err)
			}
			boundedPull := false
			for _, call := range strings.Split(string(calls), "\n") {
				if strings.Contains(call, " -- ollama pull ") &&
					strings.HasPrefix(call, "--kill-after=5s 600s ") {
					boundedPull = true
				}
			}
			if !boundedPull {
				t.Fatalf("missing bounded model pull:\n%s", calls)
			}
		})
	}
}

func TestStubSmokeRequiresTenRecords(t *testing.T) {
	for _, count := range []string{"10", "9"} {
		t.Run(count, func(t *testing.T) {
			prerequisites(t)
			fixture(t, map[string]string{
				"go": `[[ "$1" == -C && "$3" == build && "$4" == -o ]]
cp "$TEST_DIR/agentctl-fixture" "$5"`,
				"agentctl-fixture": `case "$1" in
  list) exit 0 ;;
  exec)
    echo 'session smoke-review'
    if [[ "$*" == *"--input first"* ]]; then echo 'context=1 last=first'; else echo 'context=3 last=second'; fi ;;
  verify)
    count=5
    if [[ -f "$TEST_DIR/verified" ]]; then count="$TEST_RECORDS"; fi
    touch "$TEST_DIR/verified"
    echo "session smoke-review chain verified ($count records)" ;;
  get | replay) echo first ;;
  *) exit 1 ;;
esac`,
				"kubectl": `case "$*" in
  *"port-forward"*) echo 'Forwarding from 127.0.0.1:18080'; exec sleep 30 ;;
  *"get pod"*) if [[ -f "$TEST_DIR/restarted" ]]; then echo new-pod; else echo old-pod; fi ;;
  *"delete pod"*) touch "$TEST_DIR/restarted" ;;
  *"rollout status"*) exit 0 ;;
  *"logs"*) echo 'session_uid=smoke-review' ;;
  *) exit 1 ;;
esac`,
			})
			t.Setenv("TEST_RECORDS", count)
			t.Setenv("MODEL_SOURCE", "stub")
			t.Setenv("SMOKE_TIMEOUT_SECONDS", "8")
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			out, err := exec.CommandContext(ctx, "bash", "smoke.sh").CombinedOutput()
			if count == "10" {
				if err != nil || !strings.Contains(string(out), "Restart smoke passed: smoke-review") {
					t.Fatalf("valid stub smoke: %v\n%s", err, out)
				}
			} else if err == nil || !strings.Contains(string(out), "expected ten journal records") ||
				strings.Contains(string(out), "Restart smoke passed:") {
				t.Fatalf("wrong record count did not refuse: %v\n%s", err, out)
			}
		})
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
