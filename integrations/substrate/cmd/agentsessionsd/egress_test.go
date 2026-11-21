package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"google.golang.org/grpc"

	"github.com/agent-primitives/sessions/api"
	"github.com/agent-primitives/sessions/runtime/substrate"
)

// egressFile is a valid -substrate-egress-policy-file: one HTTPS rule that injects a credential.
const egressFile = `{
  "rules": [
    {"https": {
      "hostnames": ["api.example.com"],
      "effects": {"replaceHeaders": [{"header": "Authorization", "prefix": "Bearer ", "credentialUri": "ate-secret://class/provider/key"}]}
    }},
    {"tlsPassthrough": {"hostnames": ["*.example.org"], "ports": {"numbers": [443]}}}
  ]
}`

var egressFilePolicy = substrate.EgressPolicy{Rules: []substrate.EgressRule{
	{
		Protocol:  substrate.EgressHTTPS,
		Hostnames: []string{"api.example.com"},
		ReplaceHeaders: []substrate.CredentialHeader{
			{Header: "Authorization", Prefix: "Bearer ", CredentialURI: "ate-secret://class/provider/key"},
		},
	},
	{Protocol: substrate.EgressTLSPassthrough, Hostnames: []string{"*.example.org"}, Ports: []int32{443}},
}}

// egressFakeControl is fakeControl with the EgressPolicyClient extension the ate-api adapter has:
// one policy per actor, and an update preconditioned on the stored UID and version.
type egressFakeControl struct {
	*fakeControl
	policies map[string]substrate.EgressPolicy
	versions map[string]int64
}

func (f *egressFakeControl) CreateActorEgressPolicy(_ context.Context, a substrate.ActorRef, p substrate.EgressPolicy) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "egress:"+a.Atespace+"/"+a.Name)
	if _, ok := f.policies[a.Name]; ok {
		return substrate.ErrEgressPolicyExists
	}
	f.policies[a.Name] = p
	f.version(a.Name, 1)
	return nil
}

func (f *egressFakeControl) version(name string, v int64) {
	if f.versions == nil {
		f.versions = map[string]int64{}
	}
	f.versions[name] = v
}

func (f *egressFakeControl) GetActorEgressPolicy(_ context.Context, a substrate.ActorRef) (substrate.EgressPolicyInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "egress-get:"+a.Atespace+"/"+a.Name)
	p, ok := f.policies[a.Name]
	if !ok {
		return substrate.EgressPolicyInfo{}, substrate.ErrEgressPolicyNotFound
	}
	return substrate.EgressPolicyInfo{Policy: p, UID: "policy-" + a.Name, Version: f.versions[a.Name]}, nil
}

func (f *egressFakeControl) UpdateActorEgressPolicy(_ context.Context, a substrate.ActorRef, p substrate.EgressPolicy, precond substrate.EgressPolicyInfo) (substrate.EgressPolicyInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "egress-update:"+a.Atespace+"/"+a.Name)
	if _, ok := f.policies[a.Name]; !ok {
		return substrate.EgressPolicyInfo{}, substrate.ErrEgressPolicyNotFound
	}
	if precond.UID != "policy-"+a.Name || precond.Version != f.versions[a.Name] {
		return substrate.EgressPolicyInfo{}, substrate.ErrActorBusy
	}
	f.policies[a.Name] = p
	f.version(a.Name, precond.Version+1)
	return substrate.EgressPolicyInfo{Policy: p, UID: precond.UID, Version: precond.Version + 1}, nil
}

func (f *egressFakeControl) DeleteActorEgressPolicy(context.Context, substrate.ActorRef) error {
	return errors.New("not used")
}

// A -substrate-egress-policy-file that cannot be read or is not a rules-only ateapi.v1 EgressPolicy
// refuses the start before the journal opens or a control client is built. The errors quote none
// of the file's rules.
func TestSubstrateEgressPolicyFileStartupChecks(t *testing.T) {
	dialed := false
	newControl := func(grpc.ClientConnInterface) substrate.ControlClient { dialed = true; return nil }
	for _, tt := range []struct {
		name    string
		path    string
		want    string
		notWant string
	}{
		{name: "missing", path: filepath.Join(t.TempDir(), "absent.json"), want: "no such file"},
		{name: "not JSON", path: writeFile(t, "p.json", []byte(`rules: []`)), want: "not an ateapi.v1 EgressPolicy in JSON"},
		{name: "unknown field", path: writeFile(t, "p.json", []byte(`{"rules": [{"https": {"hostnames": ["a.example.com"], "port": 443}}]}`)), want: "not an ateapi.v1 EgressPolicy in JSON"},
		{name: "metadata", path: writeFile(t, "p.json", []byte(`{"metadata": {"name": "default"}, "rules": []}`)), want: "metadata is set"},
		{name: "no protocol", path: writeFile(t, "p.json", []byte(`{"rules": [{}]}`)), want: "invalid egress rule 0", notWant: "{"},
		{
			name:    "two credential sources",
			path:    writeFile(t, "p.json", []byte(`{"rules": [{"http": {"hostnames": ["secret-host.example.com"], "effects": {"replaceHeaders": [{"header": "X", "credentialUri": "ate-secret://a/b/c", "actorJwt": {"audiences": ["x"]}}]}}}]}`)),
			want:    "invalid egress rule 0",
			notWant: "secret-host",
		},
		{name: "too large", path: writeFile(t, "p.json", []byte(`{"rules": [], "x": "`+strings.Repeat("a", maxEgressPolicyFile)+`"}`)), want: "larger than"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			journal := filepath.Join(t.TempDir(), "journal.db")
			err := run([]string{"-journal", journal, "-addr", "127.0.0.1:0", "-model", "m",
				"-substrate-harness", "w=t/w", "-substrate-atespace", "sessions",
				"-substrate-egress-policy-file", tt.path}, newControl)
			if err == nil || !strings.Contains(err.Error(), "-substrate-egress-policy-file") || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Run error = %v, want a -substrate-egress-policy-file error with %q", err, tt.want)
			}
			if tt.notWant != "" && strings.Contains(strings.TrimPrefix(err.Error(), tt.path), tt.notWant) {
				t.Fatalf("Run error = %v quotes the policy (%q)", err, tt.notWant)
			}
			if _, statErr := os.Stat(journal); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("a refused start left a journal behind: %v", statErr)
			}
			if dialed {
				t.Fatal("a refused start built a control client")
			}
		})
	}
}

// The policy in -substrate-egress-policy-file is installed on a session's actor between its
// CreateActor and its ResumeActor, and the startup log records the file and the rule count, not
// the rules.
func TestSubstrateEgressPolicyFileIsInstalled(t *testing.T) {
	ctl := &egressFakeControl{fakeControl: &fakeControl{actors: map[string]substrate.ActorStatus{}}, policies: map[string]substrate.EgressPolicy{}}
	f := parseFlags(t, func(grpc.ClientConnInterface) substrate.ControlClient { return ctl },
		"-substrate-harness", "writer=agentsuite/writer", "-substrate-atespace", "sessions", "-ateapi-insecure-skip-verify",
		"-substrate-egress-policy-file", writeFile(t, "policy.json", []byte(egressFile)))
	var logs bytes.Buffer
	backends, _, closeConn, err := f.Backends(slog.New(slog.NewJSONHandler(&logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(closeConn)
	for _, uid := range []string{"sess-a", "sess-b"} {
		if _, err := backends["writer"].Create(t.Context(), &api.SessionSpec{SessionUID: uid}); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(ctl.policies[uid], egressFilePolicy) {
			t.Fatalf("%s policy = %+v, want %+v", uid, ctl.policies[uid], egressFilePolicy)
		}
	}
	want := []string{
		"create:sessions/sess-a:tmpl=agentsuite/writer", "egress:sessions/sess-a", "resume:sessions/sess-a",
		"create:sessions/sess-b:tmpl=agentsuite/writer", "egress:sessions/sess-b", "resume:sessions/sess-b",
	}
	if got := ctl.log(); !slices.Equal(got, want) {
		t.Fatalf("control calls = %v, want %v", got, want)
	}
	out := logs.String()
	if !strings.Contains(out, `"egress_policy_rules":2`) || !strings.Contains(out, `"egress_policy_file":`) {
		t.Fatalf("startup log %s does not record the policy file and its rule count", out)
	}
	for _, leak := range []string{"api.example.com", "example.org", "ate-secret", "Bearer"} {
		if strings.Contains(out, leak) {
			t.Fatalf("log records policy contents %q: %s", leak, out)
		}
	}
}

// startEgressDaemon builds the writer backend of a daemon started on ctl, with file as its
// -substrate-egress-policy-file, or without the flag when file is empty.
func startEgressDaemon(t *testing.T, ctl *egressFakeControl, file string) *substrate.Backend {
	t.Helper()
	args := []string{"-substrate-harness", "writer=agentsuite/writer", "-substrate-atespace", "sessions", "-ateapi-insecure-skip-verify"}
	if file != "" {
		args = append(args, "-substrate-egress-policy-file", writeFile(t, "policy.json", []byte(file)))
	}
	f := parseFlags(t, func(grpc.ClientConnInterface) substrate.ControlClient { return ctl }, args...)
	backends, _, closeConn, err := f.Backends(nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(closeConn)
	return backends["writer"].(*substrate.Backend)
}

// suspendAndRestore suspends sess-a's actor and resumes it through b, leaving only the restore's
// calls in ctl's log.
func suspendAndRestore(t *testing.T, ctl *egressFakeControl, b *substrate.Backend) {
	t.Helper()
	ref, err := b.Snapshot(t.Context(), api.Incarnation{ID: "sess-a"}, api.SnapshotExternal)
	if err != nil {
		t.Fatal(err)
	}
	ctl.mu.Lock()
	ctl.calls = nil
	ctl.mu.Unlock()
	if _, err := b.Restore(t.Context(), ref); err != nil {
		t.Fatal(err)
	}
}

// A changed -substrate-egress-policy-file reaches an existing session's actor at the first resume
// after the restart that reads it: the stored policy is read and replaced. An unchanged file costs
// the read only.
func TestSubstrateEgressPolicyFileChangeReachesExistingActors(t *testing.T) {
	ctl := &egressFakeControl{fakeControl: &fakeControl{actors: map[string]substrate.ActorStatus{}}, policies: map[string]substrate.EgressPolicy{}}
	b := startEgressDaemon(t, ctl, egressFile)
	if _, err := b.Create(t.Context(), &api.SessionSpec{SessionUID: "sess-a"}); err != nil {
		t.Fatal(err)
	}
	suspendAndRestore(t, ctl, b)
	if want := []string{"egress-get:sessions/sess-a", "resume:sessions/sess-a"}; !slices.Equal(ctl.log(), want) {
		t.Fatalf("control calls for an unchanged file = %v, want %v", ctl.log(), want)
	}

	changed := `{"rules": [{"https": {"hostnames": ["api.example.com"]}}]}`
	suspendAndRestore(t, ctl, startEgressDaemon(t, ctl, changed))
	if want := []string{"egress-get:sessions/sess-a", "egress-update:sessions/sess-a", "resume:sessions/sess-a"}; !slices.Equal(ctl.log(), want) {
		t.Fatalf("control calls for a changed file = %v, want %v", ctl.log(), want)
	}
	want := substrate.EgressPolicy{Rules: []substrate.EgressRule{{Protocol: substrate.EgressHTTPS, Hostnames: []string{"api.example.com"}}}}
	if got := ctl.policies["sess-a"]; !reflect.DeepEqual(got, want) {
		t.Fatalf("policy after the restart = %+v, want %+v", got, want)
	}
}

// Removing -substrate-egress-policy-file does not revoke: after a restart without it, a resume makes
// no egress call and the actor keeps its stored rules. A restart with a file holding {} (no rules)
// clears them to an empty rule set at the next resume, with an update preconditioned on the read.
func TestSubstrateEgressPolicyFileRevocation(t *testing.T) {
	ctl := &egressFakeControl{fakeControl: &fakeControl{actors: map[string]substrate.ActorStatus{}}, policies: map[string]substrate.EgressPolicy{}}
	if _, err := startEgressDaemon(t, ctl, egressFile).Create(t.Context(), &api.SessionSpec{SessionUID: "sess-a"}); err != nil {
		t.Fatal(err)
	}

	suspendAndRestore(t, ctl, startEgressDaemon(t, ctl, ""))
	if want := []string{"resume:sessions/sess-a"}; !slices.Equal(ctl.log(), want) {
		t.Fatalf("control calls without the flag = %v, want %v", ctl.log(), want)
	}
	if got := ctl.policies["sess-a"]; !reflect.DeepEqual(got, egressFilePolicy) {
		t.Fatalf("policy after a restart without the flag = %+v, want the stored %+v", got, egressFilePolicy)
	}

	suspendAndRestore(t, ctl, startEgressDaemon(t, ctl, "{}"))
	if want := []string{"egress-get:sessions/sess-a", "egress-update:sessions/sess-a", "resume:sessions/sess-a"}; !slices.Equal(ctl.log(), want) {
		t.Fatalf("control calls with {} = %v, want %v", ctl.log(), want)
	}
	if got, ok := ctl.policies["sess-a"]; !ok || got.Rules == nil || len(got.Rules) != 0 {
		t.Fatalf("policy after a restart with {} = %+v (stored %v), want an empty rule set", got, ok)
	}
}

// Without -substrate-egress-policy-file the backend makes no egress call, and the log says so.
func TestSubstrateWithoutEgressPolicyFile(t *testing.T) {
	ctl := &egressFakeControl{fakeControl: &fakeControl{actors: map[string]substrate.ActorStatus{}}, policies: map[string]substrate.EgressPolicy{}}
	f := parseFlags(t, func(grpc.ClientConnInterface) substrate.ControlClient { return ctl },
		"-substrate-harness", "writer=agentsuite/writer", "-substrate-atespace", "sessions", "-ateapi-insecure-skip-verify")
	var logs bytes.Buffer
	backends, _, closeConn, err := f.Backends(slog.New(slog.NewJSONHandler(&logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(closeConn)
	if _, err := backends["writer"].Create(t.Context(), &api.SessionSpec{SessionUID: "sess-a"}); err != nil {
		t.Fatal(err)
	}
	if want := []string{"create:sessions/sess-a:tmpl=agentsuite/writer", "resume:sessions/sess-a"}; !slices.Equal(ctl.log(), want) {
		t.Fatalf("control calls = %v, want %v", ctl.log(), want)
	}
	if !strings.Contains(logs.String(), `"egress_policy_rules":-1`) {
		t.Fatalf("startup log %s does not record that no policy is configured", logs.String())
	}
}

// A control client that cannot install the configured policy is refused when the backends are
// built, before any session.
func TestSubstrateEgressPolicyFileNeedsAnEgressClient(t *testing.T) {
	ctl := &fakeControl{actors: map[string]substrate.ActorStatus{}}
	f := parseFlags(t, func(grpc.ClientConnInterface) substrate.ControlClient { return ctl },
		"-substrate-harness", "writer=agentsuite/writer", "-substrate-atespace", "sessions", "-ateapi-insecure-skip-verify",
		"-substrate-egress-policy-file", writeFile(t, "policy.json", []byte(egressFile)))
	if _, _, _, err := f.Backends(nil); !errors.Is(err, substrate.ErrEgressPolicyUnsupported) {
		t.Fatalf("Backends error = %v, want ErrEgressPolicyUnsupported", err)
	}
	if len(ctl.log()) != 0 {
		t.Fatalf("control calls = %v, want none", ctl.log())
	}
}
