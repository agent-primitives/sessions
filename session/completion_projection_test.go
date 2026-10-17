package session_test

import (
	"path/filepath"
	"testing"

	v1 "github.com/agent-primitives/sessions/api/genpb"
)

func TestCompletionTimeIsWithheldUntilComplete(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.db")
	store := openStore(t, path)
	registry, _ := deletionRegistry(t)
	client := serveRegistry(t, store, registry)
	uid := mustCreate(t, client)
	// v4 reserved this column without defining its meaning. A value alone is not cleanup proof.
	if _, err := deletionSQL(t, path).Exec(`UPDATE sessions SET deletion_completed_at=123 WHERE session=?`, uid); err != nil {
		t.Fatal(err)
	}
	active, err := client.GetSession(t.Context(), &v1.GetSessionRequest{Uid: uid})
	if err != nil {
		t.Fatal(err)
	}
	if active.GetDeletionCompletedTime() != nil {
		t.Fatalf("active timestamp claimed completion: %v", active)
	}
	pending, err := client.DeleteSession(t.Context(), &v1.DeleteSessionRequest{Uid: uid})
	if err != nil {
		t.Fatal(err)
	}
	if pending.GetDeletionCompletedTime() != nil {
		t.Fatalf("pending timestamp claimed completion: %v", pending)
	}
}
