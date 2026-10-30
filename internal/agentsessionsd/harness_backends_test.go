package agentsessionsd

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agent-primitives/sessions/api"
	"github.com/agent-primitives/sessions/client"
	"github.com/agent-primitives/sessions/harness/echoagent"
	"github.com/agent-primitives/sessions/placement"
	"github.com/agent-primitives/sessions/runtime/local"
)

// fakeBackends is a HarnessBackends that serves replyHarness, on a local backend, under each name
// given with -fake-harness. It records what Run asked of it.
type fakeBackends struct {
	names       []string
	harnessErr  error
	extraBuilt  string // a name Backends builds that Harnesses did not return
	backendsErr error
	nilClose    bool // Backends returns a nil close func
	// dialed counts the incarnations the dialer Backends returns was asked to reach.
	// It is a pointer so the table tests can copy a fakeBackends.
	dialed *atomic.Int32

	built  int
	closed int
	logger *slog.Logger
}

func (f *fakeBackends) RegisterFlags(fs *flag.FlagSet) {
	fs.Func("fake-harness", "serve replyHarness under this name; repeatable", func(v string) error {
		f.names = append(f.names, v)
		return nil
	})
}

func (f *fakeBackends) Harnesses() ([]string, error) { return f.names, f.harnessErr }

func (f *fakeBackends) Backends(logger *slog.Logger) (map[string]placement.Backend, placement.Dialer, func(), error) {
	f.built++
	f.logger = logger
	if f.backendsErr != nil {
		return nil, nil, nil, f.backendsErr
	}
	var locals []*local.Backend
	backends := map[string]placement.Backend{}
	names := slices.Clone(f.names)
	if f.extraBuilt != "" {
		names = append(names, f.extraBuilt)
	}
	for _, name := range names {
		b := local.New(replyHarness{})
		locals = append(locals, b)
		backends[name] = b
	}
	if f.dialed == nil {
		f.dialed = new(atomic.Int32)
	}
	dial := func(inc api.Incarnation) (api.Harness, func() error, error) {
		f.dialed.Add(1)
		return placement.DefaultDial(inc)
	}
	if f.nilClose {
		return backends, dial, nil, nil
	}
	return backends, dial, func() {
		f.closed++
		for _, b := range locals {
			_ = b.Close()
		}
	}, nil
}

// A HarnessBackends harness is served through Run like any static harness: Run builds its backend
// once, with the daemon's logger, routes turns to it by name through the dialer Backends returned,
// and closes it when the server stops. Other harnesses keep the default dialer.
func TestRunServesHarnessBackends(t *testing.T) {
	fake := &fakeBackends{}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	addrs := make(chan string, 1)
	done := make(chan error, 1)
	go func() {
		done <- Run([]string{"-journal", filepath.Join(t.TempDir(), "journal.db"), "-addr", "127.0.0.1:0", "-model", "m", "-fake-harness", "extra"},
			WithHarnessBackends(fake), withContext(ctx), withListening(func(addr string) { addrs <- addr }))
	}()
	var addr string
	select {
	case addr = <-addrs:
	case err := <-done:
		t.Fatalf("Run returned before serving: %v", err)
	}
	c, err := client.Dial(addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	tctx, tcancel := context.WithTimeout(ctx, 10*time.Second)
	defer tcancel()
	turn, err := c.Exec(tctx, client.ExecOptions{Harness: "extra", Inputs: []string{"hi"}})
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if turn.Output != "reply:hi" {
		t.Fatalf("output = %q, want reply:hi", turn.Output)
	}
	if n := fake.dialed.Load(); n == 0 {
		t.Fatal("the turn on the HarnessBackends harness did not use the dialer Backends returned")
	}
	dialed := fake.dialed.Load()
	// The echo turn fails at its model call (-model m has no server), after placement dialed it.
	if _, err := c.Exec(tctx, client.ExecOptions{Harness: "echo", Inputs: []string{"hi"}}); err == nil || !strings.Contains(err.Error(), "openai") {
		t.Fatalf("exec on echo: %v, want the model call to fail after placement", err)
	}
	if n := fake.dialed.Load(); n != dialed {
		t.Fatalf("a turn on the built-in echo harness used the HarnessBackends dialer (%d dials, want %d)", n, dialed)
	}
	if fake.built != 1 || fake.logger == nil {
		t.Fatalf("Backends called %d times with logger %v, want once with the daemon's logger", fake.built, fake.logger)
	}
	if fake.closed != 0 {
		t.Fatal("the backends were closed while the server was serving")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
	if fake.closed != 1 {
		t.Fatalf("closeBackends called %d times after Run returned, want 1", fake.closed)
	}
}

// harnessRegistry serves a HarnessBackends harness as a static entry, reserved like the built-in
// and -harness names.
func TestHarnessRegistryServesExtraBackends(t *testing.T) {
	extra := local.New(replyHarness{})
	t.Cleanup(func() { _ = extra.Close() })
	registry, closeBackends, err := harnessRegistry("test-model", remoteHarnesses{"remote": "127.0.0.1:9000"}, echoagent.Model, nil, nil, nil, map[string]placement.Backend{"extra": extra})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(closeBackends)
	if got, want := registry.Names(), []string{"chat", "echo", "extra", "remote"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("harnesses = %v, want %v", got, want)
	}
	if !registry.Reserved("extra") {
		t.Fatal("a HarnessBackends name must be reserved like the other static names")
	}
	for _, tt := range []struct {
		name  string
		model string
		extra string
		want  string
	}{
		{name: "no model", extra: "extra", want: "harness backends require -model"},
		{name: "built-in name", model: "test-model", extra: "chat", want: `harness "chat" is already served`},
		{name: "remote name", model: "test-model", extra: "remote", want: `harness "remote" is already served`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var remotes remoteHarnesses
			if tt.model != "" {
				remotes = remoteHarnesses{"remote": "127.0.0.1:9000"}
			}
			_, closeBackends, err := harnessRegistry(tt.model, remotes, echoagent.Model, nil, nil, nil, map[string]placement.Backend{tt.extra: extra})
			if err == nil {
				closeBackends()
				t.Fatalf("harnessRegistry accepted extra harness %q", tt.extra)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("harnessRegistry error = %v, want %q", err, tt.want)
			}
		})
	}
}

// Run checks a HarnessBackends build's names as it checks -harness ones, before it builds the
// backends or opens the journal, and refuses backends that are not the names it checked.
func TestRunChecksHarnessBackends(t *testing.T) {
	for _, tt := range []struct {
		name      string
		args      []string
		fake      fakeBackends
		want      string
		wantBuilt bool
	}{
		{name: "no model", args: []string{"-fake-harness", "extra"}, want: "harness extra requires -model"},
		{name: "echo", args: []string{"-model", "m", "-fake-harness", "echo"}, want: `harness "echo" is already served`},
		{name: "chat", args: []string{"-model", "m", "-fake-harness", "chat"}, want: `harness "chat" is already served`},
		{name: "a -harness name", args: []string{"-model", "m", "-harness", "x=127.0.0.1:9000", "-fake-harness", "x"}, want: `harness "x" is given with -harness and is also configured by this build's flags`},
		{name: "a name twice", args: []string{"-model", "m", "-fake-harness", "x", "-fake-harness", "x"}, want: `harness "x" is configured twice`},
		{name: "an empty name", args: []string{"-model", "m", "-fake-harness", ""}, want: "a harness has no name"},
		{name: "the build's own refusal", args: []string{"-model", "m"}, fake: fakeBackends{harnessErr: errors.New("bad backend flags")}, want: "bad backend flags"},
		{name: "a build failure", args: []string{"-model", "m", "-fake-harness", "x"}, fake: fakeBackends{backendsErr: errors.New("cannot build")}, want: "cannot build", wantBuilt: true},
		{name: "a name built but not checked", args: []string{"-model", "m", "-fake-harness", "x"}, fake: fakeBackends{extraBuilt: "echo"}, want: `harness backends: built ["echo" "x"], want ["x"]`, wantBuilt: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			journal := filepath.Join(t.TempDir(), "journal.db")
			fake := tt.fake
			err := Run(append([]string{"-journal", journal, "-addr", "127.0.0.1:0"}, tt.args...), WithHarnessBackends(&fake))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Run error = %v, want %q", err, tt.want)
			}
			if errors.Is(err, ErrUsage) {
				t.Fatalf("Run error = %v, want a configuration error (exit status 1), not a usage error", err)
			}
			if _, statErr := os.Stat(journal); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("a refused start left a journal behind: %v", statErr)
			}
			if built := fake.built > 0; built != tt.wantBuilt {
				t.Fatalf("Backends called %d times, want called: %v", fake.built, tt.wantBuilt)
			}
			if tt.wantBuilt && tt.fake.backendsErr == nil && fake.closed != 1 {
				t.Fatalf("closeBackends called %d times after a refused start, want 1", fake.closed)
			}
		})
	}
}

// Without WithHarnessBackends the build's flags do not exist, and with it a flag that does not
// parse is a usage error like Run's own.
func TestHarnessBackendsFlagsAreTheBuilds(t *testing.T) {
	journal := filepath.Join(t.TempDir(), "journal.db")
	if err := Run([]string{"-journal", journal, "-fake-harness", "x"}, withOutput(&strings.Builder{})); !errors.Is(err, ErrUsage) || !strings.Contains(err.Error(), "flag provided but not defined: -fake-harness") {
		t.Fatalf("Run error = %v, want an undefined-flag usage error", err)
	}
	fake := &fakeBackends{}
	if err := Run([]string{"-journal", journal, "-fake-harness"}, WithHarnessBackends(fake), withOutput(&strings.Builder{})); !errors.Is(err, ErrUsage) {
		t.Fatalf("Run error = %v, want a usage error", err)
	}
	if fake.built != 0 {
		t.Fatal("a usage error built the backends")
	}
}

// A HarnessBackends build may return a nil close func: Run neither calls it nor fails on it, on a
// refused start or after serving.
func TestRunAcceptsNilCloseBackends(t *testing.T) {
	stopped, cancel := context.WithCancel(t.Context())
	cancel()
	t.Run("refused start", func(t *testing.T) {
		fake := &fakeBackends{nilClose: true, extraBuilt: "echo"}
		err := Run([]string{"-journal", filepath.Join(t.TempDir(), "journal.db"), "-addr", "127.0.0.1:0", "-model", "m", "-fake-harness", "x"},
			WithHarnessBackends(fake), withContext(stopped))
		if err == nil || !strings.Contains(err.Error(), "harness backends: built") {
			t.Fatalf("Run error = %v, want the built-names refusal", err)
		}
		if fake.built != 1 {
			t.Fatalf("Backends called %d times, want 1", fake.built)
		}
	})
	t.Run("served and stopped", func(t *testing.T) {
		fake := &fakeBackends{nilClose: true}
		if err := Run([]string{"-journal", filepath.Join(t.TempDir(), "journal.db"), "-addr", "127.0.0.1:0", "-model", "m", "-fake-harness", "x"},
			WithHarnessBackends(fake), withContext(stopped)); err != nil {
			t.Fatalf("Run: %v", err)
		}
		if fake.built != 1 {
			t.Fatalf("Backends called %d times, want 1", fake.built)
		}
	})
}
