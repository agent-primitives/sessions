package sqlitelog_test

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	sqlite "modernc.org/sqlite"

	"github.com/agent-primitives/sessions/sqlitelog"
)

// sqliteBusy is SQLITE_BUSY's primary result code. Extended codes such as SQLITE_BUSY_SNAPSHOT carry
// it in their low byte.
const sqliteBusy = 5

func isBusy(err error) bool {
	var se *sqlite.Error
	return errors.As(err, &se) && se.Code()&0xff == sqliteBusy
}

// TestOpenSerializesWritersForEveryDSNForm races an Append and a RegisterHarness from two Stores
// (two "pods") on one database, opened through each DSN form Open accepts. Both are read-then-write
// transactions. Under BEGIN DEFERRED each takes a read snapshot, then fails with SQLITE_BUSY when
// it tries to write after another connection has committed; under BEGIN IMMEDIATE each waits for
// the write lock at BEGIN and the two serialize. A third connection holds the write lock and then
// commits a write, so both operations are pending at once and the stale-snapshot outcome is
// deterministic rather than timing-dependent.
//
// The caller-supplied _txlock=deferred row is the control: it shows Open does not overwrite a
// _txlock the caller chose, and that this scenario does produce SQLITE_BUSY without IMMEDIATE. An
// empty _txlock is not a choice (the driver ignores it), so that row gets IMMEDIATE.
func TestOpenSerializesWritersForEveryDSNForm(t *testing.T) {
	for _, tc := range []struct {
		name     string
		dsn      func(path string) string
		wantBusy bool
	}{
		{name: "plain path", dsn: func(p string) string { return p }},
		{name: "plain path with query", dsn: func(p string) string { return p + "?_pragma=busy_timeout(5000)" }},
		{name: "file URI", dsn: func(p string) string { return "file:" + p }},
		{name: "file URI with query", dsn: func(p string) string { return "file:" + p + "?mode=rwc" }},
		{name: "file URI with empty query", dsn: func(p string) string { return "file:" + p + "?" }},
		{name: "file URI with fragment", dsn: func(p string) string { return "file:" + p + "#frag" }},
		{name: "file URI with empty _txlock", dsn: func(p string) string { return "file:" + p + "?mode=rwc&_txlock=" }},
		{name: "file URI with caller _txlock=immediate", dsn: func(p string) string { return "file:" + p + "?_txlock=immediate" }},
		{name: "file URI with caller _txlock=deferred", dsn: func(p string) string { return "file:" + p + "?mode=rwc&_txlock=deferred" }, wantBusy: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "log.db")
			dsn := tc.dsn(path)
			appender := mustOpen(t, dsn)
			registrar := mustOpen(t, dsn)

			l := appender.Session("s1")
			fence, err := l.NewFence()
			if err != nil {
				t.Fatalf("fence: %v", err)
			}

			holder, err := sql.Open("sqlite", "file:"+path+"?_txlock=immediate")
			if err != nil {
				t.Fatalf("open holder: %v", err)
			}
			defer holder.Close()
			holder.SetMaxOpenConns(1)
			tx, err := holder.Begin()
			if err != nil {
				t.Fatalf("holder begin: %v", err)
			}
			defer func() { _ = tx.Rollback() }()
			if _, err := tx.Exec(`INSERT INTO sessions(session, fence) VALUES ('held', 1)`); err != nil {
				t.Fatalf("holder write: %v", err)
			}

			appended := make(chan error, 1)
			registered := make(chan error, 1)
			go func() {
				_, err := l.Append(0, fence, sampleEvents()[0])
				appended <- err
			}()
			go func() {
				_, _, err := registrar.RegisterHarness(sqlitelog.HarnessRecord{Name: "h", UID: "uid-1", Spec: `{}`, SpecDigest: "d1"})
				registered <- err
			}()

			if !tc.wantBusy {
				// Both must wait for the lock, not fail on it. Long enough for both to have reached
				// their first statement.
				select {
				case err := <-appended:
					t.Fatalf("Append returned while the write lock was held: %v", err)
				case err := <-registered:
					t.Fatalf("RegisterHarness returned while the write lock was held: %v", err)
				case <-time.After(250 * time.Millisecond):
				}
			} else {
				// Let both take their read snapshot before the holder commits.
				time.Sleep(250 * time.Millisecond)
			}
			if err := tx.Commit(); err != nil {
				t.Fatalf("holder commit: %v", err)
			}

			for op, ch := range map[string]chan error{"Append": appended, "RegisterHarness": registered} {
				var err error
				select {
				case err = <-ch:
				case <-time.After(30 * time.Second):
					t.Fatalf("%s did not return after the write lock was released", op)
				}
				switch {
				case tc.wantBusy && !isBusy(err):
					t.Errorf("%s: got %v, want SQLITE_BUSY under the caller's _txlock=deferred", op, err)
				case !tc.wantBusy && err != nil:
					t.Errorf("%s: %v, want the writers to serialize", op, err)
				}
			}
			if tc.wantBusy {
				return
			}
			if head, err := l.Head(); err != nil || head != 1 {
				t.Errorf("head after append: %d, %v; want 1", head, err)
			}
			if h, err := appender.Harness("h"); err != nil || h.State != sqlitelog.HarnessActive {
				t.Errorf("registered harness seen by the other store: %+v, %v", h, err)
			}
		})
	}
}

// A DSN query the driver cannot use makes Open fail with the driver's error instead of opening the
// database with the caller's parameters dropped. The unknown _txlock row also shows Open keeps the
// caller's _txlock: replacing it with immediate would let Open succeed.
func TestOpenRejectsUnusableDSNQuery(t *testing.T) {
	for name, query := range map[string]string{
		"malformed escape": "mode=rwc&x=%zz",
		"unknown _txlock":  "_txlock=sometimes",
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "log.db")
			s, err := sqlitelog.Open("file:" + path + "?" + query)
			if err == nil {
				s.Close()
				t.Fatalf("Open(%q) succeeded, want the driver's DSN error", query)
			}
		})
	}
}
