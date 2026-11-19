package api

import (
	"encoding/json"
	"testing"
)

func TestCleanupResourceTargetAndOwnedValidation(t *testing.T) {
	valid := CleanupResource{Kind: CleanupResourceActor, Namespace: "sessions", Name: "session-1"}
	if err := valid.ValidateTarget(); err != nil {
		t.Fatalf("target = %v", err)
	}
	if err := valid.ValidateOwned(); err == nil {
		t.Fatal("UID-empty target was accepted as owned")
	}
	owned := valid
	owned.UID = "actor-uid"
	if err := owned.ValidateOwned(); err != nil {
		t.Fatalf("owned resource = %v", err)
	}

	for _, tc := range []struct {
		name     string
		resource CleanupResource
	}{
		{"unknown kind", CleanupResource{Kind: "actor-ish", Namespace: "sessions", Name: "session-1"}},
		{"missing namespace", CleanupResource{Kind: CleanupResourceActor, Name: "session-1"}},
		{"missing name", CleanupResource{Kind: CleanupResourceActor, Namespace: "sessions"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.resource.ValidateTarget(); err == nil {
				t.Fatalf("invalid target accepted: %+v", tc.resource)
			}
		})
	}
}

func TestCleanupResourceDoesNotCarryIncarnationRoutingData(t *testing.T) {
	resource := CleanupResource{Kind: CleanupResourceActor, Namespace: "sessions", Name: "session-1", UID: "actor-uid"}
	inc := Incarnation{
		ID:              "session-1",
		Worker:          "worker-secret",
		Address:         "private.example:1234",
		CallMetadata:    map[string]string{"authorization": "secret"},
		CleanupResource: &resource,
	}
	blob, err := json.Marshal(inc.CleanupResource)
	if err != nil {
		t.Fatal(err)
	}
	got := string(blob)
	want := `{"Kind":"actor","Namespace":"sessions","Name":"session-1","UID":"actor-uid"}`
	if got != want {
		t.Fatalf("cleanup resource = %s, want %s", got, want)
	}
}
