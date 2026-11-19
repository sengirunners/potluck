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

// noRoomSeen is one controller's answer as stored.
type noRoomSeen struct {
	Reason string    `json:"reason"`
	RetryS int       `json:"retry_s,omitempty"`
	At     time.Time `json:"at"`
}

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
func (s store) recordRefusal(url string, e noRoom, now time.Time) {
	s.changeRefusals(func(m map[string]noRoomSeen) {
		m[url] = noRoomSeen{Reason: e.reason, RetryS: max(e.retry, 0), At: now.UTC()}
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
	hold := refusalHold
	if r.RetryS > 0 {
		hold = time.Duration(min(r.RetryS, int(maxRefusalHold/time.Second))) * time.Second
	}
	until := r.At.Add(hold)
	if !now.Before(until) {
		return nil
	}
	reason := protocol.RefusalReason(noRoom{reason: r.Reason}.Error())
	out := &protocol.HostRefusal{Reason: reason, At: r.At.UTC().Format(time.RFC3339), Until: until.UTC().Format(time.RFC3339)}
	if !out.Valid() {
		return nil
	}
	return out
}
