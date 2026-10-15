package ctxlock

import (
	"context"
	"testing"
	"testing/synctest"
	"time"
)

func TestLockAcquiredDuringCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var m Mutex
		if err := m.Lock(t.Context()); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		result := make(chan error, 1)
		go func() { result <- m.Lock(ctx) }()
		synctest.Wait() // The waiter is blocked on the held mutex.

		m.Unlock() // Commit the waiter's acquisition before canceling its context.
		cancel()
		synctest.Wait()
		if err := <-result; err != nil {
			t.Fatalf("acquired lock = %v, want nil", err)
		}
		m.Unlock()
	})
}

func TestUnlockUnlockedPanics(t *testing.T) {
	for _, initialized := range []bool{false, true} {
		name := "zero value"
		if initialized {
			name = "already unlocked"
		}
		t.Run(name, func(t *testing.T) {
			var m Mutex
			if initialized {
				if err := m.Lock(t.Context()); err != nil {
					t.Fatal(err)
				}
				m.Unlock()
			}
			panicked := make(chan bool, 1)
			go func() {
				defer func() { panicked <- recover() != nil }()
				m.Unlock()
			}()
			select {
			case ok := <-panicked:
				if !ok {
					t.Error("Unlock on an unlocked mutex did not panic")
				}
			case <-time.After(time.Second):
				t.Error("Unlock on an unlocked mutex blocked instead of panicking")
			}
		})
	}
}
