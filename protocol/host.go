package protocol

import (
	"strings"
	"time"
)

// HostStatus is describe's `host`: the host switch as a driver that has one
// reads it now (potluck-driver-tart's host override, vmhost/hostmode.go).
// Mode is the override in force: open, drain, closed, auto, or unreadable.
// State is what that means for new work: accepting, holding, draining or
// closed. Until is when the override returns to auto. During a drain,
// DrainSince is when it began, DrainDeadline when running work is cut, and
// DrainCut how many boxes it has cut so far (a driver's describe; nil from a
// reader with no boxes, vmhost.HostNow). Refused, when set, is a refusal of
// new work the switch does not show: the host's admission controller said
// it has no room. Times are RFC 3339 UTC.
type HostStatus struct {
	Mode          string       `json:"mode"`
	State         string       `json:"state"`
	Until         string       `json:"until,omitempty"`
	DrainSince    string       `json:"drain_since,omitempty"`
	DrainDeadline string       `json:"drain_deadline,omitempty"`
	DrainCut      *int         `json:"drain_cut,omitempty"`
	Refused       *HostRefusal `json:"refused,omitempty"`
}

// HostRefusal is the host's admission controller's last "no room" answer
// (a driver's admission_url; potluck-driver-tart's vmhost/admission.go),
// while it stands: Reason is the whole answer as acquire gave it ("no room
// on this host now: ..."), At when it was given, Until when it stops
// standing (the controller's retry time) unless a later lease was granted
// first. Every acquire still asks the controller; this only reports. The
// reason is the controller's own words, which the host's owner set up; it
// goes with the advert to the pools the host shares with, as the switch's
// state does.
//
// Since and Pushed say how long it has gone on: Since is when the first of
// an unbroken run of refusals was given (unset: At, or a host on a build
// without it), Pushed how many times the run's promised retry time came and
// a refusal then named a later one (asks before it never count). A Pushed
// that keeps growing means "not while that work lasts", not "a few
// minutes". Both are additions: a reader on an older build ignores them.
type HostRefusal struct {
	Reason string `json:"reason"`
	At     string `json:"at"`
	Until  string `json:"until"`
	Since  string `json:"since,omitempty"`
	Pushed int    `json:"pushed,omitempty"`
}

// MaxRefusalReason bounds a refusal's reason, in bytes.
const MaxRefusalReason = 400

// Valid says whether r is a refusal a reader may show: a reason of plain
// text within MaxRefusalReason (a controller's words, carried to other
// machines), both times RFC 3339 UTC to the second, Until not before At,
// and Since, when set, of the same form and not after At; Pushed not below 0.
func (r HostRefusal) Valid() bool {
	if r.Reason == "" || len(r.Reason) > MaxRefusalReason || !plainText(r.Reason) || r.Pushed < 0 {
		return false
	}
	since := r.Since
	if since == "" {
		since = r.At
	}
	var ts [3]time.Time
	for i, t := range []string{r.At, r.Until, since} {
		v, err := time.Parse(time.RFC3339, t)
		if err != nil || t != v.UTC().Format(time.RFC3339) { // UTC to the second, so text order is time order
			return false
		}
		ts[i] = v
	}
	return !ts[1].Before(ts[0]) && !ts[2].After(ts[0])
}

// RefusalReason makes s a reason Valid accepts: control characters and
// runs of spaces become one space, invalid UTF-8 is dropped, and the text is
// cut to MaxRefusalReason on a character boundary. A controller's words are
// often one line with a trailing newline; that must not hide the refusal.
func RefusalReason(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) {
			return ' '
		}
		return r
	}, strings.ToValidUTF8(s, ""))
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > MaxRefusalReason {
		s = strings.ToValidUTF8(s[:MaxRefusalReason], "")
	}
	return s
}

// StandsAt says whether r still stands at now (before its Until).
func (r HostRefusal) StandsAt(now time.Time) bool {
	u, err := time.Parse(time.RFC3339, r.Until)
	return err == nil && now.Before(u)
}

// Host modes: the override in force (HostUnreadable: its file cannot be
// read).
const (
	HostAuto       = "auto"
	HostOpen       = "open"
	HostDrain      = "drain"
	HostClosed     = "closed"
	HostUnreadable = "unreadable"
)

// Host states: what the mode means for new work now.
const (
	HostAccepting = "accepting"
	HostHolding   = "holding"
	HostDraining  = "draining"
	HostShut      = "closed"
)

// Host modes and states: closed sets, as the driver writes them.
var (
	hostModes  = map[string]bool{HostOpen: true, HostDrain: true, HostClosed: true, HostAuto: true, HostUnreadable: true}
	hostStates = map[string]bool{HostAccepting: true, HostHolding: true, HostDraining: true, HostShut: true}
)

// Valid says whether h is a host block a reader may show: a mode and state
// from their sets, every time RFC 3339, a drain's start and deadline
// together, a cut count (optional) only with them and not negative, and a
// refusal (optional) that is Valid. It is data a driver, or another machine,
// wrote.
func (h HostStatus) Valid() bool {
	if !hostModes[h.Mode] || !hostStates[h.State] || h.Refused != nil && !h.Refused.Valid() {
		return false
	}
	for _, t := range []string{h.Until, h.DrainSince, h.DrainDeadline} {
		if t == "" {
			continue
		}
		if _, err := time.Parse(time.RFC3339, t); err != nil || len(t) > 40 {
			return false
		}
	}
	drain := h.DrainSince != ""
	return drain == (h.DrainDeadline != "") && (h.DrainCut == nil || drain && *h.DrainCut >= 0)
}

// CleanHost is h when it is Valid, else nil: a reader shows no host block
// rather than a bad one. A refusal that is not Valid is dropped, and the
// rest of the block judged without it.
func CleanHost(h *HostStatus) *HostStatus {
	if h == nil {
		return nil
	}
	c := *h
	if h.Refused != nil {
		c.Refused = nil
		if h.Refused.Valid() {
			r := *h.Refused
			c.Refused = &r
		}
	}
	if !c.Valid() {
		return nil
	}
	if h.DrainCut != nil {
		n := *h.DrainCut
		c.DrainCut = &n
	}
	return &c
}
