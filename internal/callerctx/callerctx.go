// Package callerctx carries, inside this module, the caller's context state at the moment a
// runtime operation failed.
//
// A host classifies a failed operation as a caller interruption when the caller's context has
// ended. A runtime that runs its own teardown before it returns would let a deadline that lapses
// during that teardown replace the primary failure, so it marks a failure the caller was still
// waiting for before the teardown started.
package callerctx

import "errors"

type liveAtFailure struct{ err error }

func (e *liveAtFailure) Error() string { return e.err.Error() }
func (e *liveAtFailure) Unwrap() error { return e.err }

// MarkLive records that err ended an operation while the caller's context was still live. The
// text and errors.Is/As chain of err are unchanged. A nil err stays nil.
func MarkLive(err error) error {
	if err == nil {
		return nil
	}
	return &liveAtFailure{err: err}
}

// LiveAtFailure reports whether err carries MarkLive's mark.
func LiveAtFailure(err error) bool {
	var live *liveAtFailure
	return errors.As(err, &live)
}
