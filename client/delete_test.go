package client_test

import (
	"context"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	v1 "github.com/agent-primitives/sessions/api/genpb"
	"github.com/agent-primitives/sessions/client"
)

// A missing or misdirected SDK Delete leaves the requested UID writable; normalizing its response
// loses the server's exact intent timestamp, cursor or independent compute/execution axes.
func TestDeleteSessionPendingAuditAndRefusal(t *testing.T) {
	for _, shape := range []string{"empty", "running", "suspended"} {
		t.Run(shape, func(t *testing.T) {
			c := newClient(t)
			deleter, ok := any(c).(interface {
				DeleteSession(context.Context, string) (*v1.Session, error)
			})
			if !ok {
				t.Fatal("SDK cannot request durable session deletion")
			}
			ctx := t.Context()
			active, err := c.CreateSession(ctx, &v1.Session{Metadata: &v1.ResourceMetadata{Project: "delete", Name: "audit"}, Labels: map[string]string{"keep": "yes"}})
			if err != nil {
				t.Fatal(err)
			}
			uid := active.GetMetadata().GetUid()
			other, err := c.CreateSession(ctx, &v1.Session{Metadata: &v1.ResourceMetadata{Project: "delete"}})
			if err != nil {
				t.Fatal(err)
			}
			if shape != "empty" {
				if _, err := c.Exec(ctx, client.ExecOptions{Session: uid, Inputs: []string{"before"}}); err != nil {
					t.Fatal(err)
				}
			}
			if shape == "suspended" {
				if _, err := c.Suspend(ctx, uid); err != nil {
					t.Fatal(err)
				}
			}
			before, err := c.GetSession(ctx, uid)
			if err != nil {
				t.Fatal(err)
			}
			prefix, err := c.Replay(ctx, uid, 1, 0)
			if err != nil {
				t.Fatal(err)
			}
			start := time.Now()
			deleted, err := deleter.DeleteSession(ctx, uid)
			end := time.Now()
			if err != nil {
				t.Fatal(err)
			}
			if deleted.GetMetadata().GetUid() != uid || deleted.GetDeletionState() != v1.DeletionState_DELETION_PENDING || deleted.GetDeletionRequestedTime() == nil {
				t.Fatalf("Delete lost UID or intent-only state/times: %v", deleted)
			}
			requested := deleted.GetDeletionRequestedTime().AsTime()
			if requested.Before(start) || requested.After(end) {
				t.Fatalf("request timestamp %v outside call", requested)
			}
			if deleted.GetLastSeq() != before.GetLastSeq()+1 || deleted.GetComputeState() != before.GetComputeState() || deleted.GetExecState() != before.GetExecState() {
				t.Fatalf("Delete changed independent axes/cursor: %v", deleted)
			}
			for _, call := range []struct {
				name string
				run  func() (*v1.Session, error)
			}{
				{"retry", func() (*v1.Session, error) { return deleter.DeleteSession(ctx, uid) }},
				{"get", func() (*v1.Session, error) { return c.GetSession(ctx, uid) }},
				{"raw stub", func() (*v1.Session, error) { return c.Sessions().GetSession(ctx, &v1.GetSessionRequest{Uid: uid}) }},
			} {
				got, err := call.run()
				if err != nil || !proto.Equal(got, deleted) {
					t.Fatalf("%s changed exact returned Session/timestamps: %v %v", call.name, got, err)
				}
			}
			records, err := c.Replay(ctx, uid, 1, 0)
			if err != nil {
				t.Fatal(err)
			}
			if len(records) != len(prefix)+1 {
				t.Fatalf("audit length %d, prefix %d", len(records), len(prefix))
			}
			for i, r := range prefix {
				if !proto.Equal(r, records[i]) {
					t.Fatal("Delete changed audit prefix")
				}
			}
			deletes := 0
			for _, r := range records {
				if r.GetEvent().GetLifecycle().GetKind() == v1.Lifecycle_LIFECYCLE_DELETE {
					deletes++
					if !proto.Equal(r.GetEvent().GetTs(), deleted.GetDeletionRequestedTime()) || r.GetEvent().GetExecutionId() != "" {
						t.Fatalf("DELETE timestamp/scope: %v", r)
					}
				}
			}
			if deletes != 1 {
				t.Fatalf("DELETE count %d", deletes)
			}
			listed, err := c.ListSessions(ctx, "delete")
			if err != nil || len(listed) != 1 || listed[0].GetMetadata().GetUid() != other.GetMetadata().GetUid() {
				t.Fatalf("hidden list/UID forwarding: %v %v", listed, err)
			}
			for _, call := range []struct {
				name string
				run  func() error
			}{
				{"exec", func() error {
					_, err := c.Exec(ctx, client.ExecOptions{Session: uid, Inputs: []string{"refuse"}})
					return err
				}},
				{"fork historical", func() error { _, err := c.Fork(ctx, uid, client.ForkOptions{AtSeq: 1}); return err }},
				{"resume", func() error { _, err := c.Resume(ctx, uid, false); return err }},
				{"suspend", func() error { _, err := c.Suspend(ctx, uid); return err }},
			} {
				if err := call.run(); status.Code(err) != codes.FailedPrecondition {
					t.Fatalf("%s: want FailedPrecondition, got %v", call.name, err)
				}
			}
			got, err := c.GetSession(ctx, uid)
			if err != nil || !proto.Equal(got, deleted) {
				t.Fatalf("refusals changed tombstone: %v %v", got, err)
			}
		})
	}
}

func TestDeleteSessionPreservesServerErrors(t *testing.T) {
	c := newClient(t)
	deleter, ok := any(c).(interface {
		DeleteSession(context.Context, string) (*v1.Session, error)
	})
	if !ok {
		t.Fatal("SDK cannot request durable session deletion")
	}
	for _, tc := range []struct {
		uid  string
		code codes.Code
	}{{"", codes.InvalidArgument}, {"missing-delete", codes.NotFound}} {
		got, err := deleter.DeleteSession(t.Context(), tc.uid)
		_, rawErr := c.Sessions().DeleteSession(t.Context(), &v1.DeleteSessionRequest{Uid: tc.uid})
		if got != nil || status.Code(err) != tc.code || !proto.Equal(status.Convert(err).Proto(), status.Convert(rawErr).Proto()) {
			t.Fatalf("uid %q: response %v error %v, raw %v", tc.uid, got, err, rawErr)
		}
	}
	if _, err := c.GetSession(t.Context(), "missing-delete"); status.Code(err) != codes.NotFound {
		t.Fatalf("unknown Delete created row: %v", err)
	}
}
