package agentsessionsd

import (
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
)

// substrateModules are the vendor module prefixes only integrations/substrate may require: the
// module's path since v0.6.0 and its earlier one. runtimeSubstrate is
// the root module's one substrate-specific package, which reaches substrate through its own
// ControlClient interface; the daemon must not link even that, so substrate flags and client setup
// stay in integrations/substrate (see HarnessBackends).
const runtimeSubstrate = "github.com/agent-primitives/sessions/runtime/substrate"

var substrateModules = []string{"go.ate.dev/", "github.com/agent-substrate/"}

func isSubstrateModule(path string) bool {
	for _, prefix := range substrateModules {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	return false
}

// TestRootModuleHasNoSubstrateDependency checks the root module's go.mod requires no
// substrate module, and that the daemon, cmd/agentsessionsd and this package, links no
// substrate package, runtime/substrate included.
func TestRootModuleHasNoSubstrateDependency(t *testing.T) {
	if testing.Short() {
		t.Skip("runs the go command")
	}
	goMod := strings.TrimSpace(goCmd(t, "env", "GOMOD"))
	if goMod == "" || goMod == "/dev/null" {
		t.Fatalf("go env GOMOD = %q, want the root module's go.mod", goMod)
	}
	var mod struct {
		Module  struct{ Path string }
		Require []struct{ Path string }
	}
	if err := json.Unmarshal([]byte(goCmd(t, "mod", "edit", "-json", goMod)), &mod); err != nil {
		t.Fatal(err)
	}
	if mod.Module.Path != "github.com/agent-primitives/sessions" {
		t.Fatalf("go.mod %s is module %q, want the root module", goMod, mod.Module.Path)
	}
	for _, r := range mod.Require {
		if isSubstrateModule(r.Path) {
			t.Errorf("root go.mod requires %s; only integrations/substrate may", r.Path)
		}
	}

	deps := goCmd(t, "list", "-deps",
		"github.com/agent-primitives/sessions/cmd/agentsessionsd",
		"github.com/agent-primitives/sessions/internal/agentsessionsd")
	for _, pkg := range strings.Fields(deps) {
		if isSubstrateModule(pkg) || pkg == runtimeSubstrate || strings.HasPrefix(pkg, runtimeSubstrate+"/") {
			t.Errorf("agentsessionsd links %s; a substrate harness is added by integrations/substrate/cmd/agentsessionsd through WithHarnessBackends", pkg)
		}
	}
}

func goCmd(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("go", args...).Output()
	if err != nil {
		var stderr string
		if ee, ok := err.(*exec.ExitError); ok {
			stderr = string(ee.Stderr)
		}
		t.Fatalf("go %s: %v\n%s", strings.Join(args, " "), err, stderr)
	}
	return string(out)
}
