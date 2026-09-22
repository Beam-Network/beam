package roomworkload

import "sync"

// KeyedLocks lets independent durable workloads advance concurrently while
// serializing mutations of one workload. Entries are removed after the last
// holder or waiter leaves, so completed workloads do not retain lock state.
type KeyedLocks struct {
	mu      sync.Mutex
	entries map[string]*keyedLock
}

type keyedLock struct {
	mu   sync.Mutex
	refs int
}

func (l *KeyedLocks) Lock(key string) func() {
	l.mu.Lock()
	if l.entries == nil {
		l.entries = make(map[string]*keyedLock)
	}
	entry := l.entries[key]
	if entry == nil {
		entry = &keyedLock{}
		l.entries[key] = entry
	}
	entry.refs++
	l.mu.Unlock()

	entry.mu.Lock()
	return func() {
		entry.mu.Unlock()
		l.mu.Lock()
		entry.refs--
		if entry.refs == 0 {
			delete(l.entries, key)
		}
		l.mu.Unlock()
	}
}
