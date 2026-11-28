package claudecode

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"
)

// Bounds on a turn's environment from Start.Config.
const (
	maxTurnEnvEntries = 256
	maxTurnEnvName    = 256
	maxTurnEnvBytes   = 64 << 10 // names and values together
)

// reservedEnv names the variables a turn's environment can never set, and reservedEnvPrefixes the
// prefixes of such names: the process's identity, locale and paths, the model credentials (Claude
// Code's OAuth token, refresh token and token file descriptor among CLAUDE_CODE_OAUTH_), the
// transcript directory (CLAUDE_CONFIG_DIR) that --resume reads, and variables that make the
// dynamic loader, Node.js or a shell load or run code (BASH_FUNC_ is how bash exports functions).
// ANTHROPIC_BASE_URL is deliberately not reserved. Keep the package doc and README in sync.
var (
	reservedEnv = []string{
		"PATH", "HOME", "USER", "SHELL", "TMPDIR", "LANG", "LC_ALL", "LC_CTYPE",
		"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CONFIG_DIR",
		"NODE_OPTIONS", "BASH_ENV", "ENV", "PS4", "PROMPT_COMMAND", "ZDOTDIR",
	}
	reservedEnvPrefixes = []string{"LD_", "DYLD_", "BASH_FUNC_", "CLAUDE_CODE_OAUTH_"}
)

// reservedName reports whether a turn's environment must not set name.
func reservedName(name string) bool {
	return slices.Contains(reservedEnv, name) ||
		slices.ContainsFunc(reservedEnvPrefixes, func(p string) bool { return strings.HasPrefix(name, p) })
}

// envAllowlist is a validated Config.TurnEnvAllowlist.
type envAllowlist struct {
	names    map[string]bool
	prefixes []string
}

// newEnvAllowlist validates entries: each is a variable name, or a nonempty name prefix followed
// by '*'. An entry that is, or could match, a reserved name is refused. It returns nil for no
// entries.
func newEnvAllowlist(entries []string) (*envAllowlist, error) {
	if len(entries) == 0 {
		return nil, nil
	}
	a := &envAllowlist{names: make(map[string]bool)}
	for _, e := range entries {
		prefix, pattern := strings.CutSuffix(e, "*")
		if !envName(prefix) || strings.Contains(prefix, "*") {
			return nil, fmt.Errorf("claudecode: invalid TurnEnvAllowlist entry %q: want a variable name or a name prefix followed by '*'", e)
		}
		if !pattern {
			if reservedName(prefix) {
				return nil, fmt.Errorf("claudecode: TurnEnvAllowlist entry %q is a reserved variable", e)
			}
			a.names[prefix] = true
			continue
		}
		// The pattern covers a reserved name when the name has the prefix, and a reserved prefix
		// when either prefix extends the other (L* covers LD_, LD_X* lies inside it).
		if slices.ContainsFunc(reservedEnv, func(r string) bool { return strings.HasPrefix(r, prefix) }) ||
			slices.ContainsFunc(reservedEnvPrefixes, func(r string) bool {
				return strings.HasPrefix(r, prefix) || strings.HasPrefix(prefix, r)
			}) {
			return nil, fmt.Errorf("claudecode: TurnEnvAllowlist entry %q covers a reserved variable", e)
		}
		a.prefixes = append(a.prefixes, prefix)
	}
	return a, nil
}

// allows reports whether name matches an entry.
func (a *envAllowlist) allows(name string) bool {
	return a.names[name] || slices.ContainsFunc(a.prefixes, func(p string) bool { return strings.HasPrefix(name, p) })
}

// turnEnvFor returns the claude process's environment for a turn whose Start.Config is config:
// the base environment with the variables config's "env" member sets replaced, in name order. It
// returns the base environment itself when the feature is off or config sets nothing; otherwise a
// new slice, so h.env is never written. Every refusal wraps errTurnEnv, names at most the
// variable, never a value, and is the same for the same config: names are checked in order.
func (h *Harness) turnEnvFor(config []byte) ([]string, error) {
	if h.turnEnv == nil || len(config) == 0 {
		return h.env, nil
	}
	// A map, not a struct: encoding/json matches struct field names case-insensitively, and only
	// "env" exactly is this harness's member.
	var members map[string]json.RawMessage
	if err := json.Unmarshal(config, &members); err != nil || members == nil {
		return nil, fmt.Errorf("%w: Config is not a JSON object", errTurnEnv)
	}
	raw, ok := members["env"]
	if !ok {
		return h.env, nil
	}
	// encoding/json decodes invalid UTF-8 and an unpaired surrogate escape to U+FFFD without an
	// error, so the process would get bytes other than the journaled ones.
	if !validJSONText(raw) {
		return nil, fmt.Errorf("%w: env is not valid UTF-8 (invalid bytes or an unpaired surrogate escape)", errTurnEnv)
	}
	// Pointer values, so a JSON null is told apart from "" and refused.
	var vars map[string]*string
	if err := json.Unmarshal(raw, &vars); err != nil || vars == nil {
		return nil, fmt.Errorf("%w: env is not an object of string values", errTurnEnv)
	}
	if len(vars) == 0 {
		return h.env, nil
	}
	if len(vars) > maxTurnEnvEntries {
		return nil, fmt.Errorf("%w: env has more than %d variables", errTurnEnv, maxTurnEnvEntries)
	}
	names := make([]string, 0, len(vars))
	size := 0
	for name, v := range vars {
		names = append(names, name)
		if v != nil {
			size += len(name) + len(*v)
		}
	}
	if size > maxTurnEnvBytes {
		return nil, fmt.Errorf("%w: env names and values exceed %d bytes", errTurnEnv, maxTurnEnvBytes)
	}
	slices.Sort(names)
	for _, name := range names {
		switch {
		case len(name) > maxTurnEnvName:
			return nil, fmt.Errorf("%w: env has a name longer than %d bytes", errTurnEnv, maxTurnEnvName)
		case !envName(name):
			return nil, fmt.Errorf("%w: env name %q is not a valid variable name", errTurnEnv, name)
		case reservedName(name):
			return nil, fmt.Errorf("%w: env name %q is a reserved variable", errTurnEnv, name)
		case !h.turnEnv.allows(name):
			return nil, fmt.Errorf("%w: env name %q is not in TurnEnvAllowlist", errTurnEnv, name)
		case vars[name] == nil:
			return nil, fmt.Errorf("%w: env value of %q is not a string", errTurnEnv, name)
		case strings.IndexByte(*vars[name], 0) >= 0:
			return nil, fmt.Errorf("%w: env value of %q contains NUL", errTurnEnv, name)
		}
	}
	env := make([]string, 0, len(h.env)+len(names))
	for _, kv := range h.env {
		if name, _, _ := strings.Cut(kv, "="); !hasVar(vars, name) {
			env = append(env, kv)
		}
	}
	for _, name := range names {
		env = append(env, name+"="+*vars[name])
	}
	return env, nil
}

// validJSONText reports whether data, valid JSON, is valid UTF-8 and holds no \u escape of an
// unpaired UTF-16 surrogate. In valid JSON a backslash occurs only inside a string, where it starts
// an escape, so the escapes are found by skipping each one whole.
func validJSONText(data []byte) bool {
	if !utf8.Valid(data) {
		return false
	}
	for i := 0; i < len(data); i++ {
		if data[i] != '\\' {
			continue
		}
		r, ok := uEscape(data[i:])
		if !ok {
			i++ // a two-byte escape such as \\ or \n
			continue
		}
		i += 5
		switch {
		case r >= 0xDC00 && r <= 0xDFFF:
			return false
		case r >= 0xD800 && r <= 0xDBFF:
			lo, ok := uEscape(data[i+1:])
			if !ok || lo < 0xDC00 || lo > 0xDFFF {
				return false
			}
			i += 6
		}
	}
	return true
}

// uEscape decodes the \uXXXX escape at the start of b.
func uEscape(b []byte) (rune, bool) {
	if len(b) < 6 || b[0] != '\\' || b[1] != 'u' {
		return 0, false
	}
	var r rune
	for _, c := range b[2:6] {
		switch {
		case c >= '0' && c <= '9':
			r = r<<4 | rune(c-'0')
		case c >= 'a' && c <= 'f':
			r = r<<4 | rune(c-'a'+10)
		case c >= 'A' && c <= 'F':
			r = r<<4 | rune(c-'A'+10)
		default:
			return 0, false
		}
	}
	return r, true
}

func hasVar(vars map[string]*string, name string) bool {
	_, ok := vars[name]
	return ok
}
