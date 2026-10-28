package sqlitelog

import (
	"database/sql"

	"github.com/agent-primitives/sessions/api"
)

func migrateV8ToV9(tx *sql.Tx) error {
	if _, err := tx.Exec(`
ALTER TABLE approval_executions ADD COLUMN outcome TEXT NOT NULL DEFAULT 'RUNNING';
DELETE FROM approval_calls;
DELETE FROM approval_executions;
UPDATE sessions SET last_seq=0, latest_execution='', reporting_fork_seq=0,
 approval_corrupt=0, invalid_approval_executions=0;`); err != nil {
		return err
	}
	// Rebuild the shared projection, including records from already-open older writers.
	return backfillExecutions(tx, projectExecution)
}

func projectExecution(tx *sql.Tx, uid string, seq int64, ev api.Event) error {
	if err := projectApproval(tx, uid, seq, ev); err != nil {
		return err
	}
	if ev.Kind == api.EventLifecycle {
		return nil
	}
	if ev.ExecutionID == "" {
		// Legacy compatibility ends at the first modern execution. A later missing ID
		// must never regain reporting authority by reusing the legacy execution row.
		if _, err := tx.Exec(`UPDATE sessions SET approval_corrupt=1 WHERE session=?
 AND (? OR EXISTS(SELECT 1 FROM approval_executions WHERE session=? AND execution_id!=''))`,
			uid, ev.Kind == api.EventExecutionStart, uid); err != nil {
			return err
		}
	}
	outcome := api.ExecRunning
	switch ev.Kind {
	case api.EventEnd:
		if ev.End != nil {
			outcome = api.ExecCompleted
		}
	case api.EventError:
		if ev.Err != nil {
			outcome = api.ExecFailed
		}
	}
	_, err := tx.Exec(`UPDATE approval_executions SET outcome=? WHERE session=? AND execution_id=?`,
		string(outcome), uid, ev.ExecutionID)
	return err
}
