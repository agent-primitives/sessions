package sqlitelog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/agent-primitives/sessions/api"
	"github.com/agent-primitives/sessions/eventlog"
)

// ErrForkState means a batch is unknown, its transition is forbidden, or its children are not
// fresh, fully copied journals with valid fork metadata. Unknown future states fail closed.
var ErrForkState = errors.New("sqlitelog: invalid fork state")

// BeginFork reserves all child UIDs and metadata in one transaction. A batch's publication status,
// rather than per-child updates, makes its children visible together. Aborted UIDs are never reused.
func (s *Store) BeginFork(ctx context.Context, parent string, children []SessionMeta) (string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	info, err := txSessionInfo(ctx, tx, parent)
	if err != nil {
		return "", err
	}
	if err := checkAdmissible(tx, parent); err != nil {
		return "", err
	}
	if len(children) == 0 {
		return "", ErrForkState
	}
	seen := make(map[string]bool, len(children))
	for _, child := range children {
		if child.UID == "" || child.UID == parent || seen[child.UID] || child.ParentUID != parent || child.ForkSeq < 0 || child.ForkSeq > info.LastSeq {
			return "", ErrForkState
		}
		seen[child.UID] = true
		var exists bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sessions WHERE session=?) OR EXISTS(SELECT 1 FROM events WHERE session=?)`, child.UID, child.UID).Scan(&exists); err != nil {
			return "", err
		}
		if exists {
			return "", fmt.Errorf("%w: child %q already exists", ErrForkState, child.UID)
		}
	}
	batch, err := newLedgerID()
	if err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO fork_batches(id,parent,status) VALUES(?,?,'PREPARING')`, batch, parent); err != nil {
		return "", err
	}
	for _, child := range children {
		if err := s.putSession(tx, child); err != nil {
			return "", err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE sessions SET fork_batch=? WHERE session=?`, batch, child.UID); err != nil {
			return "", err
		}
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return batch, nil
}

func forkBatch(ctx context.Context, tx *sql.Tx, batch string) (parent, status string, err error) {
	err = tx.QueryRowContext(ctx, `SELECT parent,status FROM fork_batches WHERE id=?`, batch).Scan(&parent, &status)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrForkState
	}
	return
}

type forkChild struct {
	uid, parent string
	seq         int64
}

func forkBatchChildren(ctx context.Context, tx *sql.Tx, batch string) ([]forkChild, error) {
	rows, err := tx.QueryContext(ctx, `SELECT session,parent_uid,fork_seq FROM sessions WHERE fork_batch=? ORDER BY session`, batch)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var children []forkChild
	for rows.Next() {
		var child forkChild
		if err := rows.Scan(&child.uid, &child.parent, &child.seq); err != nil {
			return nil, err
		}
		children = append(children, child)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(children) == 0 {
		return nil, ErrForkState
	}
	return children, nil
}

// CommitFork validates complete child copies and publishes the entire batch in a single write.
// A retry of COMMITTED succeeds without touching evidence, including after a lost response.
func (s *Store) CommitFork(ctx context.Context, batch string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	parent, status, err := forkBatch(ctx, tx, batch)
	if err != nil {
		return err
	}
	if status == "COMMITTED" {
		return nil
	}
	if status != "PREPARING" {
		return ErrForkState
	}
	if err := checkAdmissible(tx, parent); err != nil {
		return err
	}
	children, err := forkBatchChildren(ctx, tx, batch)
	if err != nil {
		return err
	}
	for _, child := range children {
		if child.parent != parent || child.seq < 0 {
			return ErrForkState
		}
		info, err := txSessionInfo(ctx, tx, child.uid)
		if err != nil {
			return err
		}
		if info.DeletionState != api.DeletionActive {
			return eventlog.ErrDeleted
		}
		if info.LastSeq != child.seq+1 {
			return fmt.Errorf("%w: child %q journal incomplete", ErrForkState, child.uid)
		}
		var blob []byte
		var prev string
		if err := tx.QueryRowContext(ctx, `SELECT event,prev_hash FROM events WHERE session=? AND seq=?`, child.uid, info.LastSeq).Scan(&blob, &prev); err != nil {
			return err
		}
		ev, err := decode(blob)
		if err != nil {
			return err
		}
		if ev.Kind != api.EventLifecycle || ev.Lifecycle == nil || ev.Lifecycle.Kind != api.LifecycleFork {
			return fmt.Errorf("%w: child %q missing FORK", ErrForkState, child.uid)
		}
		var parentHash string
		if child.seq > 0 {
			if err := tx.QueryRowContext(ctx, `SELECT hash FROM events WHERE session=? AND seq=?`, parent, child.seq).Scan(&parentHash); err != nil {
				return fmt.Errorf("%w: parent prefix: %w", ErrForkState, err)
			}
		}
		if prev != parentHash {
			return fmt.Errorf("%w: child %q prefix mismatch", ErrForkState, child.uid)
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE fork_batches SET status='COMMITTED' WHERE id=?`, batch); err != nil {
		return err
	}
	return tx.Commit()
}

// AbortFork retains every copied journal and allocation attempt, adds DELETE and advances fences
// atomically with ABORTED. A committed batch can never be rolled back or erased by recovery.
func (s *Store) AbortFork(ctx context.Context, batch string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, status, err := forkBatch(ctx, tx, batch)
	if err != nil {
		return err
	}
	if status == "ABORTED" {
		return nil
	}
	if status != "PREPARING" {
		return ErrForkState
	}
	children, err := forkBatchChildren(ctx, tx, batch)
	if err != nil {
		return err
	}
	for _, child := range children {
		if _, err := txDelete(ctx, tx, child.uid); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE fork_batches SET status='ABORTED' WHERE id=?`, batch); err != nil {
		return err
	}
	return tx.Commit()
}

// RecoverForks aborts only PREPARING batches. Call once under exclusive single-host startup,
// before admitting requests; running it alongside a live fork builder would abort that builder.
func (s *Store) RecoverForks(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM fork_batches WHERE status='PREPARING' ORDER BY id`)
	if err != nil {
		return err
	}
	var batches []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		batches = append(batches, id)
	}
	err = rows.Err()
	if closeErr := rows.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	for _, batch := range batches {
		if err := s.AbortFork(ctx, batch); err != nil {
			return err
		}
	}
	return nil
}
