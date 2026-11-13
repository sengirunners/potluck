package runtarget

import "sync"

// keyLocks is a mutex per key that is forgotten once nobody holds or waits
// for it, for keys without end (request ids).
type keyLocks struct {
	mu sync.Mutex
	m  map[string]*keyLock
}

type keyLock struct {
	sync.Mutex
	n int // holders and waiters
}

// lock takes key's mutex and answers its unlock.
func (k *keyLocks) lock(key string) func() {
	k.mu.Lock()
	if k.m == nil {
		k.m = map[string]*keyLock{}
	}
	l := k.m[key]
	if l == nil {
		l = &keyLock{}
		k.m[key] = l
	}
	l.n++
	k.mu.Unlock()
	l.Lock()
	return func() {
		l.Unlock()
		k.mu.Lock()
		if l.n--; l.n == 0 {
			delete(k.m, key)
		}
		k.mu.Unlock()
	}
}
