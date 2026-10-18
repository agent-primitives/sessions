package controller

import (
	"errors"
	"fmt"
	"sync"

	"github.com/agent-primitives/sessions/eventlog"
)

// ErrJournalTampered refuses a journal whose hash chain fails verification (CHAIN-12). Exec,
// Replay, Resume and Outputs return it before they run a harness, call a model or tool, serve a
// recorded effect or append. The returned error also wraps eventlog.ErrTampered (which is
// sqlitelog.ErrTampered) and names the first bad seq.
//
// The chain is unkeyed, so this catches storage corruption and edits that do not recompute the
// hashes. A writer that rewrites events and recomputes every later hash, or removes records from
// the end of the journal, is not detected.
var ErrJournalTampered = errors.New("controller: refusing a journal that failed hash-chain verification")

// journalCheck remembers how much of the journal this controller has verified, so each read
// verifies only the records appended since: the first read is O(n) canonicalizations, later reads
// O(new records). The last verified record must still be at the same position with the same
// content_hash, which also catches a journal that shrank or was replaced below that point. A record
// below the verified head whose event changes later in this controller's life, with its stored
// hashes kept, is not re-checked.
type journalCheck struct {
	mu   sync.Mutex
	n    int // records verified, counted from the first one Read(1) returns
	seq  int64
	hash string
}

// readJournal reads the whole journal and verifies its chain before returning it.
func (c *Controller) readJournal() ([]eventlog.Record, error) {
	recs, err := c.log.Read(1)
	if err != nil {
		return nil, err
	}
	if err := c.journal.verify(recs); err != nil {
		return nil, err
	}
	return recs, nil
}

func (j *journalCheck) verify(recs []eventlog.Record) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if len(recs) < j.n {
		return fmt.Errorf("%w: %w: journal has %d records, %d were verified", ErrJournalTampered,
			eventlog.ErrTampered, len(recs), j.n)
	}
	if j.n > 0 {
		if last := recs[j.n-1]; last.Seq != j.seq || last.Hash != j.hash {
			return fmt.Errorf("%w: %w: verified record at seq %d changed", ErrJournalTampered,
				eventlog.ErrTampered, j.seq)
		}
	}
	if len(recs) == j.n {
		return nil
	}
	if err := verifyRecords(j.hash, recs[j.n:]); err != nil {
		return err
	}
	last := recs[len(recs)-1]
	j.n, j.seq, j.hash = len(recs), last.Seq, last.Hash
	return nil
}

// verifyRecords is eventlog.VerifyChain with a tamper reported as ErrJournalTampered. A
// canonicalization failure is an internal fault, not evidence of tampering, and keeps its own
// error.
func verifyRecords(prevHash string, recs []eventlog.Record) error {
	err := eventlog.VerifyChain(prevHash, recs)
	if errors.Is(err, eventlog.ErrTampered) {
		return fmt.Errorf("%w: %w", ErrJournalTampered, err)
	}
	return err
}
