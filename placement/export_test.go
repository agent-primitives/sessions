package placement

import (
	"testing"
	"time"

	"github.com/aramase/agentsessions/eventlog"
)

// SetDescribeTimeout shortens how long admission waits for Describe, for the duration of a test.
func SetDescribeTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	old := describeTimeout
	describeTimeout = d
	t.Cleanup(func() { describeTimeout = old })
}

// CommittedTurns exposes the committed-turn count Exec passes to Create.
func (p *Placer) CommittedTurns(log eventlog.Store, current string) (int, error) {
	return p.committedTurns(log, current)
}
