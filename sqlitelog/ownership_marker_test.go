package sqlitelog

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"path/filepath"
	"testing"

	sqlite "modernc.org/sqlite"
)

// Fail at the database boundary without relying on OS-specific SQLite lock timing.
type markerFailure struct{ cause error }

func (f markerFailure) Connect(context.Context) (driver.Conn, error) { return f, nil }
func (f markerFailure) Driver() driver.Driver                        { return f }
func (f markerFailure) Open(string) (driver.Conn, error)             { return f, nil }
func (f markerFailure) Close() error                                 { return nil }
func (f markerFailure) Begin() (driver.Tx, error)                    { return nil, f.cause }
func (f markerFailure) Prepare(string) (driver.Stmt, error)          { return nil, f.cause }
func (f markerFailure) QueryContext(context.Context, string, []driver.NamedValue) (driver.Rows, error) {
	return nil, f.cause
}

type markerQueryFailure struct {
	db, failed *sql.DB
	markerOnly bool
}

func (q markerQueryFailure) QueryRow(query string, args ...any) *sql.Row {
	if !q.markerOnly || query == `SELECT mode FROM journal_authorization WHERE singleton=1` {
		return q.failed.QueryRow(query, args...)
	}
	return q.db.QueryRow(query, args...)
}

func TestAuthorizationMarkerPreservesReadFailures(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE journal_authorization(singleton INTEGER, mode TEXT); INSERT INTO journal_authorization VALUES(1, 'off')`); err != nil {
		t.Fatal(err)
	}
	for _, cause := range []error{io.ErrUnexpectedEOF, context.Canceled} {
		for _, markerOnly := range []bool{false, true} {
			failed := sql.OpenDB(markerFailure{cause})
			err := checkAuthorization(markerQueryFailure{db, failed, markerOnly})
			failed.Close()
			if !errors.Is(err, cause) || errors.Is(err, ErrUnsupportedAuthorization) {
				t.Fatalf("markerOnly=%v: got %v, want operational cause %v without unsupported sentinel", markerOnly, err, cause)
			}
		}
	}
}

func TestAuthorizationMarkerPreservesBusy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "locked.db")
	holder, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	if _, err := holder.Exec(`CREATE TABLE journal_authorization(singleton INTEGER, mode TEXT); INSERT INTO journal_authorization VALUES(1, 'off'); BEGIN EXCLUSIVE`); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = holder.Exec(`ROLLBACK`) }()
	reader, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	err = checkAuthorization(reader)
	var cause *sqlite.Error
	if !errors.As(err, &cause) || cause.Code() != 5 || errors.Is(err, ErrUnsupportedAuthorization) {
		t.Fatalf("locked marker: %v, want SQLITE_BUSY without unsupported sentinel", err)
	}
}

func TestAuthorizationMarkerMissingTable(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := checkAuthorization(db); !errors.Is(err, ErrUnsupportedAuthorization) {
		t.Fatalf("missing table: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE journal_authorization(singleton INTEGER)`); err != nil {
		t.Fatal(err)
	}
	if err := checkAuthorization(db); err == nil || errors.Is(err, ErrUnsupportedAuthorization) {
		t.Fatalf("malformed marker table classified as unsupported: %v", err)
	}
}
