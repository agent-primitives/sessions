package sqlitelog

import (
	"database/sql"
	"errors"
	"fmt"
)

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
