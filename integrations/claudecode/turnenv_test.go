package claudecode

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
	"testing"
)

// turnEnvSentinel is a value no error may carry.
const turnEnvSentinel = "turn-env-sentinel-value"

func TestTurnEnvAllowlistValidation(t *testing.T) {
	t.Parallel()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	valid := Config{Binary: exe, WorkDir: t.TempDir(), PermissionMode: "dontAsk"}
	for _, test := range []struct {
		name    string
		entries []string
		want    string // error substring; empty when New must accept the entries
	}{
		{name: "exact and prefix", entries: []string{"ANTHROPIC_BASE_URL", "GIT_CONFIG_*", "HTTPS_PROXY"}},
		{name: "names beside reserved prefixes", entries: []string{"LDX", "L_*", "DYLD"}},
		{name: "empty entry", entries: []string{""}, want: "invalid TurnEnvAllowlist entry"},
		{name: "bare star", entries: []string{"*"}, want: "invalid TurnEnvAllowlist entry"},
		{name: "inner star", entries: []string{"A*B"}, want: "invalid TurnEnvAllowlist entry"},
		{name: "double star", entries: []string{"A**"}, want: "invalid TurnEnvAllowlist entry"},
		{name: "name with =", entries: []string{"A=B"}, want: "invalid TurnEnvAllowlist entry"},
		{name: "name with space", entries: []string{" A"}, want: "invalid TurnEnvAllowlist entry"},
		{name: "reserved PATH", entries: []string{"PATH"}, want: `"PATH" is a reserved variable`},
		{name: "reserved key", entries: []string{"ANTHROPIC_API_KEY"}, want: "is a reserved variable"},
		{name: "reserved token", entries: []string{"ANTHROPIC_AUTH_TOKEN"}, want: "is a reserved variable"},
		{name: "reserved config dir", entries: []string{"CLAUDE_CONFIG_DIR"}, want: "is a reserved variable"},
		{name: "reserved node options", entries: []string{"NODE_OPTIONS"}, want: "is a reserved variable"},
		{name: "reserved loader name", entries: []string{"LD_LIBRARY_PATH"}, want: "is a reserved variable"},
		{name: "reserved darwin loader name", entries: []string{"DYLD_LIBRARY_PATH"}, want: "is a reserved variable"},
		{name: "reserved bash function", entries: []string{"BASH_FUNC_x"}, want: "is a reserved variable"},
		{name: "reserved PS4", entries: []string{"PS4"}, want: "is a reserved variable"},
		{name: "reserved PROMPT_COMMAND", entries: []string{"PROMPT_COMMAND"}, want: "is a reserved variable"},
		{name: "prefix equals bash function prefix", entries: []string{"BASH_FUNC_*"}, want: "covers a reserved variable"},
		{name: "prefix inside bash function prefix", entries: []string{"BASH_FUNC_x*"}, want: "covers a reserved variable"},
		{name: "prefix covers PS4", entries: []string{"PS*"}, want: "covers a reserved variable"},
		{name: "prefix covers PROMPT_COMMAND", entries: []string{"PROMPT_*"}, want: "covers a reserved variable"},
		{name: "prefix covers credential", entries: []string{"ANTHROPIC_*"}, want: `"ANTHROPIC_*" covers a reserved variable`},
		{name: "prefix covers loader prefix", entries: []string{"LD*"}, want: "covers a reserved variable"},
		{name: "prefix covers LANG and LD_", entries: []string{"L*"}, want: "covers a reserved variable"},
		{name: "prefix inside loader prefix", entries: []string{"LD_FOO*"}, want: "covers a reserved variable"},
		{name: "prefix equals loader prefix", entries: []string{"DYLD_*"}, want: "covers a reserved variable"},
		{name: "prefix covers PATH", entries: []string{"PA*"}, want: "covers a reserved variable"},
		{name: "prefix covers ENV", entries: []string{"EN*"}, want: "covers a reserved variable"},
		{name: "exact OAuth token", entries: []string{"CLAUDE_CODE_OAUTH_TOKEN"}, want: `"CLAUDE_CODE_OAUTH_TOKEN" is a reserved variable`},
		{name: "exact OAuth refresh token", entries: []string{"CLAUDE_CODE_OAUTH_REFRESH_TOKEN"}, want: "is a reserved variable"},
		{name: "prefix covers OAuth prefix", entries: []string{"CLAUDE_CODE_*"}, want: `"CLAUDE_CODE_*" covers a reserved variable`},
		{name: "wider prefix covers OAuth prefix", entries: []string{"CLAUDE_*"}, want: "covers a reserved variable"},
		{name: "prefix inside OAuth prefix", entries: []string{"CLAUDE_CODE_OAUTH_TOKEN*"}, want: "covers a reserved variable"},
		{name: "exact Claude Code knob", entries: []string{"CLAUDE_CODE_MAX_OUTPUT_TOKENS"}},
		{name: "one bad entry among good", entries: []string{"GIT_CONFIG_*", "HOME"}, want: `"HOME" is a reserved variable`},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			cfg := valid
			cfg.TurnEnvAllowlist = test.entries
			h, err := New(cfg)
			if test.want == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.want) || h != nil {
				t.Fatalf("New = %v, %v; want an error containing %q", h, err, test.want)
			}
		})
	}
}

// TestTurnEnvDisabled: without TurnEnvAllowlist, Start.Config is not read, malformed or not. Not
// parallel: it sets environment variables.
func TestTurnEnvDisabled(t *testing.T) {
	t.Setenv("ANTHROPIC_BASE_URL", "http://base.invalid")
	h := newTestHarness(t, Config{})
	setScenario(t, h, scenario{Fixture: "text_turn.jsonl", EnvValues: []string{"ANTHROPIC_BASE_URL", "GIT_CONFIG_COUNT"}})
	for i, config := range []string{
		`{"env":{"ANTHROPIC_BASE_URL":"http://turn.invalid","GIT_CONFIG_COUNT":"0"}}`,
		`not json`,
		`{"env":{"PATH":"/x"}}`,
	} {
		s := startFor("uid", fmt.Sprintf("e%d", i), nil, "go")
		if i > 0 {
			s.History = priorTurn(fmt.Sprintf("e%d", i-1))
		}
		s.Config = []byte(config)
		if err := h.Run(context.Background(), s, &recordingSink{t: t}); err != nil {
			t.Fatalf("turn %d: %v", i, err)
		}
	}
	got := starts(t, h)
	if len(got) != 3 {
		t.Fatalf("started %d processes, want 3", len(got))
	}
	want := map[string]string{"ANTHROPIC_BASE_URL": "http://base.invalid"}
	for i, r := range got {
		if !maps.Equal(r.Values, want) {
			t.Errorf("turn %d env values = %v, want %v", i, r.Values, want)
		}
	}
}

// TestTurnEnvApplied: allowlisted names from Start.Config reach that turn's process only, over
// the base value; other members and a differently cased "env" member are ignored. Not parallel:
// it sets environment variables.
func TestTurnEnvApplied(t *testing.T) {
	t.Setenv("ANTHROPIC_BASE_URL", "http://base.invalid")
	h := newTestHarness(t, Config{TurnEnvAllowlist: []string{"ANTHROPIC_BASE_URL", "GIT_CONFIG_*"}})
	base := slices.Clone(h.env)
	watch := []string{"ANTHROPIC_BASE_URL", "GIT_CONFIG_COUNT", "GIT_CONFIG_KEY_0", "GIT_CONFIG_VALUE_0", "PATH"}
	setScenario(t, h, scenario{Fixture: "text_turn.jsonl", EnvValues: watch})
	path := os.Getenv("PATH")
	baseOnly := map[string]string{"ANTHROPIC_BASE_URL": "http://base.invalid", "PATH": path}
	turns := []struct {
		config string
		want   map[string]string
	}{
		{
			config: `{"other":{"x":1},"ENV":{"GIT_CONFIG_COUNT":"9"},"env":{"ANTHROPIC_BASE_URL":"http://turn.invalid",` +
				`"GIT_CONFIG_COUNT":"1","GIT_CONFIG_KEY_0":"http.proxy","GIT_CONFIG_VALUE_0":"http://proxy.invalid"}}`,
			want: map[string]string{
				"ANTHROPIC_BASE_URL": "http://turn.invalid", "GIT_CONFIG_COUNT": "1",
				"GIT_CONFIG_KEY_0": "http.proxy", "GIT_CONFIG_VALUE_0": "http://proxy.invalid", "PATH": path,
			},
		},
		// The next turn without env starts from the base environment again.
		{config: ``, want: baseOnly},
		{config: `{"other":true}`, want: baseOnly},
		{config: `{"env":{}}`, want: baseOnly},
		// Valid UTF-8, a surrogate pair and an escaped U+FFFD are applied as decoded; an escaped
		// backslash before "ud800" is not a surrogate escape. A producer's member is not checked.
		{
			config: `{"other":"a` + "\xff" + `b","env":{"GIT_CONFIG_KEY_0":"😀é","GIT_CONFIG_VALUE_0":"�",` +
				`"GIT_CONFIG_COUNT":"\\ud800"}}`,
			want: map[string]string{
				"ANTHROPIC_BASE_URL": "http://base.invalid", "GIT_CONFIG_COUNT": `\ud800`,
				"GIT_CONFIG_KEY_0": "\U0001F600é", "GIT_CONFIG_VALUE_0": "�", "PATH": path,
			},
		},
		// A null duplicate before a string is replaced, so the string is used.
		{
			config: `{"env":{"GIT_CONFIG_COUNT":null,"GIT_CONFIG_COUNT":"0"}}`,
			want:   map[string]string{"ANTHROPIC_BASE_URL": "http://base.invalid", "GIT_CONFIG_COUNT": "0", "PATH": path},
		},
		// The last duplicate wins; an empty value is set, not dropped.
		{
			config: `{"env":{"GIT_CONFIG_COUNT":"1"},"env":{"GIT_CONFIG_COUNT":"2","GIT_CONFIG_COUNT":"0","ANTHROPIC_BASE_URL":""}}`,
			want:   map[string]string{"ANTHROPIC_BASE_URL": "", "GIT_CONFIG_COUNT": "0", "PATH": path},
		},
	}
	for i, turn := range turns {
		s := startFor("uid", fmt.Sprintf("e%d", i), nil, "go")
		if i > 0 {
			s.History = priorTurn(fmt.Sprintf("e%d", i-1))
		}
		s.Config = []byte(turn.config)
		if err := h.Run(context.Background(), s, &recordingSink{t: t}); err != nil {
			t.Fatalf("turn %d: %v", i, err)
		}
	}
	got := starts(t, h)
	if len(got) != len(turns) {
		t.Fatalf("started %d processes, want %d", len(got), len(turns))
	}
	for i, r := range got {
		if !maps.Equal(r.Values, turns[i].want) {
			t.Errorf("turn %d env values = %v, want %v", i, r.Values, turns[i].want)
		}
		for _, name := range watch {
			if n := countOf(r.Env, name); n > 1 {
				t.Errorf("turn %d env has %s %d times", i, name, n)
			}
		}
	}
	if !slices.Equal(h.env, base) {
		t.Fatal("a turn changed the base environment")
	}
}

// TestTurnEnvRefused: an invalid per-turn environment refuses the turn before claude starts, with
// an error that never carries a value.
func TestTurnEnvRefused(t *testing.T) {
	t.Parallel()
	many := make([]string, 0, maxTurnEnvEntries+1)
	for i := range maxTurnEnvEntries + 1 {
		many = append(many, fmt.Sprintf(`"GIT_CONFIG_KEY_%d":%q`, i, turnEnvSentinel))
	}
	v := turnEnvSentinel
	for _, test := range []struct {
		name   string
		config string
		want   string
	}{
		{name: "not json", config: `{"env":` + v, want: "Config is not a JSON object"},
		{name: "array", config: `["` + v + `"]`, want: "Config is not a JSON object"},
		{name: "null", config: `null`, want: "Config is not a JSON object"},
		{name: "string", config: `"` + v + `"`, want: "Config is not a JSON object"},
		{name: "env array", config: `{"env":["` + v + `"]}`, want: "env is not an object of string values"},
		{name: "env null", config: `{"env":null}`, want: "env is not an object of string values"},
		{name: "env string", config: `{"env":"` + v + `"}`, want: "env is not an object of string values"},
		{name: "number value", config: `{"env":{"GIT_CONFIG_COUNT":1}}`, want: "env is not an object of string values"},
		{name: "null value", config: `{"env":{"GIT_CONFIG_COUNT":null}}`, want: `env value of "GIT_CONFIG_COUNT" is not a string`},
		{name: "null among values", config: `{"env":{"GIT_CONFIG_COUNT":"` + v + `","GIT_CONFIG_KEY_0":null}}`, want: `env value of "GIT_CONFIG_KEY_0" is not a string`},
		{name: "object value", config: `{"env":{"GIT_CONFIG_COUNT":{"v":"` + v + `"}}}`, want: "env is not an object of string values"},
		{name: "not allowlisted", config: `{"env":{"HTTPS_PROXY":"` + v + `"}}`, want: `env name "HTTPS_PROXY" is not in TurnEnvAllowlist`},
		{name: "wrong case", config: `{"env":{"git_config_count":"` + v + `"}}`, want: `"git_config_count" is not in TurnEnvAllowlist`},
		{name: "reserved", config: `{"env":{"PATH":"` + v + `"}}`, want: `env name "PATH" is a reserved variable`},
		{name: "reserved credential", config: `{"env":{"ANTHROPIC_API_KEY":"` + v + `"}}`, want: "is a reserved variable"},
		{name: "reserved loader", config: `{"env":{"LD_LIBRARY_PATH":"` + v + `"}}`, want: "is a reserved variable"},
		{name: "reserved bash function", config: `{"env":{"BASH_FUNC_git%%":"` + v + `"}}`, want: "is a reserved variable"},
		{name: "reserved PS4", config: `{"env":{"PS4":"` + v + `"}}`, want: "is a reserved variable"},
		{name: "reserved PROMPT_COMMAND", config: `{"env":{"PROMPT_COMMAND":"` + v + `"}}`, want: "is a reserved variable"},
		{name: "empty name", config: `{"env":{"":"` + v + `"}}`, want: "is not a valid variable name"},
		{name: "name with =", config: `{"env":{"GIT_CONFIG_A=B":"` + v + `"}}`, want: "is not a valid variable name"},
		{name: "name with NUL", config: `{"env":{"GIT_CONFIG_\u0000":"` + v + `"}}`, want: "is not a valid variable name"},
		{name: "value with NUL", config: `{"env":{"GIT_CONFIG_COUNT":"` + v + `\u0000"}}`, want: `env value of "GIT_CONFIG_COUNT" contains NUL`},
		{name: "invalid UTF-8 value", config: `{"env":{"GIT_CONFIG_COUNT":"a` + "\xff" + `b"}}`, want: "env is not valid UTF-8"},
		{name: "invalid UTF-8 name", config: `{"env":{"GIT_CONFIG_` + "\xff" + `":"` + v + `"}}`, want: "env is not valid UTF-8"},
		{name: "lone high surrogate", config: `{"env":{"GIT_CONFIG_COUNT":"a\ud800b"}}`, want: "env is not valid UTF-8"},
		{name: "lone high surrogate uppercase", config: `{"env":{"GIT_CONFIG_COUNT":"a\uD800b"}}`, want: "env is not valid UTF-8"},
		{name: "lone low surrogate", config: `{"env":{"GIT_CONFIG_COUNT":"a\udc00b"}}`, want: "env is not valid UTF-8"},
		{name: "high surrogate then other escape", config: `{"env":{"GIT_CONFIG_COUNT":"\ud800A"}}`, want: "env is not valid UTF-8"},
		{name: "high surrogate then letter", config: `{"env":{"GIT_CONFIG_COUNT":"\ud800A"}}`, want: "env is not valid UTF-8"},
		{name: "high surrogate at end", config: `{"env":{"GIT_CONFIG_COUNT":"\ud800"}}`, want: "env is not valid UTF-8"},
		{name: "surrogates in wrong order", config: `{"env":{"GIT_CONFIG_COUNT":"\udc00\ud800"}}`, want: "env is not valid UTF-8"},
		{name: "two high surrogates", config: `{"env":{"GIT_CONFIG_COUNT":"\ud800𐀀"}}`, want: "env is not valid UTF-8"},
		// The encoding is checked first, so a later check never sees the replaced value.
		{name: "surrogate under reserved name", config: `{"env":{"PATH":"\ud800"}}`, want: "env is not valid UTF-8"},
		{name: "reserved OAuth token", config: `{"env":{"CLAUDE_CODE_OAUTH_TOKEN":"` + v + `"}}`, want: `"CLAUDE_CODE_OAUTH_TOKEN" is a reserved variable`},
		// A non-string duplicate refuses the turn even when a later one is a string.
		{name: "number before string duplicate", config: `{"env":{"GIT_CONFIG_COUNT":1,"GIT_CONFIG_COUNT":"0"}}`, want: "env is not an object of string values"},
		{name: "string before null duplicate", config: `{"env":{"GIT_CONFIG_COUNT":"0","GIT_CONFIG_COUNT":null}}`, want: `env value of "GIT_CONFIG_COUNT" is not a string`},
		{name: "too many", config: `{"env":{` + strings.Join(many, ",") + `}}`, want: "more than 256 variables"},
		{name: "long name", config: `{"env":{"GIT_CONFIG_` + strings.Repeat("X", maxTurnEnvName) + `":"` + v + `"}}`, want: "name longer than 256 bytes"},
		{name: "too large", config: `{"env":{"GIT_CONFIG_VALUE_0":"` + v + strings.Repeat("x", maxTurnEnvBytes) + `"}}`, want: "exceed 65536 bytes"},
		// Names are checked in order, so the same Config always gets the same error.
		{name: "first bad name in order", config: `{"env":{"Z_NOT":"v","A_NOT":"v","PATH":"v"}}`, want: `"A_NOT" is not in TurnEnvAllowlist`},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			h := newTestHarness(t, Config{TurnEnvAllowlist: []string{"ANTHROPIC_BASE_URL", "GIT_CONFIG_*"}})
			setScenario(t, h, scenario{Fixture: "text_turn.jsonl"})
			s := startFor("uid", "e1", nil, "go")
			s.Config = []byte(test.config)
			err := h.Run(context.Background(), s, &recordingSink{t: t})
			if !errors.Is(err, errTurnEnv) || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Run = %v, want %v containing %q", err, errTurnEnv, test.want)
			}
			if strings.Contains(err.Error(), v) {
				t.Fatal("error carries a value")
			}
			if got := starts(t, h); len(got) != 0 {
				t.Fatalf("spawned %d processes", len(got))
			}
		})
	}
}

// TestTurnEnvRefusalKeepsSession: a refused first turn is like any refused input: the execution
// is not retried, and the next turn still starts the session at its first attempt.
func TestTurnEnvRefusalKeepsSession(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t, Config{TurnEnvAllowlist: []string{"GIT_CONFIG_*"}})
	setScenario(t, h, scenario{Fixture: "text_turn.jsonl"})
	ctx := context.Background()
	bad := startFor("uid", "e1", nil, "go")
	bad.Config = []byte(`{"env":{"PATH":"/x"}}`)
	if err := h.Run(ctx, bad, &recordingSink{t: t}); !errors.Is(err, errTurnEnv) {
		t.Fatalf("Run = %v, want %v", err, errTurnEnv)
	}
	if err := h.Run(ctx, bad, &recordingSink{t: t}); !errors.Is(err, errRerun) {
		t.Fatalf("retry = %v, want %v", err, errRerun)
	}
	if err := h.Run(ctx, startFor("uid", "e2", priorTurn("e1"), "go"), &recordingSink{t: t}); err != nil {
		t.Fatal(err)
	}
	got := starts(t, h)
	if len(got) != 1 || !slices.Contains(got[0].Args, "--session-id") || !slices.Contains(got[0].Args, sessionID("uid", 0)) {
		t.Fatalf("starts = %+v, want one --session-id %s", got, sessionID("uid", 0))
	}
}

func countOf(names []string, name string) int {
	n := 0
	for _, x := range names {
		if x == name {
			n++
		}
	}
	return n
}
