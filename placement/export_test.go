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

// StartedTurns exposes the started-turn count Exec offers to Create.
func (p *Placer) StartedTurns(log eventlog.Store, current string) (int, error) {
	return p.startedTurns(log, current)
}
