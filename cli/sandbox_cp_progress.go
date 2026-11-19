package cli

import (
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/sengirunners/potluck/boxcp"
)

// cpProgress prints a copy's progress on one line: the chunks landed, the
// rate since the first one and the time left at that rate, so a copy that
// takes minutes reads as working. Chunks land from several goroutines.
type cpProgress struct {
	w     io.Writer
	name  string
	now   func() time.Time
	mu    sync.Mutex
	start time.Time
	base  int // chunks already in the box when the first one landed (a resumed copy's)
}

func newCPProgress(w io.Writer, name string) *cpProgress {
	return &cpProgress{w: w, name: name, now: time.Now}
}

// Chunk is boxcp.Options.Progress: done of total chunks are in the box.
// Copies of a few chunks print nothing.
func (p *cpProgress) Chunk(done, total int) {
	if total <= 4 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	if p.start.IsZero() {
		p.start, p.base = now, done
		fmt.Fprintf(p.w, "\r%s: %d/%d chunks", p.name, done, total)
		return
	}
	line := fmt.Sprintf("\r%s: %d/%d chunks", p.name, done, total)
	if el := now.Sub(p.start); done > p.base && el > 0 {
		perChunk := el / time.Duration(done-p.base)
		line += fmt.Sprintf(", %.1f MiB/s, %s left", float64(boxcp.ChunkSize)/(1<<20)/perChunk.Seconds(), (perChunk * time.Duration(total-done)).Round(time.Second))
	}
	fmt.Fprintf(p.w, "%-64s", line) // padded: the line is shorter when the time left loses a digit
}

// cpTook is how long a copy took, for the closing line ("" when it was too
// quick to say). No rate: a resumed copy's bytes are not all from this run.
func cpTook(el time.Duration) string {
	if el < 2*time.Second {
		return ""
	}
	return " in " + el.Round(time.Second).String()
}
