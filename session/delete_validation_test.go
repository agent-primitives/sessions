package session_test

import (
	"path/filepath"
	"reflect"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	v1 "github.com/agent-primitives/sessions/api/genpb"
)

// Deleted admission beats route/CAS/recovery faults on valid requests, while existing
// required-UID, unsupported-identity and unknown-session validation order stays intact.
func TestDeletionPreservesExistingValidationPrecedence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.db")
	store := openStore(t, path)
	db := deletionSQL(t, path)
	registry, backend := deletionRegistry(t)
	client := serveRegistry(t, store, registry)
	uid := mustCreate(t, client)
	stale := int64(99)
	stream, err := client.Exec(t.Context(), &v1.ExecRequest{Session: uid, ExpectedLastSeq: &stale})
	if err == nil {
		err = drainExec(stream)
	}
	if status.Code(err) != codes.Aborted {
		t.Fatalf("active stale CAS=%v, want Aborted", err)
	}
	if _, err := client.DeleteSession(t.Context(), &v1.DeleteSessionRequest{Uid: uid}); err != nil {
		t.Fatal(err)
	}
	backend.reset()
	before := sessionDeleteRaw(t, db)
	stream, err = client.Exec(t.Context(), &v1.ExecRequest{Session: uid, Harness: "missing", ExpectedLastSeq: &stale})
	if err == nil {
		err = drainExec(stream)
	}
	if status.Code(err) != codes.FailedPrecondition {
		t.Errorf("deleted route+CAS=%v", err)
	}
	for _, target := range []string{"", "unknown", uid} {
		if _, err := client.Fork(t.Context(), &v1.ForkRequest{Session: target, Identity: &v1.IdentityRef{}}); status.Code(err) != codes.InvalidArgument {
			t.Errorf("unsupported identity at %q=%v", target, err)
		}
	}
	for _, op := range []string{"Suspend", "Resume", "Fork"} {
		for _, tc := range []struct {
			uid  string
			code codes.Code
		}{{"", codes.InvalidArgument}, {"unknown", codes.NotFound}} {
			if err := deleteRaceMutation(t.Context(), client, op, tc.uid, ""); status.Code(err) != tc.code {
				t.Errorf("%s(%q)=%v, want %v", op, tc.uid, err, tc.code)
			}
		}
	}
	stream, err = client.Exec(t.Context(), &v1.ExecRequest{Session: "unknown", Harness: "missing"})
	if err == nil {
		err = drainExec(stream)
	}
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("unknown session+override validation=%v", err)
	}
	if len(backend.effects()) != 0 || !reflect.DeepEqual(before, sessionDeleteRaw(t, db)) {
		t.Errorf("validation/refusal side effects: %v", backend.effects())
	}
	// Exec's empty UID is deliberately auto-create, not Delete's required UID validation.
	if err := deletionExec(t, client, "", ""); err != nil {
		t.Fatalf("active auto-create changed: %v", err)
	}
}
