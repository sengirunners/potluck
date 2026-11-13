package protocol

import "time"

// HostStatus is describe's `host`: the host switch as a driver that has one
// reads it now (potluck-driver-tart's host override, vmhost/hostmode.go).
// Mode is the override in force: open, drain, closed, auto, or unreadable.
// State is what that means for new work: accepting, holding, draining or
// closed. Until is when the override returns to auto. During a drain,
// DrainSince is when it began, DrainDeadline when running work is cut, and
// DrainCut how many boxes it has cut so far (a driver's describe; nil from a
// reader with no boxes, vmhost.HostNow). Times are RFC 3339 UTC.
type HostStatus struct {
	Mode          string `json:"mode"`
	State         string `json:"state"`
	Until         string `json:"until,omitempty"`
	DrainSince    string `json:"drain_since,omitempty"`
	DrainDeadline string `json:"drain_deadline,omitempty"`
	DrainCut      *int   `json:"drain_cut,omitempty"`
}

// Host modes and states: closed sets, as the driver writes them.
var (
	hostModes  = map[string]bool{"open": true, "drain": true, "closed": true, "auto": true, "unreadable": true}
	hostStates = map[string]bool{"accepting": true, "holding": true, "draining": true, "closed": true}
)

// Valid says whether h is a host block a reader may show: a mode and state
// from their sets, every time RFC 3339, a drain's start and deadline
// together, and a cut count (optional) only with them and not negative. It is data a driver,
// or another machine, wrote.
func (h HostStatus) Valid() bool {
	if !hostModes[h.Mode] || !hostStates[h.State] {
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
// rather than a bad one.
func CleanHost(h *HostStatus) *HostStatus {
	if h == nil || !h.Valid() {
		return nil
	}
	c := *h
	if h.DrainCut != nil {
		n := *h.DrainCut
		c.DrainCut = &n
	}
	return &c
}
