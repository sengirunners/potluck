package teamlink

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/sengirunners/potluck/kit/api"
)

// The hedge budget (SPEC 17.3): hedges are at most HedgeShare of the hedged
// routes' reads over the last minute (at least HedgeFloor), so an authority
// that is slow for everyone gets about 10% more reads, not three times as
// many.
var (
	HedgeShare = 0.10
	HedgeFloor = 3 // hedges a minute always allowed
)

const hedgeWindow = 60 // seconds, one bucket each

type hedgeBucket struct {
	sec                   int64
	reads, hedges, refuse int
}

// hedgeBudget counts reads and hedges per second over hedgeWindow, plus
// totals since start (GET /v0/health on the socket).
type hedgeBudget struct {
	mu                          sync.Mutex
	buckets                     [hedgeWindow]hedgeBucket
	reads, hedges, won, refused atomic.Int64
}

func (b *hedgeBudget) bucket(now time.Time) *hedgeBucket {
	sec := now.Unix()
	k := &b.buckets[sec%hedgeWindow]
	if k.sec != sec {
		*k = hedgeBucket{sec: sec}
	}
	return k
}

// window sums the last minute (caller holds mu).
func (b *hedgeBudget) window(now time.Time) (reads, hedges, refused int) {
	from := now.Unix() - hedgeWindow
	for _, k := range b.buckets {
		if k.sec > from {
			reads, hedges, refused = reads+k.reads, hedges+k.hedges, refused+k.refuse
		}
	}
	return
}

// read counts one hedgeable read.
func (b *hedgeBudget) read(now time.Time) {
	b.reads.Add(1)
	b.mu.Lock()
	b.bucket(now).reads++
	b.mu.Unlock()
}

// take reports whether a hedge may go out now, and counts it either way.
func (b *hedgeBudget) take(now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	reads, hedges, _ := b.window(now)
	if float64(hedges+1) > max(float64(HedgeFloor), HedgeShare*float64(reads)) {
		b.bucket(now).refuse++
		b.refused.Add(1)
		return false
	}
	b.bucket(now).hedges++
	b.hedges.Add(1)
	return true
}

// HedgeStats reports the budget's counters (core.HedgeReporter).
func (s *Service) HedgeStats() api.HedgeStats {
	b := &s.hedgeB
	b.mu.Lock()
	reads, hedges, refused := b.window(time.Now())
	b.mu.Unlock()
	return api.HedgeStats{Reads1m: reads, Hedges1m: hedges, Refused1m: refused,
		Reads: b.reads.Load(), Hedges: b.hedges.Load(), Won: b.won.Load(), Refused: b.refused.Load()}
}
