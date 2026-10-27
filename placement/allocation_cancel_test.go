package placement

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/agent-primitives/sessions/api"
	"github.com/agent-primitives/sessions/harness/echoagent"
	"github.com/agent-primitives/sessions/sqlitelog"
)

// Cancellation after successful runtime IO must not discard a known allocation handle.
func TestAllocationConfirmationSurvivesClientCancel(t *testing.T) {
	for _, operation := range []string{"create", "restore"} {
		t.Run(operation, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "journal.db")
			s, err := sqlitelog.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { s.Close() })
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var trace []string
			backend := cancelAllocationBackend{allocationBackend: &allocationBackend{trace: &trace}, cancel: cancel}
			p := New(backend, echoagent.Model)
			if operation == "create" {
				_, err = p.Exec(ctx, s.Session("s"), "s", nil, 0)
			} else {
				err = p.Resume(ctx, s.Session("s"), "s")
			}
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("placement ignored client cancellation: %v", err)
			}
			if ctx.Err() != context.Canceled || len(trace) != 1 || trace[0] != operation {
				t.Fatalf("runtime boundary not reached: ctx=%v trace=%v", ctx.Err(), trace)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			s, err = sqlitelog.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			rows, err := s.Allocations(t.Context(), "s")
			if err != nil || len(rows) != 1 {
				t.Fatalf("reopened allocations = %+v, %v", rows, err)
			}
			if !rows[0].Confirmed || rows[0].ResourceID != "resource-s" || rows[0].Operation != operation {
				t.Fatalf("successful %s lost its handle after cancellation: %+v", operation, rows[0])
			}
		})
	}
}

type cancelAllocationBackend struct {
	*allocationBackend
	cancel context.CancelFunc
}

func (b cancelAllocationBackend) Create(ctx context.Context, spec *api.SessionSpec) (api.Incarnation, error) {
	in, err := b.allocationBackend.Create(ctx, spec)
	b.cancel()
	return in, err
}

func (b cancelAllocationBackend) Restore(ctx context.Context, ref api.SnapshotRef) (api.Incarnation, error) {
	in, err := b.allocationBackend.Restore(ctx, ref)
	b.cancel()
	return in, err
}
