package api

// ExecState is the execution/turn axis of a session's lifecycle — the event-log axis. It
// is modeled separately from ComputeState (the incarnation axis) rather than flattened
// into a single enum, matching the durable-log & replay contract (§6). Both axes are
// authoritative and are reported independently on the wire.
type ExecState string

const (
	ExecPending   ExecState = "PENDING"
	ExecRunning   ExecState = "RUNNING"  // unfinished or uncertain, not confirmed controller liveness
	ExecAwaiting  ExecState = "AWAITING" // blocked on approval/input, resolved from the log
	ExecCompleted ExecState = "COMPLETED"
	ExecFailed    ExecState = "FAILED" // recorded error; recovery remains possible
	ExecCanceled  ExecState = "CANCELED"
)

// DeletionState is the permanent-deletion axis, independent of execution and compute.
// Pending acknowledges durable intent only; retained resources have not been reclaimed.
// Any other value is unrecognized and fails closed: it is treated as deleted, never as ACTIVE.
type DeletionState string

const (
	DeletionActive  DeletionState = "ACTIVE"
	DeletionPending DeletionState = "PENDING"
)

// ComputeState is the incarnation (compute/sandbox) axis of a session's lifecycle.
type ComputeState string

const (
	ComputeNone       ComputeState = "NONE" // no incarnation
	ComputeLive       ComputeState = "LIVE" // running
	ComputeWarm       ComputeState = "WARM" // paused: resident, worker held
	ComputeCold       ComputeState = "COLD" // suspended: snapshot in storage, worker freed
	ComputeTerminated ComputeState = "TERMINATED"
)
