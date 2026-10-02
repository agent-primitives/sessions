package sqlitelog

import (
	"database/sql"
	"errors"
	"fmt"
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

// LastTraceParent returns the W3C traceparent recorded by SetLastTraceParent for uid, or "" when
// none was recorded (tracing was off, or the session predates it).
func (s *Store) LastTraceParent(uid string) (string, error) {
	var tp string
	err := s.db.QueryRow(`SELECT traceparent FROM session_traces WHERE session = ?`, uid).Scan(&tp)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("sqlitelog: read trace link for %q: %w", uid, err)
	}
	return tp, nil
}

// SetLastTraceParent records traceparent as uid's most recent operation. It is telemetry, not
// session state: it is never hash-chained, never replayed, and losing it only breaks a trace link.
func (s *Store) SetLastTraceParent(uid, traceparent string) error {
	_, err := s.db.Exec(`INSERT INTO session_traces (session, traceparent) VALUES (?, ?)
		ON CONFLICT(session) DO UPDATE SET traceparent = excluded.traceparent`, uid, traceparent)
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
