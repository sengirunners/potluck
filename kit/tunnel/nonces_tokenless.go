package tunnel

import (
	"sync"
	"time"
)

// Limits on the nonces of tokenless POSTs (the GitHub join's, SPEC 22 over
// https): per client address (core.AddrBucket) first, then in all as a last
// resort. An entry lives until its date plus MaxSkew, the date clamped to
// now plus MaxSkew (a request passes the skew check only within MaxSkew, so
// nothing outlasts 2xMaxSkew); a restart forgets them (a replayed start is
// then still bound by its limits, and a collect needs the secret).
const (
	MaxTokenlessNonces        = 4096
	MaxTokenlessNoncesPerAddr = 64
)

// One map holds every nonce (key+nonce -> entry), so a replay is found
// whatever address it comes from; the address buckets only count capacity.
type tokenlessNonces struct {
	mu     sync.Mutex
	seen   map[string]tokenlessEntry
	perAdr map[string]int // address bucket -> live entries it added
}

type tokenlessEntry struct {
	exp  int64 // Unix seconds
	addr string
}

// add records (key, nonce) of a request dated date from addr: nonceSeen
// for a replay (from any address), nonceFull when addr's cap or the global
// one is reached, else nonceNew.
func (c *tokenlessNonces) add(addr, key, nonce string, date int64, now time.Time) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.seen == nil {
		c.seen, c.perAdr = map[string]tokenlessEntry{}, map[string]int{}
	}
	id := key + " " + nonce
	if e, ok := c.seen[id]; ok && e.exp >= now.Unix() {
		return nonceSeen
	}
	if c.perAdr[addr] >= MaxTokenlessNoncesPerAddr || len(c.seen) >= MaxTokenlessNonces {
		c.pruneLocked(now)
		if c.perAdr[addr] >= MaxTokenlessNoncesPerAddr || len(c.seen) >= MaxTokenlessNonces {
			return nonceFull
		}
	}
	if old, ok := c.seen[id]; ok { // an expired entry of this id
		c.drop(id, old)
	}
	date = min(date, now.Add(MaxSkew).Unix())
	c.seen[id] = tokenlessEntry{exp: date + int64(MaxSkew/time.Second), addr: addr}
	c.perAdr[addr]++
	return nonceNew
}

func (c *tokenlessNonces) drop(id string, e tokenlessEntry) {
	delete(c.seen, id)
	if c.perAdr[e.addr]--; c.perAdr[e.addr] <= 0 {
		delete(c.perAdr, e.addr)
	}
}

func (c *tokenlessNonces) pruneLocked(now time.Time) {
	for id, e := range c.seen {
		if e.exp < now.Unix() {
			c.drop(id, e)
		}
	}
}
