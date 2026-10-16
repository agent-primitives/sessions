package sqlitelog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/agent-primitives/sessions/api"
	"github.com/agent-primitives/sessions/eventlog"
)

// DeleteSession commits permanent deletion intent, not runtime cleanup. The retained journal
// anchors audit evidence; its predecessor remains the pre-delete head, independent of compute.
// Retries of either terminal deletion state return the original snapshot without any writes.
func (s *Store) DeleteSession(ctx context.Context, uid string) (SessionInfo, error) {
	if uid == "" {
		return SessionInfo{}, errors.New("sqlitelog: delete session requires a uid")
	}
	fail := func(phase string, err error) (SessionInfo, error) {
		// A canceled transaction may surface ErrTxDone or a driver interrupt instead of ctx.Err.
		// Retain both causes so callers can classify cancellation without losing the SQL fault.
		if cause := ctx.Err(); cause != nil && !errors.Is(err, cause) {
			err = errors.Join(err, cause)
		}
		return SessionInfo{}, fmt.Errorf("sqlitelog: delete session %q: %s: %w", uid, phase, err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fail("begin", err)
	}
	defer func() { _ = tx.Rollback() }()
	info, err := txSessionInfo(ctx, tx, uid)
	if err != nil {
		return fail("read", err)
	}
	switch info.DeletionState {
	case api.DeletionPending, api.DeletionComplete:
		return info, nil
	case api.DeletionActive:
	default:
		return fail("state", eventlog.ErrDeleted)
	}
	var fence int64
	if err := tx.QueryRowContext(ctx, `SELECT fence FROM sessions WHERE session = ?`, uid).Scan(&fence); err != nil {
		return fail("read fence", err)
	}
	if err := ctx.Err(); err != nil {
		return fail("before append", err)
	}
	now := time.Now()
	record, _, err := appendRecord(ctx, tx, uid, info.LastSeq, fence+1, api.Event{
		Kind: api.EventLifecycle, Lifecycle: &api.Lifecycle{Kind: api.LifecycleDelete},
		Timestamp: now, SchemaVersion: 1,
	})
	if err != nil {
		return fail("append intent", err)
	}
	// Like projectApproval, DELETE must not restore reporting over a gap left by an
	// older writer. Keep the cursor and suppression in this context-bound metadata write.
	if _, err := tx.ExecContext(ctx, `UPDATE sessions SET deletion_state = ?, deletion_requested_at = ?,
		deletion_seq = ?, approval_corrupt = CASE WHEN last_seq != ? THEN 1 ELSE approval_corrupt END,
		last_seq = ?, fence = ?, updated_at = ? WHERE session = ?`,
		string(api.DeletionPending), now.UnixNano(), record.Seq, info.LastSeq, record.Seq, record.Fence, now.UnixNano(), uid); err != nil {
		return fail("update intent", err)
	}
	out, err := txSessionInfo(ctx, tx, uid)
	if err != nil {
		return fail("read intent", err)
	}
	if err := tx.Commit(); err != nil {
		return fail("commit intent", err)
	}
	return out, nil
}

func txSessionInfo(ctx context.Context, tx *sql.Tx, uid string) (SessionInfo, error) {
	info, err := scanSession(tx.QueryRowContext(ctx, sessionSelect+` WHERE s.session = ?`, uid))
	if errors.Is(err, sql.ErrNoRows) {
		return SessionInfo{}, fmt.Errorf("%w: %s", ErrSessionNotFound, uid)
	}
	return info, err
}

// CheckWritable is a read-only preflight. Unknown logs stay writable for direct implicit creation.
func (l *Log) CheckWritable() error { return checkWritable(l.store.db, l.session) }

func checkWritable(q queryRower, uid string) error {
	var state string
	err := q.QueryRow(`SELECT deletion_state FROM sessions WHERE session = ?`, uid).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("sqlitelog: check writable session %q: %w", uid, err)
	}
	if api.DeletionState(state) != api.DeletionActive {
		return fmt.Errorf("sqlitelog: session %q: %w", uid, eventlog.ErrDeleted)
	}
	return nil
}
