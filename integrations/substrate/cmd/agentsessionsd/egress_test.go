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

// egressFakeControl is fakeControl with the EgressPolicyClient extension the ate-api adapter has.
type egressFakeControl struct {
	*fakeControl
	policies map[string]substrate.EgressPolicy
}

func (f *egressFakeControl) CreateActorEgressPolicy(_ context.Context, a substrate.ActorRef, p substrate.EgressPolicy) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "egress:"+a.Atespace+"/"+a.Name)
	if _, ok := f.policies[a.Name]; ok {
		return substrate.ErrEgressPolicyExists
	}
	f.policies[a.Name] = p
	return nil
}

func (f *egressFakeControl) GetActorEgressPolicy(context.Context, substrate.ActorRef) (substrate.EgressPolicyInfo, error) {
	return substrate.EgressPolicyInfo{}, errors.New("not used")
}

func (f *egressFakeControl) UpdateActorEgressPolicy(context.Context, substrate.ActorRef, substrate.EgressPolicy, substrate.EgressPolicyInfo) (substrate.EgressPolicyInfo, error) {
	return substrate.EgressPolicyInfo{}, errors.New("not used")
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
