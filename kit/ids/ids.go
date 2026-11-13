// Package ids mints ULIDs for every stored object and event. Owned by the
// integrator.
//
// IDs from one daemon are strictly increasing in mint order (monotonic within
// a millisecond) so an event id is a resume cursor across every log, but each
// step adds a RANDOM increment, so the gap between two ids a caller can see
// says nothing about how many ids were minted in between (SPEC "Leaks").
package ids

import (
	"crypto/rand"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"
)

var (
	mu      sync.Mutex
	entropy = ulid.Monotonic(rand.Reader, 1<<40)
	floor   ulid.ULID
)

// New returns a new ULID string (26 chars, Crockford base32, uppercase),
// greater than every id this process minted or saw via Floor.
func New() string {
	mu.Lock()
	defer mu.Unlock()
	now := time.Now()
	id, err := ulid.New(ulid.Timestamp(now), entropy)
	if err != nil {
		// Entropy overflow inside one millisecond: wait for the next one.
		for err != nil {
			time.Sleep(time.Millisecond)
			id, err = ulid.New(ulid.Timestamp(time.Now()), entropy)
		}
	}
	if id.Compare(floor) <= 0 {
		// The clock went backwards (or a stored id is from the future): stay above floor.
		id = next(floor)
	}
	floor = id
	return id.String()
}

// Floor makes every later New greater than s (call at start-up with the
// largest stored id). Invalid input is ignored.
func Floor(s string) {
	id, err := ulid.ParseStrict(s)
	if err != nil {
		return
	}
	mu.Lock()
	if id.Compare(floor) > 0 {
		floor = id
	}
	mu.Unlock()
}

// next is id plus a random 1..2^32 in its entropy, same timestamp.
func next(id ulid.ULID) ulid.ULID {
	var inc [4]byte
	_, _ = io.ReadFull(rand.Reader, inc[:])
	n := uint64(inc[0])<<24 | uint64(inc[1])<<16 | uint64(inc[2])<<8 | uint64(inc[3]) + 1
	out := id
	carry := n
	for i := 15; i >= 6 && carry > 0; i-- {
		s := uint64(out[i]) + carry&0xff
		out[i] = byte(s)
		carry = carry>>8 + s>>8
	}
	return out
}

// Valid reports whether s is a canonical ULID.
func Valid(s string) bool {
	_, err := ulid.ParseStrict(s)
	return err == nil && strings.ToUpper(s) == s
}

// Time is the timestamp inside id (zero for an invalid id).
func Time(s string) time.Time {
	id, err := ulid.ParseStrict(s)
	if err != nil {
		return time.Time{}
	}
	return ulid.Time(id.Time()).UTC()
}
