package agentsessionsd

import (
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
)

// substrateModule is the vendor module only integrations/substrate may require. runtimeSubstrate is
// the root module's one substrate-specific package, which reaches substrate through its own
// ControlClient interface; the daemon must not link even that, so substrate flags and client setup
// stay in integrations/substrate (see HarnessBackends).
const (
	substrateModule  = "github.com/agent-substrate/"
	runtimeSubstrate = "github.com/agent-primitives/sessions/runtime/substrate"
)

// TestRootModuleHasNoSubstrateDependency checks the root module's go.mod requires no
// agent-substrate module, and that the daemon, cmd/agentsessionsd and this package, links no
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
		if strings.HasPrefix(r.Path, substrateModule) {
			t.Errorf("root go.mod requires %s; only integrations/substrate may", r.Path)
		}
	}

	deps := goCmd(t, "list", "-deps",
		"github.com/agent-primitives/sessions/cmd/agentsessionsd",
		"github.com/agent-primitives/sessions/internal/agentsessionsd")
	for _, pkg := range strings.Fields(deps) {
		if strings.HasPrefix(pkg, substrateModule) || pkg == runtimeSubstrate || strings.HasPrefix(pkg, runtimeSubstrate+"/") {
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
