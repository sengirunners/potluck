// Package menubar is what Potluck's macOS menu bar item shows and what its
// clicks do: the facts about this machine's lending (its cores, the sizes it
// shares and what runs on them, the host switch) turned into the menu's
// lines and items. `potluck host status --json` answers a Status; the AppKit
// shell (macos/PotluckMenu) only draws it and runs an item's Args.
package menubar

import (
	"cmp"
	"fmt"
	"strings"
	"time"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/protocol"
)

// Facts are what the menu is made from.
type Facts struct {
	// Cores is this machine's logical CPU count.
	Cores int
	// Daemon is why the daemon did not answer ("" when it did).
	Daemon string
	// ListErr is why some of the shared sizes could not be listed, though
	// the daemon answered ("" when all were).
	ListErr string
	// Targets are this machine's machine-level bindings with describe and
	// live handles (GET /v0/run_targets?host=1&live=1).
	Targets []computeapi.RunTargetView
	// Host is the host switch as read now; nil when it could not be read
	// at all.
	Host *protocol.HostStatus
	// HostErr is why the host switch file cannot be read ("" when it can).
	HostErr string
	// Loc is where times are shown (time.Local in the command).
	Loc *time.Location
}

// Lending is how much of this machine is lent and in use.
type Lending struct {
	// Lent is the most cores the shared sizes may use at once: per driver
	// state directory and OS (which share their slots), the slots times the
	// largest size's cores.
	Lent int `json:"lent"`
	// InUse is the cores held by running boxes; Jobs how many those are.
	InUse int `json:"in_use"`
	Jobs  int `json:"jobs"`
	// Sizes is how many sizes this machine shares, answered or not.
	Sizes int `json:"sizes"`
	// Unknown names the shared sizes whose driver did not answer, so their
	// boxes are not counted.
	Unknown []string `json:"unknown,omitempty"`
}

// Lend sums the bindings. A binding with no size (not a box binding) lends
// nothing countable; one whose driver gives no slots counts as one slot.
func Lend(targets []computeapi.RunTargetView) Lending {
	var out Lending
	type pool struct {
		slots, cpu int
	}
	pools := map[string]*pool{}
	var order []string
	for _, t := range targets {
		if t.RunTarget.Host != "" || t.Kind != "box" {
			continue
		}
		out.Sizes++
		if t.Error != "" || t.Describe == nil || t.Describe.Size == nil {
			out.Unknown = append(out.Unknown, t.Name)
			continue
		}
		size := t.Describe.Size
		slots := 1
		if s := t.Describe.Slots; s != nil && s.Total > 0 {
			slots = s.Total
		}
		key := t.Config["state_dir"] + "\x00" + cmp.Or(size.OS, "linux")
		p := pools[key]
		if p == nil {
			p = &pool{}
			pools[key] = p
			order = append(order, key)
		}
		p.slots = max(p.slots, slots)
		p.cpu = max(p.cpu, size.CPU)
		running := max(len(t.Live)-len(t.Stopped), 0)
		out.Jobs += running
		out.InUse += running * size.CPU
	}
	for _, k := range order {
		out.Lent += pools[k].slots * pools[k].cpu
	}
	return out
}

// Item is one clickable line. Args, when set, are the potluck arguments a
// click runs; an item already in force has none. Confirm, when set, is the
// question to ask before running them.
type Item struct {
	ID      string   `json:"id"`
	Label   string   `json:"label"`
	Sub     string   `json:"sub,omitempty"`
	Checked bool     `json:"checked"`
	Args    []string `json:"args,omitempty"`
	Confirm string   `json:"confirm,omitempty"`
}

// Menu is the menu as drawn: a title and detail beside the pot, an optional
// note, a row of one mark per core, then the items.
type Menu struct {
	Title  string `json:"title"`
	Detail string `json:"detail"`
	Note   string `json:"note,omitempty"`
	// Cores is one rune per core: '●' in use, '○' lent and free, '·' kept.
	Cores string `json:"cores"`
	Items []Item `json:"items"`
}

// Status is `potluck host status`'s answer.
type Status struct {
	Mode    string  `json:"mode"`
	State   string  `json:"state"`
	Until   string  `json:"until,omitempty"`
	Cores   int     `json:"cores"`
	Lending Lending `json:"lending"`
	Menu    Menu    `json:"menu"`
}

// Build turns the facts into the status and its menu.
func Build(f Facts) Status {
	loc := f.Loc
	if loc == nil {
		loc = time.UTC
	}
	l := Lend(f.Targets)
	st := Status{Cores: f.Cores, Lending: l, Mode: protocol.HostUnreadable, State: protocol.HostHolding}
	if f.Host != nil {
		st.Mode, st.State, st.Until = f.Host.Mode, f.Host.State, f.Host.Until
	}
	m := &st.Menu
	lent := l.Lent
	switch {
	case f.Host == nil || st.Mode == protocol.HostUnreadable:
		m.Title = "Held: the host switch cannot be read"
		m.Note = cmp.Or(f.HostErr, "no new jobs start until it can")
	case f.Daemon != "":
		m.Title = "Potluck is not running"
		m.Note = "start it: potluck daemon --detach"
	case l.Sizes == 0 && f.ListErr != "":
		m.Title = "Cannot read what this Mac lends"
	case l.Sizes == 0:
		m.Title = "Not lending"
		m.Note = "share this Mac: potluck host"
	case st.Mode == protocol.HostClosed:
		m.Title = "Stopped: lending nothing"
	case st.State == protocol.HostDraining:
		m.Title = fmt.Sprintf("Winding down %d of %d cores", lent, f.Cores)
		if t, err := time.Parse(time.RFC3339, f.Host.DrainDeadline); err == nil && !now().Before(t) {
			m.Note = "jobs still running are being ended"
		} else if d := clock(f.Host.DrainDeadline, loc); d != "" {
			m.Note = "jobs still running end at " + d
		}
	case st.State == protocol.HostAccepting:
		m.Title = fmt.Sprintf("Lending %d of %d cores", lent, f.Cores)
	default: // auto, and the owner's schedule or idle rule says no now
		m.Title = "Paused by your lending rules"
	}
	if f.Daemon == "" {
		m.Detail = inUse(l)
		if len(l.Unknown) > 0 {
			m.Note = join(m.Note, "not counted (no answer): "+strings.Join(l.Unknown, ", "))
		}
		if f.ListErr != "" {
			m.Note = join(m.Note, "not everything shared could be read: "+f.ListErr)
		}
	}
	if u := clock(st.Until, loc); u != "" && st.Mode != protocol.HostAuto {
		m.Note = join(m.Note, "until "+u+", then back to your rules")
	}
	m.Cores = dots(f.Cores, lent, l.InUse)
	keep := st.Mode == protocol.HostAuto || st.Mode == protocol.HostOpen
	m.Items = []Item{
		item("keep", "Keep lending", "", keep, protocol.HostAuto),
		item("wind", "Wind down", "finish jobs, take no new ones", st.Mode == protocol.HostDrain, protocol.HostDrain),
		item("stop", "Stop now", "end jobs now", st.Mode == protocol.HostClosed, protocol.HostClosed),
	}
	if stop := &m.Items[2]; stop.Args != nil && l.Jobs > 0 {
		stop.Confirm = fmt.Sprintf("End %s now? Their work is lost.", plural(l.Jobs, "running job"))
	}
	return st
}

func item(id, label, sub string, checked bool, mode string) Item {
	it := Item{ID: id, Label: label, Sub: sub, Checked: checked}
	if !checked {
		it.Args = []string{"host", "set", mode}
	}
	return it
}

func inUse(l Lending) string {
	if l.Jobs == 0 {
		return "nothing running"
	}
	return fmt.Sprintf("%s in use by %s", plural(l.InUse, "core"), plural(l.Jobs, "job"))
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}

func join(a, b string) string {
	if a == "" {
		return b
	}
	return a + "; " + b
}

// clock is an RFC 3339 time as a local HH:MM, with the date when it is not
// today's ("" for none or a bad one).
func clock(s string, loc *time.Location) string {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return ""
	}
	t = t.In(loc)
	if y, m, d := t.Date(); y != nowIn(loc).Year() || m != nowIn(loc).Month() || d != nowIn(loc).Day() {
		return t.Format("Jan 2 15:04")
	}
	return t.Format("15:04")
}

// now is the clock; tests set it.
var now = time.Now

func nowIn(loc *time.Location) time.Time { return now().In(loc) }

// dots is one mark per core: in use first, then lent and free, then kept.
// More in use than lent (a size changed under running boxes) shows as all
// lent in use.
func dots(cores, lent, inUse int) string {
	if cores <= 0 {
		return ""
	}
	lent = min(lent, cores)
	inUse = min(inUse, lent)
	return strings.Repeat("●", inUse) + strings.Repeat("○", lent-inUse) + strings.Repeat("·", cores-lent)
}
