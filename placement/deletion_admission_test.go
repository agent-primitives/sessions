package placement

import (
	"errors"
	"sync"
	"testing"
)

// Shared admission must not release deletion fencing when a concurrent parent operation returns.
func TestDeletionAdmissionRetainsEverySharedHold(t *testing.T) {
	p := New(nil, nil)
	r, err := NewRegistry("echo", map[string]*Placer{"echo": p})
	if err != nil {
		t.Fatal(err)
	}
	for _, order := range []bool{false, true} {
		first, err := p.guard.tryReadLock("parent")
		if err != nil {
			t.Fatal(err)
		}
		second, err := p.guard.tryReadLock("parent")
		if err != nil {
			t.Fatal(err)
		}
		turn, err := p.trySessionLock("parent")
		if err != nil {
			t.Fatalf("shared hold refused parent operation: %v", err)
		}
		turn()
		if order {
			first()
		} else {
			second()
		}
		called := false
		if err := r.WithSessionGuard("parent", func() error { called = true; return nil }); !errors.Is(err, ErrSessionBusy) || called {
			t.Fatalf("Delete crossed remaining shared hold: %v callback=%v", err, called)
		}
		if order {
			second()
		} else {
			first()
		}
		if err := r.WithSessionGuard("parent", func() error {
			if err := r.WithSessionRead("parent", func() error { called = true; return nil }); !errors.Is(err, ErrSessionBusy) {
				t.Errorf("routing read crossed Delete: %v", err)
			}
			if _, err := p.trySessionLock("parent"); !errors.Is(err, ErrSessionBusy) {
				t.Errorf("parent operation crossed Delete: %v", err)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if called || guardEntries(p) != 0 {
			t.Fatal("callback ran or shared admission leaked")
		}
	}
}

func TestDeletionAdmissionConcurrentReadersAndDeleteDrain(t *testing.T) {
	p := New(nil, nil)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 1000 {
				release, err := p.guard.tryReadLock("parent")
				if err == nil {
					release()
				} else if !errors.Is(err, ErrSessionBusy) {
					t.Error(err)
				}
				release, err = p.guard.tryDeleteLock("parent")
				if err == nil {
					release()
				} else if !errors.Is(err, ErrSessionBusy) {
					t.Error(err)
				}
			}
		})
	}
	wg.Wait()
	if guardEntries(p) != 0 {
		t.Fatal("concurrent admissions leaked entries")
	}
}
