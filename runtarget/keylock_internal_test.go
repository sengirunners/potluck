package runtarget

import (
	"sync"
	"testing"
	"time"
)

// TestKeyLocks: one key is held by one caller at a time, other keys are
// not held up, and a key nobody holds is forgotten.
func TestKeyLocks(t *testing.T) {
	var k keyLocks
	un := k.lock("a")
	got := make(chan struct{})
	go func() { k.lock("a")(); close(got) }()
	k.lock("b")() // another key: not held up
	select {
	case <-got:
		t.Fatal("a second holder of a while the first held it")
	case <-time.After(50 * time.Millisecond):
	}
	un()
	<-got
	var wg sync.WaitGroup
	for range 50 {
		wg.Add(1)
		go func() { defer wg.Done(); k.lock("c")() }()
	}
	wg.Wait()
	if n := len(k.m); n != 0 {
		t.Errorf("%d keys kept after every holder left", n)
	}
}
