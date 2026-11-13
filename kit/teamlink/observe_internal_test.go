package teamlink

import "testing"

// A header older than what the replica holds is ignored, unless it corrects
// what its own request sent (an authority restored from a backup).
func TestStaleHeader(t *testing.T) {
	for _, c := range []struct {
		name                string
		version, held, sent int64
		want                bool
	}{
		{"current", 3, 3, 3, false},
		{"newer, the rescope's correction", 3, 2, 2, false},
		{"a long poll begun before the change", 2, 3, 2, true},
		{"a correction older than what is held", 3, 4, 2, true},
		{"nothing sent, older", 2, 3, 0, true},
		{"nothing sent, first header", 2, 0, 0, false},
		{"restored authority: lower than sent", 2, 5, 5, false},
	} {
		if got := staleHeader(c.version, c.held, c.sent); got != c.want {
			t.Errorf("%s: stale %v, want %v", c.name, got, c.want)
		}
	}
}

// One rule over both orders: the authority's version, then the send order.
func TestTakeHeader(t *testing.T) {
	for _, c := range []struct {
		name                string
		version, held, sent int64
		seq, headerSeq      uint64
		want                bool
	}{
		{"later call, same version (rules changed)", 3, 3, 3, 8, 7, true},
		{"earlier call, same version (a long poll's old rules)", 3, 3, 3, 6, 7, false},
		{"later call, newer version", 4, 3, 3, 8, 7, true},
		{"earlier call, newer version (a 409's correction)", 4, 3, 3, 6, 7, true},
		{"later call, older version (the inversion)", 2, 3, 2, 8, 7, false},
		{"earlier call, older version (a long poll)", 2, 3, 2, 6, 7, false},
		{"later call, lower than it sent (restored authority)", 2, 5, 5, 8, 7, true},
		{"earlier call, lower than it sent", 2, 5, 5, 6, 7, false},
	} {
		if got := takeHeader(c.version, c.held, c.sent, c.seq, c.headerSeq); got != c.want {
			t.Errorf("%s: take %v, want %v", c.name, got, c.want)
		}
	}
}
