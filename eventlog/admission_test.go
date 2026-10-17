package eventlog_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/agent-primitives/sessions/eventlog"
)

type writableLog struct {
	eventlog.Store
	err    error
	checks int
}

func (l *writableLog) CheckWritable() error { l.checks++; return l.err }

type unpublishedLog struct {
	*writableLog
}

func (l unpublishedLog) CheckAdmissible() error {
	return fmt.Errorf("preparing fork: %w", eventlog.ErrUnpublished)
}

func TestCheckAdmissibleRefusesUnpublishedButAllowsTrustedWrites(t *testing.T) {
	log := unpublishedLog{&writableLog{Store: eventlog.AsStore(eventlog.New())}}
	if err := eventlog.CheckAdmissible(log); !errors.Is(err, eventlog.ErrUnpublished) {
		t.Fatalf("admission = %v, want ErrUnpublished", err)
	}
	if log.checks != 0 {
		t.Fatal("admission fell back to builder-permissive CheckWritable")
	}
	if err := eventlog.CheckWritable(log); err != nil || log.checks != 1 {
		t.Fatalf("trusted writes refused: %v checks=%d", err, log.checks)
	}
}

func TestCheckAdmissibleFallsBackToDeletionBarrier(t *testing.T) {
	for _, want := range []error{nil, eventlog.ErrDeleted, errors.New("storage unavailable")} {
		log := &writableLog{Store: eventlog.AsStore(eventlog.New()), err: want}
		if err := eventlog.CheckAdmissible(log); !errors.Is(err, want) || log.checks != 1 {
			t.Fatalf("admission = %v, want %v; checks=%d", err, want, log.checks)
		}
	}
	if err := eventlog.CheckAdmissible(eventlog.AsStore(eventlog.New())); err != nil {
		t.Fatalf("standalone log refused: %v", err)
	}
}
