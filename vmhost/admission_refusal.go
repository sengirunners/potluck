package vmhost

// The admission controller's last "no room" answer to this state
// directory's runs, kept so describe (and so potluck host status and the
// binding's advert) can say the host is refusing new work and why, which
// the host switch cannot know: the controller decides, and the driver only
// hears it when it asks for a lease (takeRoom). A run's supervisor writes
// the answer when a lease is refused and removes it when a later one is
// granted to a run of this directory; describe shows it until the
// controller's retry time. It is kept per controller, not per size: a
// refused request for one size shows as the host refusing, and a grant of
// any size clears it. A grant to another state directory on the same
// controller does not clear this one. It only reports: every acquire still
// asks the controller, whose answer is the one decision.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/sengirunners/potluck/protocol"
)

// refusalHold is how long an answer with no retry time stands; maxRefusalHold
// bounds a retry time.
const (
	refusalHold    = 2 * time.Minute
	maxRefusalHold = 24 * time.Hour
)

// noRoomSeen is one controller's answer as stored. Since, Due and Pushed
// are the run of refusals it belongs to: when the first of them was given,
// the retry time the run last promised, and how many times that time came
// and a refusal then named a later one.
type noRoomSeen struct {
	Reason string    `json:"reason"`
	RetryS int       `json:"retry_s,omitempty"`
	At     time.Time `json:"at"`
	Since  time.Time `json:"since,omitzero"`
	Due    time.Time `json:"due,omitzero"`
	Pushed int       `json:"pushed,omitempty"`
}

// until is when r stops standing: its retry time (refusalHold with none).
func (r noRoomSeen) until() time.Time {
	hold := refusalHold
	if r.RetryS > 0 {
		hold = time.Duration(min(r.RetryS, int(maxRefusalHold/time.Second))) * time.Second
	}
	return r.At.Add(hold)
}

// A refusal given within runGrace after the last one's retry time is the
// same run of refusals: a caller asks again at the retry time, and the
// answer then is the one that says whether the room came. The promised
// time was pushed back when a refusal asked within pushedBy of it, or
// after, names a time pushedBy or more past it. Refusals asked before it
// (other callers, a retry loop) never count, however much they move the
// time on: one push per promise at most.
const (
	runGrace = 2 * time.Minute
	pushedBy = 5 * time.Second
)

func (s store) refusalFile() string { return filepath.Join(s.dir, "admission-refused.json") }

// readRefusals is the stored answers by admission URL (none when the file
// is missing or unreadable: it is a report, never a gate).
func (s store) readRefusals() map[string]noRoomSeen {
	b, err := os.ReadFile(s.refusalFile())
	if err != nil {
		return nil
	}
	var m map[string]noRoomSeen
	if json.Unmarshal(b, &m) != nil {
		return nil
	}
	return m
}

// changeRefusals applies f to the stored answers under the store's lock.
func (s store) changeRefusals(f func(map[string]noRoomSeen)) {
	_ = flocked(filepath.Join(s.dir, "admission-refused.lock"), func() error {
		m := s.readRefusals()
		if m == nil {
			m = map[string]noRoomSeen{}
		}
		f(m)
		if len(m) == 0 {
			if err := os.Remove(s.refusalFile()); err != nil && !os.IsNotExist(err) {
				return err
			}
			return nil
		}
		b, err := json.Marshal(m)
		if err != nil {
			return err
		}
		tmp, err := os.CreateTemp(s.dir, "admission-refused-*")
		if err != nil {
			return err
		}
		defer os.Remove(tmp.Name())
		_, werr := tmp.Write(b)
		if cerr := tmp.Close(); werr == nil {
			werr = cerr
		}
		if werr != nil {
			return werr
		}
		if err := os.Chmod(tmp.Name(), 0o600); err != nil {
			return err
		}
		return os.Rename(tmp.Name(), s.refusalFile())
	})
}

// recordRefusal keeps the controller at url's "no room" answer.
// A refusal while an earlier one still stands (or within runGrace of its
// retry time) continues that run: its Since stays, and Pushed counts each
// promised retry time that came and was moved later.
func (s store) recordRefusal(url string, e noRoom, now time.Time) {
	s.changeRefusals(func(m map[string]noRoomSeen) {
		r := noRoomSeen{Reason: e.reason, RetryS: max(e.retry, 0), At: now.UTC(), Since: now.UTC()}
		r.Due = r.until()
		if prev, ok := m[url]; ok && !prev.At.After(r.At) && r.At.Before(prev.until().Add(runGrace)) {
			r.Since, r.Due, r.Pushed = prev.Since, prev.Due, max(prev.Pushed, 0)
			if r.Since.IsZero() { // kept by a build without them
				r.Since, r.Due = prev.At, prev.until()
			}
			if !r.At.Before(r.Due.Add(-pushedBy)) && r.until().Sub(r.Due) >= pushedBy {
				r.Pushed++
				r.Due = r.until()
			}
		}
		m[url] = r
	})
}

// clearRefusal drops url's answer when it is older than asked: the
// controller granted a lease asked for then. An answer another run got
// since stands.
func (s store) clearRefusal(url string, asked time.Time) {
	if r, ok := s.readRefusals()[url]; !ok || !r.At.Before(asked) {
		return
	}
	s.changeRefusals(func(m map[string]noRoomSeen) {
		if r, ok := m[url]; ok && r.At.Before(asked) {
			delete(m, url)
		}
	})
}

// refusedNow is url's answer as describe's host block shows it, while it
// stands: until its retry time (refusalHold with none). nil when there is
// none, or no url.
func (s store) refusedNow(url string, now time.Time) *protocol.HostRefusal {
	if url == "" {
		return nil
	}
	r, ok := s.readRefusals()[url]
	if !ok || r.Reason == "" {
		return nil
	}
	until := r.until()
	if !now.Before(until) {
		return nil
	}
	reason := protocol.RefusalReason(noRoom{reason: r.Reason}.Error())
	out := &protocol.HostRefusal{Reason: reason, At: r.At.UTC().Format(time.RFC3339), Until: until.UTC().Format(time.RFC3339), Pushed: max(r.Pushed, 0)}
	if !r.Since.IsZero() && r.Since.Before(r.At.Truncate(time.Second)) {
		out.Since = r.Since.UTC().Format(time.RFC3339)
	}
	if !out.Valid() {
		return nil
	}
	return out
}
