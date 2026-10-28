package sqlitelog

import (
	"database/sql"

	"github.com/agent-primitives/sessions/eventlog"
)

// ForkSession returns a log handle for a fresh fork child. It defers inherited metadata until
// NewFence, then commits it atomically with the child's first fence. Obtaining the handle publishes
// nothing, so refused/unprovisioned clones leave no phantom sessions. Use Session for later access.
func (s *Store) ForkSession(meta SessionMeta) *Log {
	return &Log{store: s, session: meta.UID, forkMetadata: &meta}
}

// ForkSessionLog is ForkSession as an eventlog.SessionJournal, the form session.SessionStore
// needs. The value is the same *Log, so publication still waits for the child's first fence.
func (s *Store) ForkSessionLog(meta SessionMeta) eventlog.SessionJournal { return s.ForkSession(meta) }

func (l *Log) forkFence() (int64, error) {
	tx, err := l.store.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if err := l.store.putSession(tx, *l.forkMetadata); err != nil {
		return 0, err
	}
	fence, err := l.newFence(tx)
	if err != nil {
		return 0, err
	}
	return fence, tx.Commit()
}

// A fence and fork metadata must use the same transaction; normal session fencing still uses DB.
type fenceQuerier interface {
	QueryRow(string, ...any) *sql.Row
}
