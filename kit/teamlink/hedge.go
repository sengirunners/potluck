package teamlink

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sengirunners/potluck/kit/core"
)

// Hedged reads (SPEC 17.3 "Forwarded reads"). A read forwarded to the
// authority can sit on one connection that has stopped moving (a lost
// packet in TCP's retransmission backoff over a home uplink, or a dead
// pooled connection) while a new connection would answer at once: measured
// on a real replica, one read in about 150 waited 2.7 s for its first byte
// while the five reads started during that wait answered in 0.13 s each. So
// a read that has heard nothing for HedgeAfter is asked again on a new
// connection, at most MaxHedges times, and the first whole answer wins; the
// others are cancelled. Writes are never sent twice; a route that thinks
// before it answers waits longer (RouteHook.Thinks). Which routes only read
// is each route's row (routes.go).
var (
	// HedgeAfter: how long every attempt of a read may be silent (no answer
	// byte) before another goes out on a new connection. Several times a
	// healthy read's time to first byte over a home uplink (p99 0.43 s).
	HedgeAfter = time.Second
	// MaxHedges: extra attempts per read.
	MaxHedges = 2
	// hedgeReadAhead: an answer is read whole before it is relayed, up to
	// this much (a read can still be hedged while nothing was relayed). A
	// longer one that is still arriving is relayed as it comes, unhedged
	// from then on: it is moving.
	hedgeReadAhead = 1 << 20
)

// sendForward is send for a call forwarded from this machine's socket: a
// read is hedged.
func (s *Service) sendForward(ctx context.Context, x *Remote, req *http.Request, body []byte) (*http.Response, error) {
	if MaxHedges <= 0 || !s.Hedgeable(req) {
		return s.Send(ctx, x, req, body)
	}
	row, _ := s.rule(req)
	return s.hedged(ctx, x, req, body, row.hook().Thinks)
}

// fresh is x's client that never reuses a connection (a hedge must not go
// out on another idle connection that is just as stuck).
func fresh(x *Remote) *http.Client {
	x.Mu.Lock()
	defer x.Mu.Unlock()
	if x.FreshHC == nil {
		if nr, ok := x.HC.Transport.(interface{ NoReuse() http.RoundTripper }); ok {
			x.FreshHC = &http.Client{Transport: nr.NoReuse()}
		} else if t, ok := x.HC.Transport.(*http.Transport); ok {
			t = t.Clone()
			t.DisableKeepAlives = true
			x.FreshHC = &http.Client{Transport: t}
		} else {
			x.FreshHC = x.HC
		}
	}
	return x.FreshHC
}

// hedgeResult is one attempt's outcome: an answer read whole (or the first
// hedgeReadAhead of a longer one still arriving), or an error.
type hedgeResult struct {
	n      int
	resp   *http.Response
	err    error
	cancel context.CancelFunc
}

func (s *Service) hedged(ctx context.Context, x *Remote, req *http.Request, body []byte, thinks int) (*http.Response, error) {
	maxHedges := MaxHedges
	results := make(chan hedgeResult, maxHedges+1)
	var heard atomic.Int64 // UnixNano of the last byte (or attempt start)
	heard.Store(time.Now().UnixNano())
	// A row may think for seconds before its first byte (RouteHook.Thinks):
	// until its answer starts it waits that many times longer.
	var answering atomic.Bool
	progress := core.ReadProgress(ctx)
	var mu sync.Mutex
	var cancels []context.CancelFunc
	start := func(n int, hc *http.Client) {
		actx, cancel := context.WithCancel(ctx)
		// A transport that reads the answer before handing it over (the
		// tunnel's) shows it moving here.
		actx = core.WithReadProgress(actx, func() {
			heard.Store(time.Now().UnixNano())
			if progress != nil {
				progress()
			}
		})
		mu.Lock()
		cancels = append(cancels, cancel)
		mu.Unlock()
		heard.Store(time.Now().UnixNano())
		out := req.Clone(actx)
		out.Header = req.Header.Clone()
		out.Body = io.NopCloser(bytes.NewReader(body))
		go func() {
			resp, err := s.SendWith(actx, x, hc, out, body)
			if err != nil {
				results <- hedgeResult{n: n, err: err, cancel: cancel}
				return
			}
			heard.Store(time.Now().UnixNano())
			answering.Store(true)
			var buf bytes.Buffer
			_, err = io.Copy(&buf, io.LimitReader(heardReader{resp.Body, &heard, progress}, int64(hedgeReadAhead)))
			if err != nil {
				resp.Body.Close()
				results <- hedgeResult{n: n, err: err, cancel: cancel}
				return
			}
			rest := resp.Body
			if buf.Len() < hedgeReadAhead {
				resp.Body.Close() // whole
				rest = nil
			}
			resp.Body = &hedgedBody{buf: buf.Bytes(), rest: rest, cancel: cancel}
			results <- hedgeResult{n: n, resp: resp, cancel: cancel}
		}()
	}
	s.hedgeB.read(time.Now())
	start(0, x.Client())
	live, hedges := 1, 0
	var firstErr error
	t := time.NewTimer(HedgeAfter)
	defer t.Stop()
	finish := func(win int) {
		mu.Lock()
		defer mu.Unlock()
		for i, c := range cancels {
			if i != win {
				c()
			}
		}
	}
	for {
		select {
		case r := <-results:
			live--
			if r.err == nil {
				finish(r.n)
				go drainLosers(results, live)
				if r.n > 0 {
					s.hedgeB.won.Add(1)
					s.d.Log.Debug("teamlink: hedged read answered", "path", req.URL.Path, "attempt", r.n)
				}
				return r.resp, nil
			}
			r.cancel()
			if firstErr == nil {
				firstErr = r.err
			}
			if live == 0 {
				finish(-1)
				return nil, firstErr
			}
		case <-t.C:
			silent, wait := time.Since(time.Unix(0, heard.Load())), HedgeAfter
			if thinks > 0 && !answering.Load() {
				wait = time.Duration(thinks) * HedgeAfter
			}
			if silent < wait {
				t.Reset(wait - silent)
				continue
			}
			if hedges < maxHedges && ctx.Err() == nil && s.hedgeB.take(time.Now()) {
				hedges++
				live++
				s.d.Log.Debug("teamlink: read silent, asking again on a new connection", "path", req.URL.Path, "silent", silent, "hedge", hedges)
				start(hedges, fresh(x))
			}
			t.Reset(HedgeAfter)
		case <-ctx.Done():
			finish(-1)
			go drainLosers(results, live)
			if firstErr != nil {
				return nil, firstErr
			}
			return nil, s.Unreachable(x.Snapshot().Via, req.URL.Host, ctx.Err())
		}
	}
}

// drainLosers closes the answers of attempts that lost (cancelled, they end
// soon).
func drainLosers(results <-chan hedgeResult, n int) {
	for ; n > 0; n-- {
		if r := <-results; r.resp != nil {
			r.resp.Body.Close()
		}
	}
}

// heardReader stamps every read that brings bytes.
type heardReader struct {
	r        io.Reader
	heard    *atomic.Int64
	progress func()
}

func (h heardReader) Read(p []byte) (int, error) {
	n, err := h.r.Read(p)
	if n > 0 {
		h.heard.Store(time.Now().UnixNano())
		if h.progress != nil {
			h.progress()
		}
	}
	return n, err
}

// hedgedBody is the winner's answer: what was read ahead, then (for a long
// answer) the rest as it arrives.
type hedgedBody struct {
	buf    []byte
	rest   io.ReadCloser
	cancel context.CancelFunc
	once   sync.Once
}

func (b *hedgedBody) Read(p []byte) (int, error) {
	if len(b.buf) > 0 {
		n := copy(p, b.buf)
		b.buf = b.buf[n:]
		return n, nil
	}
	if b.rest == nil {
		return 0, io.EOF
	}
	return b.rest.Read(p)
}

func (b *hedgedBody) Close() error {
	var err error
	b.once.Do(func() {
		if b.rest != nil {
			err = b.rest.Close()
		}
		b.cancel()
	})
	return err
}
