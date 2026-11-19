package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

// A long copy shows its rate and the time left; a short one stays quiet.
func TestCopyProgressShowsRateAndTimeLeft(t *testing.T) {
	var out bytes.Buffer
	at := time.Unix(0, 0)
	p := newCPProgress(&out, "zm")
	p.now = func() time.Time { return at }

	p.Chunk(1, 4)
	if out.Len() != 0 {
		t.Fatalf("a 4-chunk copy printed %q", out.String())
	}
	p.Chunk(1, 100) // the first chunk: nothing to rate yet
	if got := out.String(); !strings.Contains(got, "1/100 chunks") || strings.Contains(got, "MiB/s") {
		t.Fatalf("first chunk printed %q", got)
	}
	out.Reset()
	at = at.Add(2 * time.Second)
	p.Chunk(3, 100) // 2 chunks (1 MiB) in 2 s: 0.5 MiB/s, 97 chunks left at 1 s each
	got := out.String()
	for _, want := range []string{"3/100 chunks", "0.5 MiB/s", "1m37s left"} {
		if !strings.Contains(got, want) {
			t.Errorf("%q lacks %q", got, want)
		}
	}
}

// A copy resumed with chunks already in the box rates only what this run sent.
func TestCopyProgressRatesOnlyWhatThisRunSent(t *testing.T) {
	var out bytes.Buffer
	at := time.Unix(0, 0)
	p := newCPProgress(&out, "zm")
	p.now = func() time.Time { return at }
	p.Chunk(91, 100) // 90 were there already
	out.Reset()
	at = at.Add(time.Second)
	p.Chunk(92, 100) // one chunk (0.5 MiB) in 1 s, not 91
	if got := out.String(); !strings.Contains(got, "0.5 MiB/s") || !strings.Contains(got, "8s left") {
		t.Errorf("rate counted the resumed chunks: %q", got)
	}
}
