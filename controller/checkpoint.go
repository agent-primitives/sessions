package controller

import (
	"errors"

	"github.com/agent-primitives/sessions/api"
	"github.com/agent-primitives/sessions/eventlog"
)

// ErrCheckpointIncompleteTurn refuses a memory checkpoint whose latest execution recovery may run
// again. Capturing RAM after the harness changed it but before END committed would let Resume apply
// the same mutation twice.
var ErrCheckpointIncompleteTurn = errors.New("controller: checkpoint refused because the latest execution is incomplete and recovery may run it again")

// ValidateCheckpointState verifies records and reports whether a memory checkpoint can be taken
// without capturing a turn that Resume may run again. A partial input prefix is safe because the
// harness was not invoked. An owned undecided approval request and an inherited unresolved request
// are also safe: recovery parks or refuses them without invoking the harness. A decided gate is not
// safe: an earlier recovery may have changed RAM before failing without a receipt.
func ValidateCheckpointState(records []eventlog.Record) error {
	if err := verifyRecords("", records); err != nil {
		return err
	}
	events := make([]api.Event, 0, len(records))
	for _, record := range records {
		events = append(events, record.Event)
	}
	executions, err := recordedExecutions(events)
	if err != nil {
		return err
	}
	scan, err := scanApprovalRecords(records, 0)
	if err != nil {
		return err
	}
	execution, err := pendingResumeExecution(events, executions)
	if err != nil {
		if errors.Is(err, ErrIncompleteInvocation) && len(executions) > 0 &&
			incompleteInputPrefix(events, executions[len(executions)-1]) {
			return nil
		}
		return err
	}
	if execution == nil {
		return nil
	}
	if state := scan.selected; state != nil && state.ExecutionID == execution.id && state.Receipt == nil {
		if state.Inherited || state.Request != nil && state.Decision == nil {
			return nil
		}
	}
	return ErrCheckpointIncompleteTurn
}

// incompleteInputPrefix recognizes only the prefix Exec can leave before Harness.Run: one valid
// start followed by fewer valid inputs than it declared, with no later event from any execution.
func incompleteInputPrefix(events []api.Event, execution recordedExecution) bool {
	if !execution.hasStart || execution.completed || execution.inputCount == nil ||
		execution.inputRecords != len(execution.inputs) || int64(execution.inputRecords) >= *execution.inputCount {
		return false
	}
	for i, event := range events[execution.start:] {
		if event.Kind == api.EventLifecycle {
			continue
		}
		if event.ExecutionID != execution.id {
			return false
		}
		switch event.Kind {
		case api.EventExecutionStart:
			if i != 0 || event.ExecutionStart == nil {
				return false
			}
		case api.EventInput:
			if event.Message == nil {
				return false
			}
		default:
			return false
		}
	}
	return true
}
