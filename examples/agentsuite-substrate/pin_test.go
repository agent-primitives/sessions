package scripts_test

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// The substrate-e2e workflow's SUBSTRATE_VERSION names the Substrate release CI and the examples
// install from its published images; both Go modules that import Substrate must build against that
// same release, or the client and the cluster's ate-api can disagree on field numbers silently.
func TestSubstrateModulesRequireTheWorkflowRelease(t *testing.T) {
	root := filepath.Join("..", "..")
	workflow, err := os.ReadFile(filepath.Join(root, ".github", "workflows", "substrate-e2e.yml"))
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^  SUBSTRATE_VERSION: (v\S+)$`).FindSubmatch(workflow)
	if m == nil {
		t.Fatal("substrate-e2e.yml sets no SUBSTRATE_VERSION")
	}
	version := string(m[1])
	require := regexp.MustCompile(`(?m)^\s*(?:require\s+)?go\.ate\.dev/substrate (\S+)$`)
	for _, mod := range []string{
		filepath.Join(root, "integrations", "substrate", "go.mod"),
		"go.mod",
	} {
		b, err := os.ReadFile(mod)
		if err != nil {
			t.Fatal(err)
		}
		got := require.FindSubmatch(b)
		if got == nil {
			t.Errorf("%s does not require go.ate.dev/substrate", mod)
		} else if string(got[1]) != version {
			t.Errorf("%s requires go.ate.dev/substrate %s; substrate-e2e.yml installs %s", mod, got[1], version)
		}
	}
}
