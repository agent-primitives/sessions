package sqlitelog

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"modernc.org/sqlite"
)

// sessionTracesSchema holds the W3C traceparent of each session's most recent operation, so the next
// one can link to it across a wait or suspension. It is telemetry only: outside the hash chain and
// the sessions row, so a run-specific trace id never reaches canonical bytes or replay. Open creates
// it without bumping user_version, because builds that do not know it ignore it safely; a version
// bump would only lock them out of journals whose meaning has not changed.
const sessionTracesSchema = `CREATE TABLE IF NOT EXISTS session_traces (
  session     TEXT PRIMARY KEY,
  traceparent TEXT NOT NULL
);`

// sqliteReadOnly is SQLITE_READONLY, the primary result code for a write on a read-only connection.
const sqliteReadOnly = 8

// ensureSessionTraces creates session_traces when it is missing. A read-only connection to a
// journal without it opens anyway, as it would on a build that does not know the table, and reads
// it as holding no links.
func ensureSessionTraces(db *sql.DB) error {
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'session_traces'`).Scan(&n); err != nil {
		return fmt.Errorf("sqlitelog: session_traces: %w", err)
	}
	if n != 0 {
		return nil
	}
	if _, err := db.Exec(sessionTracesSchema); err != nil {
		var serr *sqlite.Error
		if errors.As(err, &serr) && serr.Code()&0xff == sqliteReadOnly {
			return nil
		}
		return fmt.Errorf("sqlitelog: session_traces: %w", err)
	}
	return nil
}

// noTraceTable reports whether err is a read of session_traces on a read-only journal that lacks it.
func noTraceTable(err error) bool {
	return err != nil && strings.Contains(err.Error(), "no such table: session_traces")
}

// LastTraceParent returns the W3C traceparent recorded for uid, or "" when
// none was recorded (tracing was off, the session predates it, or the journal is read-only and
// was written by a build without the table).
func (s *Store) LastTraceParent(uid string) (string, error) {
	var tp string
	err := s.db.QueryRow(`SELECT traceparent FROM session_traces WHERE session = ?`, uid).Scan(&tp)
	if errors.Is(err, sql.ErrNoRows) || noTraceTable(err) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("sqlitelog: read trace link for %q: %w", uid, err)
	}
	return tp, nil
}

// InitTraceParent records traceparent as uid's first operation unless uid already has one. A fork
// stamps each child before the child is published, so the child's first operation links to the
// fork; never overwriting means a late stamp cannot hide an operation that already ran. It is
// telemetry, not session state: never hash-chained, never replayed, and losing it only breaks a
// trace link.
func (s *Store) InitTraceParent(uid, traceparent string) error {
	_, err := s.db.Exec(`INSERT INTO session_traces (session, traceparent) VALUES (?, ?)
		ON CONFLICT(session) DO NOTHING`, uid, traceparent)
	if err != nil {
		return fmt.Errorf("sqlitelog: record trace link for %q: %w", uid, err)
	}
	return nil
}

// ExchangeTraceParent records traceparent as uid's most recent operation and returns the one it
// replaces, or "" when there was none. The read and the write share one write transaction, so two
// overlapping operations on a session each link to a distinct predecessor rather than the same one.
func (s *Store) ExchangeTraceParent(uid, traceparent string) (prev string, err error) {
	tx, err := s.db.Begin()
	if err != nil {
		return "", fmt.Errorf("sqlitelog: exchange trace link for %q: %w", uid, err)
	}
	defer func() { _ = tx.Rollback() }()
	err = tx.QueryRow(`SELECT traceparent FROM session_traces WHERE session = ?`, uid).Scan(&prev)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("sqlitelog: exchange trace link for %q: %w", uid, err)
	}
	if _, err := tx.Exec(`INSERT INTO session_traces (session, traceparent) VALUES (?, ?)
		ON CONFLICT(session) DO UPDATE SET traceparent = excluded.traceparent`, uid, traceparent); err != nil {
		return "", fmt.Errorf("sqlitelog: exchange trace link for %q: %w", uid, err)
	}
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("sqlitelog: exchange trace link for %q: %w", uid, err)
	}
	return prev, nil
}
