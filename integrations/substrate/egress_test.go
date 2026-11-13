package ateadapter

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/agent-primitives/sessions/runtime/substrate"
	atepb "go.ate.dev/substrate/api/ateapi/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// egressCtl is an ate-api Control double for the EgressPolicy RPCs. It records the requests and
// answers with stored, or fails with err.
type egressCtl struct {
	atepb.ControlClient
	calls  []string
	create *atepb.CreateActorEgressPolicyRequest
	update *atepb.UpdateActorEgressPolicyRequest
	stored *atepb.EgressPolicy
	err    error
}

func (c *egressCtl) GetActorEgressPolicy(_ context.Context, in *atepb.GetActorEgressPolicyRequest, _ ...grpc.CallOption) (*atepb.EgressPolicy, error) {
	c.calls = append(c.calls, "get:"+in.GetActor().GetAtespace()+"/"+in.GetActor().GetName()+"/"+in.GetName())
	return c.stored, c.err
}

func (c *egressCtl) CreateActorEgressPolicy(_ context.Context, in *atepb.CreateActorEgressPolicyRequest, _ ...grpc.CallOption) (*atepb.EgressPolicy, error) {
	c.calls = append(c.calls, "create:"+in.GetActor().GetAtespace()+"/"+in.GetActor().GetName())
	c.create = in
	return c.stored, c.err
}

func (c *egressCtl) UpdateActorEgressPolicy(_ context.Context, in *atepb.UpdateActorEgressPolicyRequest, _ ...grpc.CallOption) (*atepb.EgressPolicy, error) {
	c.calls = append(c.calls, "update:"+in.GetActor().GetAtespace()+"/"+in.GetActor().GetName())
	c.update = in
	return c.stored, c.err
}

func (c *egressCtl) DeleteActorEgressPolicy(_ context.Context, in *atepb.DeleteActorEgressPolicyRequest, _ ...grpc.CallOption) (*atepb.EgressPolicy, error) {
	c.calls = append(c.calls, "delete:"+in.GetActor().GetAtespace()+"/"+in.GetActor().GetName()+"/"+in.GetName())
	return c.stored, c.err
}

var egressActor = substrate.ActorRef{Atespace: "space", Name: "sess-x"}

// fullPolicy uses every protocol, both port forms and both credential sources.
func fullPolicy() substrate.EgressPolicy {
	return substrate.EgressPolicy{Rules: []substrate.EgressRule{
		{Protocol: substrate.EgressHTTP, Hostnames: []string{"plain.example.com"}},
		{Protocol: substrate.EgressHTTPS, Hostnames: []string{"api.example.com", "*.example.org"}, Ports: []int32{443, 8443},
			ReplaceHeaders: []substrate.CredentialHeader{
				{Header: "Authorization", Prefix: "Bearer ", CredentialURI: "ate-secret://kv/vault/model-key"},
				{Header: "X-Actor-Token", ActorJWT: &substrate.ActorJWTSource{Audiences: []string{"https://aud.example.com"}, ExpirationSeconds: 600}},
			}},
		{Protocol: substrate.EgressTLSPassthrough, Hostnames: []string{"db.example.com"}, AllPorts: true},
	}}
}

func fullPolicyProto() *atepb.EgressPolicy {
	return &atepb.EgressPolicy{
		Metadata: &atepb.ResourceMetadata{Atespace: "space", Name: "default"},
		Rules: []*atepb.EgressRule{
			{Http: &atepb.HttpRule{Hostnames: []string{"plain.example.com"}}},
			{Https: &atepb.HttpsRule{Hostnames: []string{"api.example.com", "*.example.org"}, Ports: &atepb.Ports{Numbers: []int32{443, 8443}},
				Effects: &atepb.HttpRuleEffects{ReplaceHeaders: []*atepb.CredentialHeader{
					{Header: "Authorization", Prefix: "Bearer ", CredentialUri: "ate-secret://kv/vault/model-key"},
					{Header: "X-Actor-Token", ActorJwt: &atepb.ActorJwtSource{Audiences: []string{"https://aud.example.com"}, ExpirationSeconds: 600}},
				}}}},
			{TlsPassthrough: &atepb.TlsPassthroughRule{Hostnames: []string{"db.example.com"}, Ports: &atepb.Ports{All: &atepb.AllPorts{}}}},
		},
	}
}

// Create sends the actor's singleton "default" policy with every rule, port form and credential
// source, and Get converts the stored resource back to the same neutral policy.
func TestEgressPolicyRoundTrip(t *testing.T) {
	ctl := &egressCtl{}
	ad := FromClient(ctl)
	if err := ad.CreateActorEgressPolicy(context.Background(), egressActor, fullPolicy()); err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(ctl.create.GetEgressPolicy(), fullPolicyProto()) {
		t.Fatalf("create sent %v\nwant %v", ctl.create.GetEgressPolicy(), fullPolicyProto())
	}
	if got := ctl.create.GetActor(); got.GetAtespace() != "space" || got.GetName() != "sess-x" {
		t.Fatalf("create targets %v, want space/sess-x", got)
	}

	stored := fullPolicyProto()
	stored.Metadata.Uid, stored.Metadata.Version = "policy-uid", 3
	ctl.stored = stored
	info, err := ad.GetActorEgressPolicy(context.Background(), egressActor)
	if err != nil {
		t.Fatal(err)
	}
	want := substrate.EgressPolicyInfo{Policy: fullPolicy(), UID: "policy-uid", Version: 3}
	if !reflect.DeepEqual(info, want) {
		t.Fatalf("get=%+v\nwant %+v", info, want)
	}
	if !slices.Contains(ctl.calls, "get:space/sess-x/default") {
		t.Fatalf("calls=%v, want the default policy read", ctl.calls)
	}
}

// Update sends the precondition read from Get; Delete names the singleton.
func TestEgressPolicyUpdateAndDelete(t *testing.T) {
	stored := fullPolicyProto()
	stored.Metadata.Uid, stored.Metadata.Version = "policy-uid", 4
	ctl := &egressCtl{stored: stored}
	ad := FromClient(ctl)
	info, err := ad.UpdateActorEgressPolicy(context.Background(), egressActor, fullPolicy(), substrate.EgressPolicyInfo{UID: "policy-uid", Version: 3})
	if err != nil {
		t.Fatal(err)
	}
	if m := ctl.update.GetEgressPolicy().GetMetadata(); m.GetUid() != "policy-uid" || m.GetVersion() != 3 || m.GetName() != "default" || m.GetAtespace() != "space" {
		t.Fatalf("update metadata=%v, want the precondition on space/default", m)
	}
	if info.Version != 4 {
		t.Fatalf("update returned %+v, want the stored version", info)
	}
	if err := ad.DeleteActorEgressPolicy(context.Background(), egressActor); err != nil {
		t.Fatal(err)
	}
	if ctl.calls[len(ctl.calls)-1] != "delete:space/sess-x/default" {
		t.Fatalf("calls=%v, want the default policy deleted", ctl.calls)
	}
}

// Each RPC maps substrate's codes onto the backend's sentinels and keeps the status error.
func TestEgressPolicyErrorMapping(t *testing.T) {
	calls := map[string]func(*Adapter) error{
		"get": func(ad *Adapter) error {
			_, err := ad.GetActorEgressPolicy(context.Background(), egressActor)
			return err
		},
		"create": func(ad *Adapter) error {
			return ad.CreateActorEgressPolicy(context.Background(), egressActor, fullPolicy())
		},
		"update": func(ad *Adapter) error {
			_, err := ad.UpdateActorEgressPolicy(context.Background(), egressActor, fullPolicy(), substrate.EgressPolicyInfo{UID: "u", Version: 1})
			return err
		},
		"delete": func(ad *Adapter) error { return ad.DeleteActorEgressPolicy(context.Background(), egressActor) },
	}
	for _, tc := range []struct {
		code codes.Code
		want error // nil: no sentinel
		// writes, when set, is the sentinel for the calls that carry a policy (create, update).
		writes error
	}{
		{code: codes.AlreadyExists, want: substrate.ErrEgressPolicyExists},
		{code: codes.NotFound, want: substrate.ErrEgressPolicyNotFound},
		{code: codes.FailedPrecondition, want: substrate.ErrActorNotFound},
		{code: codes.Aborted, want: substrate.ErrActorBusy},
		{code: codes.PermissionDenied},
		{code: codes.InvalidArgument, writes: substrate.ErrEgressPolicyInvalid},
	} {
		for name, call := range calls {
			t.Run(name+"/"+tc.code.String(), func(t *testing.T) {
				err := call(FromClient(&egressCtl{err: status.Error(tc.code, "refused")}))
				if status.Code(err) != tc.code {
					t.Fatalf("err=%v lost its status code %v", err, tc.code)
				}
				want := tc.want
				if tc.writes != nil && (name == "create" || name == "update") {
					want = tc.writes
				}
				for _, s := range []error{substrate.ErrEgressPolicyExists, substrate.ErrEgressPolicyNotFound, substrate.ErrActorNotFound, substrate.ErrActorBusy, substrate.ErrEgressPolicyInvalid} {
					if errors.Is(err, s) != (s == want) {
						t.Fatalf("err=%v: errors.Is(%v)=%v, want %v", err, s, errors.Is(err, s), s == want)
					}
				}
			})
		}
	}
}

// A policy substrate's message cannot express is refused before any RPC as
// substrate.ErrEgressPolicyInvalid, and the error names the rule by index only.
func TestEgressPolicyToProtoRefusals(t *testing.T) {
	jwt := &substrate.ActorJWTSource{Audiences: []string{"aud"}, ExpirationSeconds: 600}
	for name, rule := range map[string]substrate.EgressRule{
		"unspecified protocol":       {Hostnames: []string{"secret-host.example.com"}},
		"all ports and port numbers": {Protocol: substrate.EgressHTTPS, Hostnames: []string{"secret-host.example.com"}, AllPorts: true, Ports: []int32{443}},
		"headers on tls_passthrough": {Protocol: substrate.EgressTLSPassthrough, Hostnames: []string{"secret-host.example.com"}, AllPorts: true,
			ReplaceHeaders: []substrate.CredentialHeader{{Header: "Authorization", CredentialURI: "ate-secret://kv/vault/key"}}},
		"header with both sources": {Protocol: substrate.EgressHTTPS, Hostnames: []string{"secret-host.example.com"},
			ReplaceHeaders: []substrate.CredentialHeader{{Header: "Authorization", CredentialURI: "ate-secret://kv/vault/key", ActorJWT: jwt}}},
		"header with no source": {Protocol: substrate.EgressHTTPS, Hostnames: []string{"secret-host.example.com"},
			ReplaceHeaders: []substrate.CredentialHeader{{Header: "Authorization"}}},
	} {
		t.Run(name, func(t *testing.T) {
			ctl := &egressCtl{}
			err := FromClient(ctl).CreateActorEgressPolicy(context.Background(), egressActor, substrate.EgressPolicy{Rules: []substrate.EgressRule{rule}})
			if !errors.Is(err, errEgressRule) || !errors.Is(err, substrate.ErrEgressPolicyInvalid) {
				t.Fatalf("err=%v want errEgressRule and substrate.ErrEgressPolicyInvalid", err)
			}
			if len(ctl.calls) != 0 {
				t.Fatalf("calls=%v, want none for a refused policy", ctl.calls)
			}
			if strings.Contains(err.Error(), "secret-host") || strings.Contains(err.Error(), "ate-secret") {
				t.Fatalf("error carries policy contents: %v", err)
			}
			_, err = FromClient(ctl).UpdateActorEgressPolicy(context.Background(), egressActor, substrate.EgressPolicy{Rules: []substrate.EgressRule{rule}}, substrate.EgressPolicyInfo{UID: "u", Version: 1})
			if !errors.Is(err, errEgressRule) || !errors.Is(err, substrate.ErrEgressPolicyInvalid) || len(ctl.calls) != 0 {
				t.Fatalf("update: err=%v calls=%v, want substrate.ErrEgressPolicyInvalid and no RPC", err, ctl.calls)
			}
		})
	}
}

// A stored or file policy whose rule sets no protocol or several, both port forms, or a header
// with both or neither credential source is refused rather than half converted.
func TestEgressPolicyFromProtoRefusals(t *testing.T) {
	for name, rule := range map[string]*atepb.EgressRule{
		"no protocol":    {},
		"two protocols":  {Http: &atepb.HttpRule{Hostnames: []string{"a.example.com"}}, Https: &atepb.HttpsRule{Hostnames: []string{"a.example.com"}}},
		"both port sets": {Https: &atepb.HttpsRule{Hostnames: []string{"a.example.com"}, Ports: &atepb.Ports{All: &atepb.AllPorts{}, Numbers: []int32{443}}}},
		"header with both sources": {Https: &atepb.HttpsRule{Hostnames: []string{"a.example.com"}, Effects: &atepb.HttpRuleEffects{ReplaceHeaders: []*atepb.CredentialHeader{
			{Header: "Authorization", CredentialUri: "ate-secret://kv/vault/key", ActorJwt: &atepb.ActorJwtSource{Audiences: []string{"aud"}, ExpirationSeconds: 600}}}}}},
		"header with no source": {Http: &atepb.HttpRule{Hostnames: []string{"a.example.com"}, Effects: &atepb.HttpRuleEffects{ReplaceHeaders: []*atepb.CredentialHeader{
			{Header: "Authorization"}}}}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := EgressPolicyFromProto(&atepb.EgressPolicy{Rules: []*atepb.EgressRule{rule}}); !errors.Is(err, errEgressRule) {
				t.Fatalf("err=%v want errEgressRule", err)
			}
		})
	}
}

// The conversion handles exactly these fields. A Substrate bump that adds a field to any message
// the policy is built from fails here, so the field is converted (or knowingly dropped) instead of
// silently lost.
func TestEgressPolicyConversionCoversEveryField(t *testing.T) {
	handled := map[protoreflect.Name][]string{
		"EgressPolicy":       {"metadata", "rules"},
		"EgressRule":         {"http", "https", "tls_passthrough"},
		"HttpRule":           {"hostnames", "ports", "effects"},
		"HttpsRule":          {"hostnames", "ports", "effects"},
		"TlsPassthroughRule": {"hostnames", "ports"},
		"Ports":              {"all", "numbers"},
		"AllPorts":           {},
		"HttpRuleEffects":    {"replace_headers"},
		"CredentialHeader":   {"header", "prefix", "credential_uri", "actor_jwt"},
		"ActorJwtSource":     {"audiences", "expiration_seconds"},
	}
	seen := map[protoreflect.Name]bool{}
	var walk func(md protoreflect.MessageDescriptor)
	walk = func(md protoreflect.MessageDescriptor) {
		if seen[md.Name()] {
			return
		}
		seen[md.Name()] = true
		want, ok := handled[md.Name()]
		if !ok {
			t.Errorf("message %s is reachable from EgressPolicy but the conversion does not know it", md.FullName())
			return
		}
		var got []string
		fields := md.Fields()
		for i := range fields.Len() {
			f := fields.Get(i)
			got = append(got, string(f.Name()))
			// Metadata is set from the actor ref, not converted.
			if f.Message() != nil && f.Name() != "metadata" {
				walk(f.Message())
			}
		}
		if !slices.Equal(got, want) {
			t.Errorf("%s fields=%v, the conversion handles %v", md.FullName(), got, want)
		}
	}
	walk((&atepb.EgressPolicy{}).ProtoReflect().Descriptor())
	for name := range handled {
		if !seen[name] {
			t.Errorf("%s is no longer reachable from EgressPolicy", name)
		}
	}
}
