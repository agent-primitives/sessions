package placement

import (
	"context"

	"github.com/agent-primitives/sessions/api"
	"github.com/agent-primitives/sessions/controller"
	"github.com/agent-primitives/sessions/eventlog"
)

// CloseApproval closes an approval gate under the shared session guard, as Approve journals a
// decision: it returns ErrSessionBusy while another operation such as a live Resume holds the
// guard, and ErrCheckpointing while a stateful Fork holds the session's checkpoint mark. Like
// Approve it needs no served harness or compute. See controller.CloseApproval for the writes.
func (r *Registry) CloseApproval(ctx context.Context, log eventlog.Store, sessionUID string, ref api.ApprovalRef, closer api.CallerIdentity) (decision, receipt eventlog.Record, err error) {
	release, err := r.guard.tryLock(sessionUID)
	if err != nil {
		return eventlog.Record{}, eventlog.Record{}, err
	}
	defer release()
	if err := r.sessions.markCheckpoint(sessionUID); err != nil {
		return eventlog.Record{}, eventlog.Record{}, err
	}
	defer r.sessions.endCheckpoint(sessionUID)
	return controller.CloseApproval(log, ref, closer)
}
