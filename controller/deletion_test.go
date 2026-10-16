package controller_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/agent-primitives/sessions/api"
	"github.com/agent-primitives/sessions/controller"
	"github.com/agent-primitives/sessions/eventlog"
	"github.com/agent-primitives/sessions/sqlitelog"
)

type deletionHarness struct{ describes, runs int }

func (h *deletionHarness) Describe(context.Context) (api.Descriptor, error) {
	h.describes++
	return api.Descriptor{ID: "deletion-test", Capabilities: api.Capabilities{Resumability: api.ResumabilityStatelessReplay}}, nil
}
func (h *deletionHarness) Run(ctx context.Context, _ *api.Start, sink api.EventSink) error {
	h.runs++
	if _, err := sink.Model(ctx, api.ModelRequest{}); err != nil {
		return err
	}
	_, err := sink.ToolCall(ctx, api.ToolCall{ID: "effect", Tool: "write", IdempotencyKey: "once", Mediation: api.MediationControllerMediated})
	return err
}

// Removing the entry preflight would Describe (and Resume would Run/re-drive) a deleted journal.
// These are real SQLite crash cuts, not a writer that only fails at Append.
func TestDeletedControllerEntryHasNoEffects(t *testing.T) {
	for _, cut := range []string{"empty", "no-input", "model-intent", "model-answer", "tool-intent", "tool-answer"} {
		for _, construction := range []string{"retained", "supplied-fence"} {
			for _, operation := range []string{"Exec", "Resume"} {
				t.Run(cut+"/"+construction+"/"+operation, func(t *testing.T) {
					path := filepath.Join(t.TempDir(), "journal.db")
					store, err := sqlitelog.Open(path)
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { _ = store.Close() })
					db, err := sql.Open("sqlite", path)
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { _ = db.Close() })
					log := store.Session("session")
					h := &deletionHarness{}
					models, tools := 0, 0
					model := func(context.Context, api.ModelRequest) (api.ModelResponse, error) {
						models++
						return api.ModelResponse{Message: *api.TextMessage("assistant", "answer")}, nil
					}
					opts := []controller.Option{controller.WithSessionUID("session"), controller.WithToolExecutor(func(context.Context, controller.ToolCallContext, api.ToolCall) (api.ToolResult, error) {
						tools++
						return api.ToolResult{ID: "effect"}, nil
					})}
					c, err := controller.New(log, model, opts...)
					if err != nil {
						t.Fatal(err)
					}
					if cut != "empty" {
						if err := c.Exec(t.Context(), h, nil, 0); err != nil {
							t.Fatal(err)
						}
						records, err := log.Read(1)
						if err != nil {
							t.Fatal(err)
						}
						kind := map[string]api.EventKind{"no-input": api.EventExecutionStart, "model-intent": api.EventModelCall, "model-answer": api.EventOutput, "tool-intent": api.EventToolCall, "tool-answer": api.EventToolResult}[cut]
						var seq int64
						for _, record := range records {
							if record.Event.Kind == kind {
								seq = record.Seq
								break
							}
						}
						if seq == 0 {
							t.Fatalf("missing fixture kind %s", kind)
						}
						// Raw test-only truncation must keep the append-owned cursor at its cut.
						// These histories are nongated; no approval reference survives the cut.
						if _, err := db.Exec("DELETE FROM events WHERE session = ? AND seq > ?", "session", seq); err != nil {
							t.Fatal(err)
						}
						if _, err := db.Exec("UPDATE sessions SET last_seq = ? WHERE session = ?", seq, "session"); err != nil {
							t.Fatal(err)
						}
					}
					if _, err := store.DeleteSession(t.Context(), "session"); err != nil {
						t.Fatal(err)
					}
					if construction == "supplied-fence" {
						c, err = controller.New(log, model, append(opts, controller.WithFence(999))...)
						if err != nil {
							t.Fatal(err)
						}
					}
					h.describes, h.runs, models, tools = 0, 0, 0, 0
					before := deletionRawState(t, db)
					if operation == "Exec" {
						err = c.Exec(t.Context(), h, nil, 0)
					} else {
						var resumed bool
						resumed, err = c.Resume(t.Context(), h)
						if resumed {
							t.Error("deleted Resume reported recovery")
						}
					}
					if !errors.Is(err, eventlog.ErrDeleted) {
						t.Errorf("%s = %v, want exact ErrDeleted", operation, err)
					}
					if h.describes != 0 || h.runs != 0 || models != 0 || tools != 0 {
						t.Errorf("deleted entry effects: Describe=%d Run=%d model=%d tool=%d", h.describes, h.runs, models, tools)
					}
					if !reflect.DeepEqual(before, deletionRawState(t, db)) {
						t.Error("deleted entry changed raw sessions/fences/timestamps/events")
					}
					if err := log.Verify(); err != nil {
						t.Fatal(err)
					}
				})
			}
		}
	}
}

func deletionRawState(t *testing.T, db *sql.DB) [][]any {
	t.Helper()
	var out [][]any
	for _, query := range []string{"SELECT * FROM sessions ORDER BY session", "SELECT * FROM events ORDER BY session,seq"} {
		rows, err := db.Query(query)
		if err != nil {
			t.Fatal(err)
		}
		cols, err := rows.Columns()
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			values := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range values {
				ptrs[i] = &values[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				t.Fatal(err)
			}
			out = append(out, values)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		if err := rows.Close(); err != nil {
			t.Fatal(err)
		}
	}
	return out
}

// Parent admission must precede reading/copying; target admission must precede NewFence.
func TestControllerForkRefusesDeletedSourceAndTarget(t *testing.T) {
	for _, deleted := range []string{"parent", "child"} {
		t.Run(deleted, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "journal.db")
			store, err := sqlitelog.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			db, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = db.Close() })
			for _, uid := range []string{"parent", "child"} {
				if err := store.PutSession(sqlitelog.SessionMeta{UID: uid}); err != nil {
					t.Fatal(err)
				}
			}
			parent, child := store.Session("parent"), store.Session("child")
			c, err := controller.New(parent, func(context.Context, api.ModelRequest) (api.ModelResponse, error) { return api.ModelResponse{}, nil }, controller.WithSessionUID("parent"), controller.WithToolExecutor(func(context.Context, controller.ToolCallContext, api.ToolCall) (api.ToolResult, error) {
				return api.ToolResult{ID: "effect"}, nil
			}))
			if err != nil {
				t.Fatal(err)
			}
			if err := c.Exec(t.Context(), &deletionHarness{}, nil, 0); err != nil {
				t.Fatal(err)
			}
			if _, err := store.DeleteSession(t.Context(), deleted); err != nil {
				t.Fatal(err)
			}
			before := deletionRawState(t, db)
			if err := controller.Fork(parent, child, 1); !errors.Is(err, eventlog.ErrDeleted) {
				t.Errorf("Fork=%v, want ErrDeleted", err)
			}
			if !reflect.DeepEqual(before, deletionRawState(t, db)) {
				t.Error("refused Fork changed raw state")
			}
		})
	}
}
