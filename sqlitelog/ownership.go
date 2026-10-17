package sqlitelog

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/agent-primitives/sessions/api"
)

// ErrUnsupportedAuthorization is returned by Open when the journal requires caller
// enforcement, has an unknown authorization mode, or lacks its authorization marker.
// This host has no enforcement capability, even if it understands the journal's schema.
var ErrUnsupportedAuthorization = errors.New("sqlitelog: unsupported journal authorization")

func migrateV4ToV5(tx *sql.Tx) error {
	_, err := tx.Exec(`ALTER TABLE sessions ADD COLUMN owner_issuer TEXT COLLATE BINARY NOT NULL DEFAULT '';
		ALTER TABLE sessions ADD COLUMN owner_subject TEXT COLLATE BINARY NOT NULL DEFAULT ''
			CHECK ((owner_issuer = '') = (owner_subject = ''));
		CREATE INDEX sessions_owner_project_created
			ON sessions(owner_issuer, owner_subject, project, created_at DESC, session);
		CREATE TABLE journal_authorization (
			singleton INTEGER PRIMARY KEY CHECK (singleton = 1),
			mode TEXT NOT NULL
		);
		INSERT INTO journal_authorization(singleton, mode) VALUES(1, 'off');`)
	return err
}

// ErrSessionExists is returned by CreateOwnedSession for any existing UID,
// including unbound log-created rows and tombstones. Creation never claims them.
var ErrSessionExists = errors.New("sqlitelog: session already exists")

// CreateOwnedSession atomically inserts metadata and an immutable verified owner,
// admitting the harness under the same transaction as PutSessionOnActiveHarness.
// registered distinguishes registered from static harness names. The trusted host
// must verify owner; this method does not authenticate it. Existing UIDs are refused.
func (s *Store) CreateOwnedSession(m SessionMeta, owner api.CallerIdentity, registered bool) error {
	if !owner.Valid() {
		return api.ErrInvalidCallerIdentity
	}
	if m.UID == "" {
		return errors.New("sqlitelog: session metadata requires a uid")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("sqlitelog: create owned session: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	// Target only the UID conflict: a CHECK or other integrity failure is not a duplicate.
	result, err := tx.Exec(`INSERT INTO sessions(session, owner_issuer, owner_subject)
		VALUES(?, ?, ?) ON CONFLICT(session) DO NOTHING`, m.UID, owner.Issuer, owner.Subject)
	if err != nil {
		return fmt.Errorf("sqlitelog: insert owned session: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count == 0 {
		return ErrSessionExists
	}
	if err := checkActiveHarness(tx, m.Harness, registered); err != nil {
		return err
	}
	if err := s.putSession(tx, m); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sqlitelog: create owned session: %w", err)
	}
	return nil
}

func checkAuthorization(db queryRower) error {
	var mode string
	if err := db.QueryRow(`SELECT mode FROM journal_authorization WHERE singleton=1`).Scan(&mode); err != nil {
		return fmt.Errorf("%w: read marker: %w", ErrUnsupportedAuthorization, err)
	}
	if mode != "off" {
		return ErrUnsupportedAuthorization
	}
	return nil
}
