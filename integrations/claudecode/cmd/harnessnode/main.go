// Command harnessnode serves the unmediated Claude Code harness (package claudecode) over
// harnesswire. It is the root module's harnessnode with this one harness: HARNESS_KIND must be
// unset or "claudecode", and the server settings (HARNESS_ADDR, HARNESS_MAX_RECV_MSG_SIZE and the
// rest) are read as there. The harness is configured from the environment:
//
//   - HARNESS_CLAUDE_BIN: the claude executable (default "claude", looked up in PATH). A relative
//     path is resolved against this process's working directory.
//   - HARNESS_CLAUDE_WORKDIR: the directory Claude Code runs in. Required.
//   - HARNESS_CLAUDE_PERMISSION_MODE: passed as --permission-mode. Required.
//   - HARNESS_CLAUDE_ALLOW_BYPASS_PERMISSIONS: a boolean (strconv.ParseBool), default false. It
//     must be true when the permission mode is bypassPermissions, and must not be otherwise.
//   - HARNESS_CLAUDE_ENV_ALLOWLIST: optional comma-separated names of further variables claude
//     receives from this process's environment. claude gets only PATH, HOME, USER, SHELL, TMPDIR,
//     LANG, LC_ALL, LC_CTYPE, ANTHROPIC_API_KEY, ANTHROPIC_AUTH_TOKEN, ANTHROPIC_BASE_URL and
//     CLAUDE_CONFIG_DIR by default; add proxy or CA variables here when the deployment needs them.
//   - HARNESS_MODEL: optional; pins --model and the harness's declared model.
//   - HARNESS_CLAUDE_MAX_TURNS, HARNESS_CLAUDE_MAX_BUDGET_USD: optional per-turn limits.
//
// HARNESS_MAX_RECV_MSG_SIZE also sizes the harness's tool receipts against History. No model
// credential is configured here: claude gets ANTHROPIC_API_KEY or ANTHROPIC_AUTH_TOKEN from this
// process's environment, which must hold only a placeholder key that the runtime's egress replaces
// (spec EXE-40).
package main

import (
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"

	"github.com/agent-primitives/sessions/api"
	"github.com/agent-primitives/sessions/integrations/claudecode"
	"github.com/agent-primitives/sessions/internal/harnessnode"
)

func main() { harnessnode.Main(harnessnode.WithHarness("claudecode", newHarness)) }

// newHarness builds the harness from the environment variables in the package doc.
func newHarness(s harnessnode.Settings) (api.Harness, error) {
	cfg := claudecode.Config{
		Binary:         os.Getenv("HARNESS_CLAUDE_BIN"),
		WorkDir:        os.Getenv("HARNESS_CLAUDE_WORKDIR"),
		Model:          os.Getenv("HARNESS_MODEL"),
		PermissionMode: os.Getenv("HARNESS_CLAUDE_PERMISSION_MODE"),
		StartLimit:     s.MaxRecvMsgSize, // 0 keeps gRPC's default, as the server does
	}
	if v := os.Getenv("HARNESS_CLAUDE_ALLOW_BYPASS_PERMISSIONS"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return nil, fmt.Errorf("HARNESS_CLAUDE_ALLOW_BYPASS_PERMISSIONS=%q: want a boolean", v)
		}
		cfg.AllowBypassPermissions = b
	}
	for name := range strings.SplitSeq(os.Getenv("HARNESS_CLAUDE_ENV_ALLOWLIST"), ",") {
		if name = strings.TrimSpace(name); name != "" {
			cfg.EnvAllowlist = append(cfg.EnvAllowlist, name)
		}
	}
	if v := os.Getenv("HARNESS_CLAUDE_MAX_TURNS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return nil, fmt.Errorf("HARNESS_CLAUDE_MAX_TURNS=%q: want a non-negative integer", v)
		}
		cfg.MaxTurns = n
	}
	if v := os.Getenv("HARNESS_CLAUDE_MAX_BUDGET_USD"); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil || !(f >= 0) || math.IsInf(f, 0) {
			return nil, fmt.Errorf("HARNESS_CLAUDE_MAX_BUDGET_USD=%q: want a non-negative number", v)
		}
		cfg.MaxBudgetUSD = f
	}
	h, err := claudecode.New(cfg)
	if err != nil {
		return nil, err
	}
	return h, nil
}
