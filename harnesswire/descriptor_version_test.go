package harnesswire

import (
	"reflect"
	"testing"

	"github.com/agent-primitives/sessions/api"
	v1 "github.com/agent-primitives/sessions/api/genpb"
)

func TestDescriptorRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		name, version string
		class         api.HarnessClass
	}{
		{name: "legacy defaults"},
		{name: "mediated", version: "replay-v1", class: api.HarnessClassMediated},
		{name: "unmediated", version: "replay-v1", class: api.HarnessClassUnmediated},
	} {
		t.Run(tc.name, func(t *testing.T) {
			desc := api.Descriptor{ID: "implementation", Version: tc.version, Models: []string{"model"}, Tools: []api.ToolSpec{{Name: "tool", Mediation: api.MediationControllerMediated}}, Capabilities: api.Capabilities{Resumability: api.ResumabilityStatelessReplay}, HarnessClass: tc.class}
			encoded := DescriptorToProto(desc)
			if encoded.GetVersion() != tc.version {
				t.Fatalf("wire version = %q, want %q", encoded.GetVersion(), tc.version)
			}
			if got := descriptorFromProto(encoded); !reflect.DeepEqual(got, desc) {
				t.Fatalf("round-trip = %#v, want %#v", got, desc)
			}
		})
	}
}

func TestUnknownHarnessClassDoesNotBecomeLegacyMediated(t *testing.T) {
	decoded := descriptorFromProto(&v1.HarnessDescriptor{HarnessClass: v1.HarnessClass(99)})
	if decoded.HarnessClass == "" || decoded.HarnessClass == api.HarnessClassMediated {
		t.Fatalf("unknown wire class decoded as legacy-mediated: %q", decoded.HarnessClass)
	}
	encoded := DescriptorToProto(api.Descriptor{HarnessClass: api.HarnessClass("FUTURE_CLASS")})
	if encoded.GetHarnessClass() == v1.HarnessClass_HARNESS_CLASS_UNSPECIFIED || encoded.GetHarnessClass() == v1.HarnessClass_HARNESS_CLASS_MEDIATED {
		t.Fatalf("unknown Go class encoded as legacy-mediated: %v", encoded.GetHarnessClass())
	}
}
