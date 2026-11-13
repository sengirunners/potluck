package vmhost

import (
	"fmt"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Policy is when the owner wants the Mac back. IdleS > 0: the owner is active
// while the Mac's HID idle time (keyboard, mouse, trackpad) is below it.
// Window "HH:MM[:SS]-HH:MM[:SS]" (local time, may wrap midnight): work only
// inside it, every day. Windows is the same with weekdays: entries split by
// ";", each "[days ]HH:MM[:SS]-HH:MM[:SS]", days "mon", "mon-fri" (a range
// may wrap: "fri-mon") or "sat,sun"; no days is every day, and a window that
// wraps midnight belongs to the day it opens. A binding sets one of the two.
// While either says no, describe answers available false and acquire is
// refused; the host's state (hostmode.go) says what happens to running work.
type Policy struct {
	IdleS   int
	Window  string
	Windows string
}

func (p Policy) on() bool { return p.IdleS > 0 || p.windowed() }

func (p Policy) windowed() bool { return p.Window != "" || p.Windows != "" }

// Allows says whether work may run now, given the owner's idle time.
func (p Policy) Allows(now time.Time, idle time.Duration) (bool, string) {
	if p.windowed() {
		in, err := p.inWindow(now)
		if err != nil {
			return false, err.Error()
		}
		if !in {
			return false, "outside the window " + p.windowText()
		}
	}
	if p.IdleS > 0 && idle < time.Duration(p.IdleS)*time.Second {
		return false, fmt.Sprintf("the owner is active (idle %ds, under %ds)", int(idle.Seconds()), p.IdleS)
	}
	return true, ""
}

func (p Policy) windowText() string { return p.Window + p.Windows }

// span is one window: the weekdays it opens on (bit i is time.Weekday i),
// and its ends in seconds since midnight (to <= from wraps midnight).
type span struct {
	days     uint8
	from, to int
}

const everyDay = 1<<7 - 1

// spans parses the policy's windows.
func (p Policy) spans() ([]span, error) {
	switch {
	case p.Window != "" && p.Windows != "":
		return nil, fmt.Errorf("window and windows: set one")
	case p.Window != "":
		from, to, err := parseWindow(p.Window)
		return []span{{everyDay, from, to}}, err
	}
	var out []span
	for _, e := range strings.Split(p.Windows, ";") {
		e = strings.TrimSpace(e)
		days, clock := uint8(everyDay), e
		if d, c, ok := strings.Cut(e, " "); ok {
			var err error
			if days, err = parseDays(d); err != nil {
				return nil, fmt.Errorf("windows %q: %v", p.Windows, err)
			}
			clock = strings.TrimSpace(c)
		}
		from, to, err := parseWindow(clock)
		if err != nil {
			return nil, fmt.Errorf("windows %q: %v", p.Windows, err)
		}
		out = append(out, span{days, from, to})
	}
	return out, nil
}

var dayNames = []string{"sun", "mon", "tue", "wed", "thu", "fri", "sat"}

// parseDays reads "mon", "mon-fri" or "sat,sun" as weekday bits.
func parseDays(s string) (uint8, error) {
	day := func(n string) (int, error) {
		if i := slices.Index(dayNames, strings.ToLower(n)); i >= 0 {
			return i, nil
		}
		return 0, fmt.Errorf("%q is not a day (sun, mon, tue, wed, thu, fri, sat)", n)
	}
	var bits uint8
	for _, part := range strings.Split(s, ",") {
		a, b, isRange := strings.Cut(part, "-")
		from, err := day(a)
		if err != nil {
			return 0, err
		}
		to := from
		if isRange {
			if to, err = day(b); err != nil {
				return 0, err
			}
		}
		for d := from; ; d = (d + 1) % 7 {
			bits |= 1 << d
			if d == to {
				break
			}
		}
	}
	return bits, nil
}

func (sp span) opensOn(d time.Weekday) bool { return sp.days&(1<<d) != 0 }

// inWindow says whether now is inside one of the policy's windows.
func (p Policy) inWindow(now time.Time) (bool, error) {
	ss, err := p.spans()
	if err != nil {
		return false, err
	}
	m := now.Hour()*3600 + now.Minute()*60 + now.Second()
	today, yesterday := now.Weekday(), (now.Weekday()+6)%7
	for _, sp := range ss {
		if sp.to > sp.from && sp.opensOn(today) && sp.from <= m && m < sp.to {
			return true, nil
		}
		if sp.to <= sp.from && (sp.opensOn(today) && m >= sp.from || sp.opensOn(yesterday) && m < sp.to) {
			return true, nil
		}
	}
	return false, nil
}

// lastClose is when a window last closed at or before now (the zero time if
// none did in the past week): when leaving the window began a drain.
func (p Policy) lastClose(now time.Time) time.Time {
	ss, err := p.spans()
	if err != nil {
		return time.Time{}
	}
	var last time.Time
	for k := 0; k <= 8; k++ {
		day := time.Date(now.Year(), now.Month(), now.Day()-k, 0, 0, 0, 0, now.Location())
		for _, sp := range ss {
			if !sp.opensOn(day.Weekday()) {
				continue
			}
			next := 0 // wall clock, as inWindow reads it, so a DST day moves nothing
			if sp.to <= sp.from {
				next = 1
			}
			end := time.Date(day.Year(), day.Month(), day.Day()+next, 0, 0, sp.to, 0, now.Location())
			if !end.After(now) && end.After(last) {
				last = end
			}
		}
	}
	return last
}

var clockRe = regexp.MustCompile(`^([01]\d|2[0-3]):([0-5]\d)(?::([0-5]\d))?$`)

// parseWindow answers the window's ends in seconds since midnight.
func parseWindow(w string) (from, to int, err error) {
	a, b, _ := strings.Cut(w, "-")
	ma, mb := clockRe.FindStringSubmatch(a), clockRe.FindStringSubmatch(b)
	if ma == nil || mb == nil {
		return 0, 0, fmt.Errorf("window %q is not HH:MM[:SS]-HH:MM[:SS]", w)
	}
	sec := func(m []string) int {
		h, _ := strconv.Atoi(m[1])
		mi, _ := strconv.Atoi(m[2])
		s, _ := strconv.Atoi(m[3]) // "" is 0
		return h*3600 + mi*60 + s
	}
	return sec(ma), sec(mb), nil
}

var hidIdleRe = regexp.MustCompile(`"HIDIdleTime" = (\d+)`)

// hidIdle is how long since the last keyboard or pointer input on this Mac.
func hidIdle() (time.Duration, error) {
	out, err := exec.Command("ioreg", "-c", "IOHIDSystem", "-d", "4", "-r", "-k", "HIDIdleTime").Output()
	if err != nil {
		return 0, err
	}
	m := hidIdleRe.FindSubmatch(out)
	if m == nil {
		return 0, fmt.Errorf("ioreg shows no HIDIdleTime")
	}
	ns, err := strconv.ParseInt(string(m[1]), 10, 64)
	return time.Duration(ns), err
}

// reading is what a policy check saw: the idle time and when it was read,
// whether owner input (not the window, not an unreadable idle time) is what
// said no, and whether the clock is outside every window.
type reading struct {
	idle    time.Duration
	at      time.Time
	byInput bool
	outside bool
}

func (p Policy) check() (bool, string, reading) {
	rd := reading{at: time.Now()}
	if !p.on() {
		return true, "", rd
	}
	if p.IdleS > 0 {
		idle, err := hidIdle()
		rd.at = time.Now()
		if err != nil {
			return false, "cannot read the owner's idle time: " + err.Error(), rd
		}
		rd.idle = idle
	}
	ok, why := p.Allows(rd.at, rd.idle)
	if !ok {
		inWindow, err := p.inWindow(rd.at)
		if !p.windowed() {
			inWindow, err = true, nil
		}
		rd.outside = err == nil && !inWindow
		rd.byInput = err == nil && inWindow && p.IdleS > 0 && rd.idle < time.Duration(p.IdleS)*time.Second
	}
	return ok, why, rd
}

// ReclaimPolicy is the reclaim policy the driver takes from a binding's config
// (reclaim_idle_s, window or windows): the whole config is checked as the driver checks
// it, so a binding the driver would refuse is refused here too. For hosts that
// write that config (zm host share) to test what the driver will read.
func ReclaimPolicy(cfg map[string]string) (Policy, error) {
	c, err := parseConfig(cfg)
	if err != nil {
		return Policy{}, err
	}
	return c.Reclaim, nil
}
