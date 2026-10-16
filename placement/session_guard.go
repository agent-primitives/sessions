package placement

import "sync"

// sessionGuard preserves exclusive turn/lifecycle admission while separately fencing deletion.
// Fork and routing reads hold shared deletion admission without blocking an ordinary operation:
// a stateful fork may supersede a running turn, and rollback releases the parent for new turns.
// Every admission and release shares mu; entries disappear only after the last hold is released.
type sessionGuard struct {
	mu      sync.Mutex
	entries map[string]*sessionGuardEntry
}

type sessionGuardEntry struct {
	operation bool
	readers   int
	deleting  bool
}

type guardKind uint8

const (
	guardOperation guardKind = iota
	guardRead
	guardDelete
)

func (g *sessionGuard) tryLock(uid string) (func(), error) {
	return g.acquire(uid, guardOperation)
}

func (g *sessionGuard) tryReadLock(uid string) (func(), error) {
	return g.acquire(uid, guardRead)
}

func (g *sessionGuard) tryDeleteLock(uid string) (func(), error) {
	return g.acquire(uid, guardDelete)
}

func (g *sessionGuard) acquire(uid string, kind guardKind) (func(), error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	entry := g.entries[uid]
	if entry == nil {
		entry = new(sessionGuardEntry)
		if g.entries == nil {
			g.entries = make(map[string]*sessionGuardEntry)
		}
		g.entries[uid] = entry
	}
	if entry.deleting || (kind == guardOperation && entry.operation) ||
		(kind == guardDelete && (entry.operation || entry.readers > 0)) {
		return nil, ErrSessionBusy
	}
	switch kind {
	case guardOperation:
		entry.operation = true
	case guardRead:
		entry.readers++
	case guardDelete:
		entry.deleting = true
	}
	return func() {
		g.mu.Lock()
		defer g.mu.Unlock()
		switch kind {
		case guardOperation:
			entry.operation = false
		case guardRead:
			entry.readers--
		case guardDelete:
			entry.deleting = false
		}
		if !entry.operation && entry.readers == 0 && !entry.deleting {
			delete(g.entries, uid)
		}
	}, nil
}

// busy reports whether any session holds operation, read or deletion admission.
func (g *sessionGuard) busy() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.entries) > 0
}
