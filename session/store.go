package session

import (
	"context"
	"reflect"

	"github.com/agent-primitives/sessions/api"
	v1 "github.com/agent-primitives/sessions/api/genpb"
	"github.com/agent-primitives/sessions/eventlog"
	"github.com/agent-primitives/sessions/sqlitelog"
)

// SessionStore is the durable state Service reads and writes: session metadata, the per-session
// journals, owner-filtered listing, the approval and deletion projections, and harness lookups.
// *sqlitelog.Store is the reference implementation and the only one in this module; every method
// has the semantics of its *sqlitelog.Store namesake, including the record types, the paging
// limits and the sentinel errors the service maps to gRPC codes, which an implementation returns
// wrapped so that errors.Is matches them (sqlitelog.ErrSessionNotFound, ErrSessionExists,
// ErrHarnessNotFound, ErrHarnessRetired, ErrHarnessNameCollision, ErrInvalidPageToken, and
// eventlog.ErrDeleted).
//
// The service also relies on these properties, which the signatures cannot state:
//   - Session rows and journals share one transaction scope. SessionInfo's LastSeq, ComputeState
//     and PendingApproval are projections updated inside the journal append that changes them, so
//     a SessionInfo read after an append returns the new values.
//   - PutSessionOnActiveHarness and CreateOwnedSession check the harness catalog and write the
//     row in one transaction, so a RetireHarness that has returned or a registration under a static
//     name is always seen, including by other hosts sharing the store.
//   - DeleteSession appends the journal's DELETE record and sets the deletion fields in one
//     transaction. After it, the session's journal refuses writes with eventlog.ErrDeleted, and
//     its CheckWritable reports that too. The host calls CheckWritable before any compute work
//     and before answering an approval retry, so a journal that wraps another forwards it.
//   - A durable journal records allocation attempts through the optional BeginAllocation and
//     ConfirmAllocation methods that placement checks for; a journal without them still runs,
//     without the allocation ledger.
//   - Replay returns stored events with their protobuf field presence through a journal's optional
//     ReadProto method, so an audit client can detect presence-only tampering; for a journal
//     without it, Replay converts Read's domain records, which carry no presence.
//   - Trace links between a session's operations go through the store's optional
//     ExchangeTraceParent and SetLastTraceParent methods (traceLinkStore); a store without them
//     still serves every RPC, and its sessions' operations are traced but not linked.
//
// spec.md §6.7 states these obligations as requirements STORE-1 to STORE-7.
type SessionStore interface {
	// SessionLog returns the journal of session uid. Obtaining the handle writes nothing.
	SessionLog(uid string) eventlog.SessionJournal
	// ForkSessionLog returns the journal of a fork child that is not yet published: meta is
	// committed with the child's first fence (NewFence), so a refused or unprovisioned clone leaves
	// no session behind.
	ForkSessionLog(meta sqlitelog.SessionMeta) eventlog.SessionJournal
	// SessionInfo returns one session's metadata and its journal projections.
	SessionInfo(uid string) (sqlitelog.SessionInfo, error)
	// ListSessions returns one project's ACTIVE sessions, newest first, filtered by owner when
	// opts.Owner is set.
	ListSessions(opts sqlitelog.ListOptions) (sqlitelog.SessionPage, error)
	// PutSession upserts session metadata without a harness admission check.
	PutSession(meta sqlitelog.SessionMeta) error
	// PutSessionOnActiveHarness upserts session metadata after admitting its harness: the write
	// happens only if the harness is servable.
	PutSessionOnActiveHarness(meta sqlitelog.SessionMeta, registered bool) error
	// CreateOwnedSession inserts session metadata bound to a verified owner if its harness is
	// servable; an existing UID is refused.
	CreateOwnedSession(meta sqlitelog.SessionMeta, owner api.CallerIdentity, registered bool) error
	// DeleteSession commits permanent deletion intent and returns the resulting session.
	DeleteSession(ctx context.Context, uid string) (sqlitelog.SessionInfo, error)
	// Harness returns one harness registration.
	Harness(name string) (sqlitelog.HarnessRecord, error)
}

// HarnessStore is the harness catalog HarnessRegistry and ReserveStaticHarnessNames read and
// write. *sqlitelog.Store is the reference implementation; every method has the semantics of its
// *sqlitelog.Store namesake, including the sentinel errors (sqlitelog.ErrHarnessNotFound,
// ErrHarnessExists, ErrHarnessSpecConflict, ErrHarnessNameReserved, ErrHarnessNameCollision),
// returned wrapped so that errors.Is matches them.
//
// RegisterHarness, CreateHarness and ReserveHarnessNames each check and write in one transaction,
// and each checks the others' rows, so a name is either reserved for a static harness or
// registered, never both, and of concurrent CreateHarness calls for one name at most one succeeds,
// however hosts sharing the store interleave. A host that serves sessions gives Service and
// HarnessRegistry the same store, since session admission reads this catalog. spec.md §6.7 states
// these obligations as requirements.
type HarnessStore interface {
	// Harness returns one registration, active or retired.
	Harness(name string) (sqlitelog.HarnessRecord, error)
	// ListHarnesses returns up to opts.Limit registrations in name order, after opts.After. It
	// returns fewer than opts.Limit only when no registration remains: callers page until then.
	ListHarnesses(opts sqlitelog.HarnessListOptions) ([]sqlitelog.HarnessRecord, error)
	// RegisterHarness stores rec as an active registration, or recognizes or reactivates it.
	RegisterHarness(rec sqlitelog.HarnessRecord) (sqlitelog.HarnessRecord, sqlitelog.RegisterResult, error)
	// CreateHarness stores rec as an active registration only if its name was never registered or
	// reserved; it changes nothing otherwise.
	CreateHarness(rec sqlitelog.HarnessRecord) (sqlitelog.HarnessRecord, error)
	// RetireHarness marks a registration retired so new sessions cannot select it.
	RetireHarness(name, reason string) (sqlitelog.HarnessRecord, error)
	// ReserveHarnessNames permanently reserves names for static harnesses.
	ReserveHarnessNames(names []string) error
	// HarnessNameReserved reports whether a name is reserved for a static harness. HarnessRegistry
	// uses it only as a preflight; CreateHarness checks the reservation again when it writes.
	HarnessNameReserved(name string) (bool, error)
}

var (
	_ SessionStore = (*sqlitelog.Store)(nil)
	_ HarnessStore = (*sqlitelog.Store)(nil)
)

// traceLinkStore is the optional SessionStore half that records each session's latest traced
// operation, so the next one can link to it. *sqlitelog.Store has it. ExchangeTraceParent must read
// and replace in one step, so overlapping operations never share a predecessor.
type traceLinkStore interface {
	ExchangeTraceParent(uid, traceparent string) (prev string, err error)
	SetLastTraceParent(uid, traceparent string) error
}

var _ traceLinkStore = (*sqlitelog.Store)(nil)

// protoReader is the optional journal method Replay prefers (*sqlitelog.Log has it): it returns
// the stored protobuf events, keeping field presence that a domain-event round trip drops.
type protoReader interface {
	ReadProto(fromSeq int64) ([]*v1.LogRecord, error)
}

// readProto returns the records of log with seq >= fromSeq as wire records: the stored protobuf
// events when log implements protoReader, and its domain records converted otherwise.
func readProto(log eventlog.Store, fromSeq int64) ([]*v1.LogRecord, error) {
	if r, ok := log.(protoReader); ok {
		return r.ReadProto(fromSeq)
	}
	recs, err := log.Read(fromSeq)
	if err != nil {
		return nil, err
	}
	out := make([]*v1.LogRecord, 0, len(recs))
	for _, r := range recs {
		out = append(out, eventlog.RecordToProto(r))
	}
	return out, nil
}

// nilStore reports whether store is nil or a nil pointer, so constructors that took a
// *sqlitelog.Store keep refusing a nil one passed as an interface.
func nilStore(store any) bool {
	if store == nil {
		return true
	}
	v := reflect.ValueOf(store)
	return v.Kind() == reflect.Pointer && v.IsNil()
}
