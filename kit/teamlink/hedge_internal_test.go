package teamlink

import (
	"testing"
	"time"
)

// Hedges are at most HedgeShare of the last minute's reads, at least
// HedgeFloor, and the window slides.
func TestHedgeBudget(t *testing.T) {
	var b hedgeBudget
	t0 := time.Unix(1_000_000, 0)
	for i := 0; i < HedgeFloor; i++ {
		if !b.take(t0) {
			t.Fatalf("hedge %d refused with no reads (floor %d)", i+1, HedgeFloor)
		}
	}
	if b.take(t0) {
		t.Fatal("a hedge over the floor with no reads")
	}
	for i := 0; i < 100; i++ {
		b.read(t0.Add(time.Duration(i) * 100 * time.Millisecond))
	}
	now := t0.Add(10 * time.Second)
	n := 0
	for b.take(now) {
		n++
	}
	if total := HedgeFloor + n; total != 10 {
		t.Fatalf("%d hedges for 100 reads, want 10", total)
	}
	if b.refused.Load() != 2 {
		t.Fatalf("refused %d, want 2", b.refused.Load())
	}
	// A minute later the window is empty: the floor again.
	later := now.Add(61 * time.Second)
	for i := 0; i < HedgeFloor; i++ {
		if !b.take(later) {
			t.Fatalf("hedge %d refused a minute later", i+1)
		}
	}
}
