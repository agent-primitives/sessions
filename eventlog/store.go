package eventlog

import "github.com/agent-primitives/sessions/api"

// Store is the error-returning event-log contract the controller depends on, so it can run
// against either the in-memory Log (via AsStore) or the persistent sqlitelog.Log interchangeably.
// Every operation can fail because a durable backend does I/O; the in-memory adapter simply never
// returns an error. The unified shape is the error-returning one because the controller must
// handle read/head/fence failures from the persistent backend.
type Store interface {
	// Append commits ev as the next record iff expectedLastSeq == head (single-writer CAS) and
	// fence is current.
	Append(expectedLastSeq, fence int64, ev api.Event) (Record, error)
	// Head returns the seq of the last committed record (0 if empty).
	Head() (int64, error)
	// NewFence advances and returns the incarnation fencing token.
	NewFence() (int64, error)
	// Read returns all records with seq >= fromSeq, in order.
	Read(fromSeq int64) ([]Record, error)
	// Verify walks the chain and reports the first integrity violation.
	Verify() error
}

// SessionJournal is a Store for one session of a store that supports deletion, with the deletion
// preflight required rather than optional. CheckWritable is read-only: it returns an error
// wrapping ErrDeleted once the session's deletion has committed, nil for a session that is
// writable or has no record yet, and any other error for a failed check, which the caller treats
// as a refusal. The host runs it before compute work, so a wrapper forwards it to the journal it
// wraps; Append and NewFence still enforce the authoritative storage barrier.
type SessionJournal interface {
	Store
	CheckWritable() error
}

// CheckWritable performs the durable-deletion preflight for a Store that has one, as every
// SessionJournal does. A Store without it (the in-memory log, which has no deletion) is always
// writable. Wrappers around durable stores must forward CheckWritable to preserve refusal before
// compute work; Append and NewFence still enforce the authoritative storage barrier.
func CheckWritable(log Store) error {
	if writable, ok := log.(interface{ CheckWritable() error }); ok {
		return writable.CheckWritable()
	}
	return nil
}

// AsStore adapts the in-memory Log (whose reads cannot fail) to the error-returning Store.
func AsStore(l *Log) Store { return storeAdapter{l} }

type storeAdapter struct{ l *Log }

func (a storeAdapter) Append(expectedLastSeq, fence int64, ev api.Event) (Record, error) {
	return a.l.Append(expectedLastSeq, fence, ev)
}
func (a storeAdapter) Head() (int64, error)                 { return a.l.Head(), nil }
func (a storeAdapter) NewFence() (int64, error)             { return a.l.NewFence(), nil }
func (a storeAdapter) Read(fromSeq int64) ([]Record, error) { return a.l.Read(fromSeq), nil }
func (a storeAdapter) Verify() error                        { return a.l.Verify() }

var _ Store = storeAdapter{}
