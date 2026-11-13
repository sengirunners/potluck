package protocol

import "testing"

func TestHostStatusValid(t *testing.T) {
	zero, one, neg := 0, 1, -1
	const at = "2026-11-13T12:00:00Z"
	for _, c := range []struct {
		name string
		h    HostStatus
		ok   bool
	}{
		{"auto", HostStatus{Mode: "auto", State: "accepting"}, true},
		{"unreadable", HostStatus{Mode: "unreadable", State: "holding"}, true},
		{"open until", HostStatus{Mode: "open", State: "accepting", Until: at}, true},
		{"drain", HostStatus{Mode: "drain", State: "draining", DrainSince: at, DrainDeadline: at, DrainCut: &zero}, true},
		{"drain cut one", HostStatus{Mode: "drain", State: "closed", DrainSince: at, DrainDeadline: at, DrainCut: &one}, true},
		{"unknown mode", HostStatus{Mode: "wide-open", State: "accepting"}, false},
		{"unknown state", HostStatus{Mode: "auto", State: "great"}, false},
		{"bad time", HostStatus{Mode: "open", State: "accepting", Until: "tomorrow"}, false},
		{"control character", HostStatus{Mode: "open", State: "accepting", Until: at + "\x1b[2J"}, false},
		{"negative cut", HostStatus{Mode: "drain", State: "draining", DrainSince: at, DrainDeadline: at, DrainCut: &neg}, false},
		{"half a drain", HostStatus{Mode: "drain", State: "draining", DrainSince: at}, false},
		{"drain without a cut count", HostStatus{Mode: "drain", State: "draining", DrainSince: at, DrainDeadline: at}, true},
		{"cut without a drain", HostStatus{Mode: "auto", State: "accepting", DrainCut: &one}, false},
	} {
		if got := c.h.Valid(); got != c.ok {
			t.Errorf("%s: Valid %v, want %v", c.name, got, c.ok)
		}
		if got := CleanHost(&c.h); (got != nil) != c.ok {
			t.Errorf("%s: CleanHost %v, want kept %v", c.name, got, c.ok)
		}
	}
	if CleanHost(nil) != nil {
		t.Error("CleanHost(nil) is not nil")
	}
}
