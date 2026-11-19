package protocol

import (
	"strings"
	"testing"
)

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
		{"refused", HostStatus{Mode: "auto", State: "accepting", Refused: &HostRefusal{Reason: "no room on this host now: busy", At: at, Until: at}}, true},
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

// A refusal a reader may not show is dropped, and the rest of the block
// kept: it is a report from a controller, not the switch.
func TestHostRefusalCleaned(t *testing.T) {
	const at = "2026-11-13T12:00:00Z"
	for _, r := range []HostRefusal{
		{Reason: "", At: at, Until: at},
		{Reason: string(make([]byte, MaxRefusalReason+1)), At: at, Until: at},
		{Reason: "busy", At: "now", Until: at},
		{Reason: "busy", At: at},
		{Reason: "busy\x1b[2J", At: at, Until: at},
		{Reason: "two\nlines", At: at, Until: at},
		{Reason: "busy", At: at, Until: "2026-11-13T11:00:00Z"},                 // until before at
		{Reason: "busy", At: "2026-11-13T12:00:00.5Z", Until: at},               // not to the second
		{Reason: "busy", At: "2026-11-13T04:00:00-08:00", Until: at},            // not UTC
		{Reason: "busy", At: at, Until: at, Since: "2026-11-13T12:00:01Z"},      // since after at
		{Reason: "busy", At: at, Until: at, Since: "2026-11-13T03:00:00-08:00"}, // since not UTC
		{Reason: "busy", At: at, Until: at, Since: "earlier"},
		{Reason: "busy", At: at, Until: at, Pushed: -1},
	} {
		h := HostStatus{Mode: "auto", State: "accepting", Refused: &r}
		if h.Valid() {
			t.Errorf("%+v: Valid", r)
		}
		if c := CleanHost(&h); c == nil || c.Refused != nil || c.Mode != "auto" {
			t.Errorf("%+v: CleanHost %+v, want the block without it", r, c)
		}
	}
	long := HostRefusal{Reason: "busy", At: at, Until: at, Since: "2026-11-13T11:00:00Z", Pushed: 6}
	if c := CleanHost(&HostStatus{Mode: "auto", State: "accepting", Refused: &long}); c == nil || c.Refused == nil || *c.Refused != long {
		t.Errorf("a refusal pushed back since earlier: CleanHost %+v", c)
	}
	h := HostStatus{Mode: "auto", State: "accepting", Refused: &HostRefusal{Reason: "busy", At: at, Until: at}}
	c := CleanHost(&h)
	c.Refused.Reason = "changed"
	if h.Refused.Reason != "busy" {
		t.Error("CleanHost shares the refusal with its input")
	}
}

// A controller's words with control characters or too long become a reason
// Valid accepts, rather than hiding the refusal.
func TestRefusalReason(t *testing.T) {
	for in, want := range map[string]string{
		"CPU busy\n":           "CPU busy",
		"busy\tqueue\r\n  now": "busy queue now",
		"busy\x1b[2J":          "busy [2J",
		"bad\xffutf8":          "badutf8",
	} {
		got := RefusalReason(in)
		if got != want {
			t.Errorf("RefusalReason(%q) = %q, want %q", in, got, want)
		}
		const at = "2026-11-13T12:00:00Z"
		if !(HostRefusal{Reason: got, At: at, Until: at}).Valid() {
			t.Errorf("RefusalReason(%q) = %q is not Valid", in, got)
		}
	}
	long := RefusalReason(strings.Repeat("é", MaxRefusalReason))
	if len(long) > MaxRefusalReason || !(HostRefusal{Reason: long, At: "2026-11-13T12:00:00Z", Until: "2026-11-13T12:00:00Z"}).Valid() {
		t.Errorf("a long reason: %d bytes, valid %v", len(long), HostRefusal{Reason: long}.Valid())
	}
}
