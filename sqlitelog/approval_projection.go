package sqlitelog

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/agent-primitives/sessions/api"
)

// v2ToV3 is a frozen reporting-only rung. Reducer state is indexed by execution and call:
// appending an event never decodes an old journal or rewrites a growing serialized history.
const v2ToV3 = `
ALTER TABLE sessions ADD COLUMN last_seq INTEGER NOT NULL DEFAULT 0;
ALTER TABLE sessions ADD COLUMN latest_execution TEXT NOT NULL DEFAULT '';
ALTER TABLE sessions ADD COLUMN reporting_fork_seq INTEGER NOT NULL DEFAULT 0;
ALTER TABLE sessions ADD COLUMN approval_corrupt INTEGER NOT NULL DEFAULT 0;
ALTER TABLE sessions ADD COLUMN invalid_approval_executions INTEGER NOT NULL DEFAULT 0;
CREATE TABLE approval_executions (
 session TEXT NOT NULL,
 execution_id TEXT NOT NULL,
 first_seq INTEGER NOT NULL,
 gated INTEGER NOT NULL DEFAULT 0,
 invalid INTEGER NOT NULL DEFAULT 0,
 has_start INTEGER NOT NULL DEFAULT 0,
 expected_inputs INTEGER,
 input_records INTEGER NOT NULL DEFAULT 0,
 input_messages INTEGER NOT NULL DEFAULT 0,
 completed INTEGER NOT NULL DEFAULT 0,
 call_id TEXT NOT NULL DEFAULT '',
 call_seq INTEGER NOT NULL DEFAULT 0,
 request_seq INTEGER NOT NULL DEFAULT 0,
 decision_seq INTEGER NOT NULL DEFAULT 0,
 approved INTEGER NOT NULL DEFAULT 0,
 receipt INTEGER NOT NULL DEFAULT 0,
 PRIMARY KEY (session, execution_id)
);
CREATE TABLE approval_calls (
 session TEXT NOT NULL,
 execution_id TEXT NOT NULL,
 call_id TEXT NOT NULL,
 gated INTEGER NOT NULL,
 PRIMARY KEY (session, execution_id, call_id)
);`

// Backfill uses the same bounded reducer as Append. Undecodable journals are isolated at
// session scope: preserve their metadata/cursor, suppress all references, and continue with
// healthy neighbors. SQL failures, unlike invalid evidence, abort the entire migration.
func migrateV2ToV3(tx *sql.Tx) error {
	if _, err := tx.Exec(v2ToV3); err != nil {
		return err
	}
	rows, err := tx.Query(`SELECT session FROM sessions ORDER BY session`)
	if err != nil {
		return err
	}
	var sessions []string
	for rows.Next() {
		var uid string
		if err := rows.Scan(&uid); err != nil {
			rows.Close()
			return err
		}
		sessions = append(sessions, uid)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, uid := range sessions {
		// Seek one row at a time rather than retaining all journals in memory. The primary key
		// bounds each lookup; closing it before reduction also avoids a live read/write cursor.
		var cursor int64
		for {
			var seq int64
			var blob []byte
			err := tx.QueryRow(`SELECT seq,event FROM events WHERE session=? AND seq>? ORDER BY seq LIMIT 1`, uid, cursor).Scan(&seq, &blob)
			if errors.Is(err, sql.ErrNoRows) {
				break
			}
			if err != nil {
				return err
			}
			cursor = seq
			ev, err := decode(blob)
			if err != nil {
				if _, err := tx.Exec(`UPDATE sessions SET approval_corrupt=1, last_seq=(SELECT COALESCE(MAX(seq),0) FROM events WHERE session=?) WHERE session=?`, uid, uid); err != nil {
					return err
				}
				break
			}
			if err := projectApproval(tx, uid, seq, ev); err != nil {
				return fmt.Errorf("session %q seq %d: %w", uid, seq, err)
			}
		}
	}
	return nil
}

// approvalExecution is only reporting evidence, never admission/decision/recovery authority.
// Structural invalidity is sticky within one execution. Invalid gated evidence suppresses all
// references, matching strict inspection, while malformed nongated histories remain unrelated.
// Input completeness is evaluated at the captured cursor, so a partial start may become valid.
type approvalExecution struct {
	firstSeq                         int64
	gated, invalid, hasStart         bool
	expectedInputs                   sql.NullInt64
	inputs, messages                 int64
	completed                        bool
	callID                           string
	callSeq, requestSeq, decisionSeq int64
	approved, receipt                bool
}

func projectApproval(tx *sql.Tx, uid string, seq int64, ev api.Event) error {
	// An already-open older writer can skip this reducer. Never regain reporting
	// authority by advancing the cursor over events we have not inspected.
	if _, err := tx.Exec(`UPDATE sessions SET approval_corrupt=CASE WHEN last_seq != ? THEN 1 ELSE approval_corrupt END, last_seq=? WHERE session=?`, seq-1, seq, uid); err != nil {
		return err
	}
	if ev.Kind == api.EventLifecycle {
		if ev.Lifecycle != nil && ev.Lifecycle.Kind == api.LifecycleFork {
			_, err := tx.Exec(`UPDATE sessions SET reporting_fork_seq=? WHERE session=?`, seq, uid)
			return err
		}
		return nil
	}
	var state approvalExecution
	err := tx.QueryRow(`SELECT first_seq,gated,invalid,has_start,expected_inputs,input_records,input_messages,completed,call_id,call_seq,request_seq,decision_seq,approved,receipt FROM approval_executions WHERE session=? AND execution_id=?`, uid, ev.ExecutionID).Scan(&state.firstSeq, &state.gated, &state.invalid, &state.hasStart, &state.expectedInputs, &state.inputs, &state.messages, &state.completed, &state.callID, &state.callSeq, &state.requestSeq, &state.decisionSeq, &state.approved, &state.receipt)
	if errors.Is(err, sql.ErrNoRows) {
		state.firstSeq = seq
		if _, err := tx.Exec(`UPDATE sessions SET latest_execution=? WHERE session=?`, ev.ExecutionID, uid); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	wasInvalid := state.invalidApproval(ev.ExecutionID)
	if ev.Kind == api.EventApprovalRequest || ev.Kind == api.EventApprovalResult ||
		ev.Kind == api.EventToolCall && ev.ToolCall != nil && ev.ToolCall.Mediation == api.MediationRequiresApproval ||
		ev.Kind == api.EventToolResult && ev.Result != nil && (ev.Result.ApprovalRequestSeq != 0 || ev.Result.ApprovalDecisionSeq != 0) {
		state.gated = true
	}
	if !state.invalid {
		if err := state.reduce(tx, uid, seq, ev); err != nil {
			return err
		}
	}
	if nowInvalid := state.invalidApproval(ev.ExecutionID); wasInvalid != nowInvalid {
		delta := -1
		if nowInvalid {
			delta = 1
		}
		if _, err := tx.Exec(`UPDATE sessions SET invalid_approval_executions=invalid_approval_executions+? WHERE session=?`, delta, uid); err != nil {
			return err
		}
	}
	_, err = tx.Exec(`INSERT INTO approval_executions(session,execution_id,first_seq,gated,invalid,has_start,expected_inputs,input_records,input_messages,completed,call_id,call_seq,request_seq,decision_seq,approved,receipt)
 VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(session,execution_id) DO UPDATE SET
 gated=excluded.gated,invalid=excluded.invalid,has_start=excluded.has_start,expected_inputs=excluded.expected_inputs,input_records=excluded.input_records,input_messages=excluded.input_messages,completed=excluded.completed,call_id=excluded.call_id,call_seq=excluded.call_seq,request_seq=excluded.request_seq,decision_seq=excluded.decision_seq,approved=excluded.approved,receipt=excluded.receipt`, uid, ev.ExecutionID, state.firstSeq, state.gated, state.invalid, state.hasStart, state.expectedInputs, state.inputs, state.messages, state.completed, state.callID, state.callSeq, state.requestSeq, state.decisionSeq, state.approved, state.receipt)
	return err
}

func (s approvalExecution) invalidApproval(executionID string) bool {
	return s.gated && (s.invalid || executionID == "" || s.hasStart &&
		(!s.expectedInputs.Valid || s.expectedInputs.Int64 < 0 || s.inputs != s.messages || s.inputs != s.expectedInputs.Int64))
}

func (s *approvalExecution) reduce(tx *sql.Tx, uid string, seq int64, ev api.Event) error {
	open := s.callID != "" && !s.receipt
	switch ev.Kind {
	case api.EventExecutionStart:
		if seq != s.firstSeq || ev.ExecutionStart == nil {
			s.invalid = true
			break
		}
		s.hasStart = true
		if ev.ExecutionStart.InputCount != nil {
			s.expectedInputs = sql.NullInt64{Int64: *ev.ExecutionStart.InputCount, Valid: true}
		}
	case api.EventInput:
		if open {
			s.invalid = true
			break
		}
		s.inputs++
		if ev.Message != nil {
			s.messages++
		}
	case api.EventToolCall:
		if open || ev.ToolCall == nil {
			s.invalid = true
			break
		}
		call := ev.ToolCall
		gated := call.Mediation == api.MediationRequiresApproval
		var priorGated bool
		err := tx.QueryRow(`SELECT gated FROM approval_calls WHERE session=? AND execution_id=? AND call_id=?`, uid, ev.ExecutionID, call.ID).Scan(&priorGated)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err == nil && (priorGated || gated) {
			s.invalid = true
			break
		}
		if _, err := tx.Exec(`INSERT OR IGNORE INTO approval_calls(session,execution_id,call_id,gated) VALUES(?,?,?,?)`, uid, ev.ExecutionID, call.ID, gated); err != nil {
			return err
		}
		if !gated {
			break
		}
		if s.completed || call.ID == "" || call.IdempotencyKey == "" {
			s.invalid = true
			break
		}
		s.callID = call.ID
		s.callSeq = seq
		s.requestSeq = 0
		s.decisionSeq = 0
		s.receipt = false
		s.approved = false
	case api.EventApprovalRequest:
		if ev.Approval == nil || !open || s.requestSeq != 0 || ev.Approval.ToolCallID != s.callID {
			s.invalid = true
			break
		}
		s.requestSeq = seq
	case api.EventApprovalResult:
		if ev.ApprovalResult == nil || !open || s.requestSeq == 0 || s.decisionSeq != 0 {
			s.invalid = true
			break
		}
		decision := ev.ApprovalResult
		if decision.ToolCallID != s.callID || decision.RequestSeq != s.requestSeq {
			s.invalid = true
			break
		}
		s.decisionSeq = seq
		s.approved = decision.Approved
	case api.EventToolResult:
		result := ev.Result
		if open {
			if result == nil || s.requestSeq == 0 || s.decisionSeq == 0 {
				s.invalid = true
				break
			}
			valid := result.IsError && result.Code == api.ToolResultCodeApprovalDenied
			if s.approved {
				valid = !result.IsError && result.Code == api.ToolResultCodeUnspecified ||
					result.IsError && (result.Code == api.ToolResultCodeExecutorError || result.Code == api.ToolResultCodePolicyDenied)
			}
			if result.ID != s.callID || result.ApprovalRequestSeq != s.requestSeq || result.ApprovalDecisionSeq != s.decisionSeq || !valid {
				s.invalid = true
				break
			}
			s.receipt = true
		} else if result != nil {
			var gated bool
			err := tx.QueryRow(`SELECT gated FROM approval_calls WHERE session=? AND execution_id=? AND call_id=?`, uid, ev.ExecutionID, result.ID).Scan(&gated)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			if gated || result.ApprovalRequestSeq != 0 || result.ApprovalDecisionSeq != 0 {
				s.invalid = true
			}
		}
	case api.EventEnd:
		if open {
			s.invalid = true
		}
		s.completed = true
	case api.EventError:
		// Audit only: neither a decision nor a terminal modern execution boundary.
	default:
		if open {
			s.invalid = true
		}
	}
	return nil
}
