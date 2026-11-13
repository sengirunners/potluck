package ids

import (
	"testing"
	"time"
)

func TestMonotonicAndValid(t *testing.T) {
	prev := New()
	for i := 0; i < 5000; i++ {
		id := New()
		if id <= prev {
			t.Fatalf("not increasing: %s then %s", prev, id)
		}
		if !Valid(id) {
			t.Fatalf("invalid %s", id)
		}
		prev = id
	}
	if Valid("nope") || Valid("") {
		t.Fatal("Valid accepted junk")
	}
	if d := time.Since(Time(prev)); d < 0 || d > time.Minute {
		t.Fatalf("Time: %v", d)
	}
}

func TestFloorFromFuture(t *testing.T) {
	future := "7ZZZZZZZZZ0000000000000000"
	Floor(future)
	if id := New(); id <= future {
		t.Fatalf("%s not above floor %s", id, future)
	}
}
