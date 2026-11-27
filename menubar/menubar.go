// Package menubar is what Potluck's macOS menu bar item shows and what its
// clicks do: the facts about this machine's lending (its cores, the sizes it
// shares and what runs on them, the host switch) turned into the menu's
// lines and items. `potluck host status --json` answers a Status; the AppKit
// shell (macos/PotluckMenu) only draws it and runs an item's Args.
package menubar

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/hostmem"
	"github.com/sengirunners/potluck/hostsetup"
	"github.com/sengirunners/potluck/protocol"
	"github.com/sengirunners/potluck/vmhost"
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
	// Start and Share are the commands that start this machine's daemon and
	// share this machine, as the program that answers spells them
	// ("potluck daemon --detach", "potluck host"; cli renders them).
	Start, Share string
	// Background is a host run going on in the background, or the last
	// one's end (nil: none).
	Background *Background
	// Setup is what the setup checklist is made from.
	Setup SetupFacts
	// Memory is the memory pressure and swap now (nil: not read);
	// MemoryMB the machine's memory.
	Memory   *hostmem.Reading
	MemoryMB int64
	// Lend is the lender's dials (potluck host lend); LendErr why they
	// cannot be read ("" when they can), which holds new work.
	Lend    vmhost.HostLend
	LendErr string
	// DiskFreeGB and DiskTotalGB are the disk boxes live on (-1 and 0: not
	// read).
	DiskFreeGB, DiskTotalGB float64
}

// Background is a host run in the background (host --detach): running
// now, or ended, with the last line it wrote.
type Background struct {
	State string `json:"state"` // running or ended
	Last  string `json:"last,omitempty"`
}

// Lending is how much of this machine is lent and in use.
type Lending struct {
	// Shared is the most cores the shared sizes may use at once: per
	// driver state directory and OS (which share their slots), the slots
	// times the largest size's cores. Lent is that with the lender's cores
	// dial, which bounds each of those groups.
	Shared int `json:"shared"`
	Lent   int `json:"lent"`
	// InUse is the cores held by running boxes; Jobs how many those are.
	InUse int `json:"in_use"`
	Jobs  int `json:"jobs"`
	// MemoryShared, MemoryLent and MemoryInUse are the same for memory, in
	// MiB.
	MemoryShared int `json:"memory_shared_mb"`
	MemoryLent   int `json:"memory_lent_mb"`
	MemoryInUse  int `json:"memory_in_use_mb"`
	// Groups is how many state directory and OS groups there are: a dial
	// bounds each.
	Groups int `json:"groups"`
	// Sizes is how many sizes this machine shares, answered or not.
	Sizes int `json:"sizes"`
	// Unknown names the shared sizes whose driver did not answer, so their
	// boxes are not counted.
	Unknown []string `json:"unknown,omitempty"`
	// DialsIgnoredBy names the shared sizes whose driver's describe does not
	// list protocol.CapHostLend: the lender's dials do not limit their jobs
	// (a driver from before the dials ignores them).
	DialsIgnoredBy []string `json:"dials_ignored_by,omitempty"`
	// minCPU and minMemory are the smallest size's: a dial's step; sizes
	// are the sizes that answered, for the choices too small for some.
	minCPU, minMemory int
	sizes             []computeapi.RunTargetSize
}

// Lend sums the bindings under the lender's dials. A binding with no size
// (not a box binding) lends nothing countable; one whose driver gives no
// slots counts as one slot.
func Lend(targets []computeapi.RunTargetView, dial vmhost.HostLend) Lending {
	var out Lending
	type pool struct {
		slots, cpu, mem int
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
		if !slices.Contains(t.Describe.Capabilities, protocol.CapHostLend) {
			out.DialsIgnoredBy = append(out.DialsIgnoredBy, t.Name)
		}
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
		p.mem = max(p.mem, size.MemoryMB)
		out.sizes = append(out.sizes, *size)
		if size.CPU > 0 && (out.minCPU == 0 || size.CPU < out.minCPU) {
			out.minCPU = size.CPU
		}
		if size.MemoryMB > 0 && (out.minMemory == 0 || size.MemoryMB < out.minMemory) {
			out.minMemory = size.MemoryMB
		}
		running := max(len(t.Live)-len(t.Stopped), 0)
		out.Jobs += running
		out.InUse += running * size.CPU
		out.MemoryInUse += running * size.MemoryMB
	}
	bound := func(n, limit int) int {
		if limit > 0 {
			return min(n, limit)
		}
		return n
	}
	for _, k := range order {
		p := pools[k]
		out.Shared += p.slots * p.cpu
		out.Lent += bound(p.slots*p.cpu, dial.Cores)
		out.MemoryShared += p.slots * p.mem
		out.MemoryLent += bound(p.slots*p.mem, dial.MemoryMB)
	}
	out.Groups = len(order)
	return out
}

// DialsIgnored says which shared sizes the dials do not limit
// (Lending.DialsIgnoredBy), "" when every size that answered obeys them.
func DialsIgnored(l Lending) string {
	if len(l.DialsIgnoredBy) == 0 {
		return ""
	}
	which := "the dials (cores, memory and disk kept free) do not limit " + strings.Join(l.DialsIgnoredBy, ", ")
	if len(l.DialsIgnoredBy) == l.Sizes-len(l.Unknown) {
		which = "the dials (cores, memory and disk kept free) limit no size this machine shares (" + strings.Join(l.DialsIgnoredBy, ", ") + ")"
	}
	return which + ": the driver the daemon runs for them does not say it obeys the dials (one from before them ignores them), so the dials do not hold their jobs until the daemon runs a driver that does"
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

// Menu is the menu as drawn: a title and detail, an optional note, a row of
// one mark per core, then the items.
type Menu struct {
	Title  string `json:"title"`
	Detail string `json:"detail"`
	Note   string `json:"note,omitempty"`
	// Attention is that the menu bar marks its picture (needsAttention);
	// the title and the note say why.
	Attention bool `json:"attention,omitempty"`
	// Cores is one rune per core: '●' in use, '○' lent and free, '·' kept.
	Cores string `json:"cores"`
	// Memory is the memory pressure line ("" when not read).
	Memory string `json:"memory,omitempty"`
	Items  []Item `json:"items"`
	// Dials are the lender's dials, each a submenu of choices, below the
	// switch.
	Dials []Dial `json:"dials,omitempty"`
	// Windows are items below the switch that each open a window: the
	// app runs Args and draws the JSON answer as the window named by ID
	// ("jobs": a JobsView; "setup": the Setup of the same status, which
	// the window asks for again every few seconds while it is open).
	Windows []Item `json:"windows,omitempty"`
}

// Status is `potluck host status`'s answer. Refused, when set, is the
// host's admission controller refusing new work now (Refused), whatever the
// switch says.
type Status struct {
	Mode    string                `json:"mode"`
	State   string                `json:"state"`
	Until   string                `json:"until,omitempty"`
	Refused *protocol.HostRefusal `json:"refused,omitempty"`
	Cores   int                   `json:"cores"`
	Lending Lending               `json:"lending"`
	Menu    Menu                  `json:"menu"`
	// Background is Facts.Background.
	Background *Background `json:"background,omitempty"`
	// Setup is the setup checklist and the one thing to do next.
	Setup Setup `json:"setup"`
	// ImagesBehind is a line per shared size that lends other images than
	// the daemon's build pins, and why (computeapi.ImagesBehind).
	ImagesBehind []string `json:"images_behind,omitempty"`
}

// imagesBehind is Status.ImagesBehind of targets.
func imagesBehind(targets []computeapi.RunTargetView) []string {
	var out []string
	for _, t := range targets {
		if b := t.ImagesBehind; b != nil {
			out = append(out, t.Name+" lends other images than this version pins ("+strings.Join(b.Lends, ", ")+", not "+strings.Join(b.Current, ", ")+"): "+b.Why)
		}
	}
	return out
}

// Refused is the newest admission refusal still standing now on any of the
// host's shared sizes (each driver state directory keeps its own), or on
// host. A describe may be older than now (the daemon caches it), so one
// past its Until is dropped here.
func Refused(host *protocol.HostStatus, targets []computeapi.RunTargetView) *protocol.HostRefusal {
	var out *protocol.HostRefusal
	take := func(h *protocol.HostStatus) {
		if h = protocol.CleanHost(h); h == nil || h.Refused == nil || !h.Refused.StandsAt(now()) {
			return
		}
		if out == nil || h.Refused.At > out.At { // RFC 3339 UTC: text order is time order
			out = h.Refused
		}
	}
	take(host)
	for _, t := range targets {
		if t.RunTarget.Host == "" && t.Describe != nil {
			take(t.Describe.Host)
		}
	}
	return out
}

// Build turns the facts into the status and its menu.
func Build(f Facts) Status {
	loc := f.Loc
	if loc == nil {
		loc = time.UTC
	}
	ended := Lend(slices.DeleteFunc(slices.Clone(f.Targets), func(t computeapi.RunTargetView) bool { return !f.Setup.Stranded(t) }), f.Lend)
	f.Targets = slices.DeleteFunc(slices.Clone(f.Targets), f.Setup.Stranded)
	l := Lend(f.Targets, f.Lend)
	over := overNote(l)
	// A size nobody can use any more lends nothing, but a job still on it runs.
	l.Jobs, l.InUse, l.MemoryInUse = l.Jobs+ended.Jobs, l.InUse+ended.InUse, l.MemoryInUse+ended.MemoryInUse
	st := Status{Cores: f.Cores, Lending: l, Mode: protocol.HostUnreadable, State: protocol.HostHolding, Background: f.Background}
	if f.Host != nil {
		st.Mode, st.State, st.Until = f.Host.Mode, f.Host.State, f.Host.Until
	}
	st.Refused = Refused(f.Host, f.Targets)
	f.Setup.Run = runNow(f.Setup)
	setup := runState(f.Setup)
	m := &st.Menu
	lent := l.Lent
	switch {
	case f.Daemon != "": // before the switch, as in needsAttention
		m.Title = "Potluck is not running"
		m.Note = "start it: " + f.Start
	case f.Host == nil || st.Mode == protocol.HostUnreadable:
		m.Title = "Held: the host switch cannot be read"
		m.Note = cmp.Or(f.HostErr, "no new jobs start until it can")
	case l.Sizes == 0 && f.ListErr != "":
		m.Title = "Cannot read what this Mac lends"
	case l.Sizes == 0 && setup == SetupRunning:
		m.Title = "Setting up: " + stageSaid(f.Setup.Run)
		if n := len(f.Setup.Run.Stages); n > 0 {
			m.Note = f.Setup.Run.Stages[n-1].Detail
		}
	case l.Sizes == 0 && (setup == SetupBlocked || setup == SetupInterrupted):
		m.Title = stoppedSaid(f.Setup.Run) // the menu's Set up and check… and host status's setup lines say why, and what next
	case l.Sizes == 0 && ended.Sizes > 0:
		m.Title = "Not lending"
		m.Note = "the group this Mac lent to is gone; ask for a new invite"
	case l.Sizes == 0:
		m.Title = "Not lending"
		m.Note = "share this Mac: " + f.Share
	case st.Mode == protocol.HostClosed: // host pause, or Stop now
		m.Title = "Paused: lending nothing"
	case st.State == protocol.HostDraining:
		m.Title = fmt.Sprintf("Winding down %d of %d cores", lent, f.Cores)
		if t, err := time.Parse(time.RFC3339, f.Host.DrainDeadline); err == nil && !now().Before(t) {
			m.Note = "jobs still running are being ended"
		} else if d := clock(f.Host.DrainDeadline, loc); d != "" {
			m.Note = "jobs still running end at " + d
		}
	case noDriver(l):
		m.Title = "Cannot reach what this Mac lends"
	case st.State == protocol.HostAccepting && st.Refused != nil:
		m.Title = "Refusing new work now"
		m.Note = st.Refused.Reason
		if at, u := clock(st.Refused.At, loc), clock(st.Refused.Until, loc); at != "" && u != "" {
			m.Note += " (said at " + at + ", retry after " + u + ")"
		}
		if r := st.Refused; r.Pushed > 0 {
			m.Note += fmt.Sprintf("; its retry time was pushed back %s", times(r.Pushed))
			if since := clock(r.Since, loc); since != "" {
				m.Note += ", refusing since " + since
			}
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
	switch {
	case l.Sizes == 0 && setup == SetupRunning: // the stage says it
	case f.Daemon != "" && !f.Setup.Stopped && (setup == SetupBlocked || setup == SetupInterrupted):
		m.Note = join(m.Note, "the last setup "+strings.TrimPrefix(stoppedSaid(f.Setup.Run), "Setup "))
	case l.Sizes > 0 && setup == SetupRunning:
		m.Note = join(m.Note, "setting up again: "+stageSaid(f.Setup.Run))
	case l.Sizes > 0 && (setup == SetupBlocked || setup == SetupInterrupted):
		m.Note = join(m.Note, "the last setup "+strings.TrimPrefix(stoppedSaid(f.Setup.Run), "Setup "))
		if !noDriver(l) {
			m.Note += "; what was shared before still lends"
		}
	case f.Background != nil && f.Background.State == "running":
		m.Note = join(m.Note, "sharing goes on in the background: "+f.Background.Last)
	}
	if st.ImagesBehind = imagesBehind(f.Targets); len(st.ImagesBehind) > 0 {
		lend := " lend"
		if len(st.ImagesBehind) == 1 {
			lend = " lends"
		}
		m.Note = join(m.Note, plural(len(st.ImagesBehind), "size")+lend+" other images than this version pins (host status says why)")
	}
	if u := clock(st.Until, loc); u != "" && st.Mode != protocol.HostAuto {
		m.Note = join(m.Note, "until "+u+", then back to your rules")
	}
	if f.LendErr != "" {
		m.Note = join(m.Note, "no new jobs start until your limits can be read: "+f.LendErr)
	} else if over != "" && f.Daemon == "" {
		m.Note = join(m.Note, over)
	}
	if n := DialsIgnored(l); n != "" && f.Daemon == "" {
		m.Note = join(m.Note, n)
	}
	m.Attention = needsAttention(f, st, setup, ended.Sizes)
	m.Cores = dots(f.Cores, lent, l.InUse)
	m.Memory = memoryLine(f, l)
	keep := st.Mode == protocol.HostAuto || st.Mode == protocol.HostOpen
	m.Items = []Item{
		item("keep", "Keep lending", "", keep, protocol.HostAuto),
		item("wind", "Wind down", "finish jobs, take no new ones", st.Mode == protocol.HostDrain, protocol.HostDrain),
		item("stop", "Stop now", "end jobs now", st.Mode == protocol.HostClosed, protocol.HostClosed),
	}
	if st.Mode == protocol.HostClosed { // paused: the way back
		m.Items[0].Label = "Resume lending"
	}
	if stop := &m.Items[2]; stop.Args != nil && l.Jobs > 0 {
		stop.Confirm = fmt.Sprintf("End %s now? Their work is lost.", plural(l.Jobs, "running job"))
	}
	if f.Daemon == "" && f.LendErr == "" {
		m.Dials = dials(f, l)
	}
	m.Windows = []Item{
		{ID: "setup", Label: "Set up and check…", Sub: "what this Mac has, what it lacks, and the next step",
			Args: []string{"--json", "host", "status", "--probe"}},
		{ID: "jobs", Label: "Jobs this Mac ran…", Sub: "when, how long, counts and electricity",
			Args: []string{"--json", "host", "jobs"}},
		{ID: "uninstall", Label: "Uninstall Potluck…", Sub: "shows what it would remove first, and asks",
			Args: []string{"--json", "uninstall", "--dry-run"}},
	}
	st.Setup = buildSetup(f, st)
	return st
}

// needsAttention is that lending stopped and nobody chose that: not on a Mac
// nobody set up (as far as can be read), one a person paused, winds down or
// stopped, or one that still lends. ended is how many sizes lend to a group
// that is gone.
func needsAttention(f Facts, st Status, setup string, ended int) bool {
	l := st.Lending
	switch {
	case st.Mode == protocol.HostClosed, st.Mode == protocol.HostDrain:
		return false
	case f.Daemon != "":
		return !f.Setup.Stopped && setup != SetupNone && setup != SetupRunning // a first run starts it
	case l.Sizes == 0 && ended > 0 && setup != SetupRunning: // the group it lent to is gone; it needs a new invite
		return true
	case l.Sizes == 0 && f.ListErr == "" && setup == SetupNone:
		return false
	case switchUnread(f, st), f.LendErr != "":
		return true
	case l.Sizes == 0:
		return f.ListErr != "" || setup == SetupBlocked || setup == SetupInterrupted
	}
	return noDriver(l)
}

// noDriver is that no shared size's driver answers.
func noDriver(l Lending) bool { return l.Sizes > 0 && len(l.Unknown) == l.Sizes }

// stageAt is the stage the run is at or stopped at: the first before any.
func stageAt(r *hostsetup.Run) string {
	if s := r.Blocked(); s != nil {
		return s.Name
	}
	if n := len(r.Stages); n > 0 {
		return r.Stages[n-1].Name
	}
	return hostsetup.Order[0]
}

// stageSaid is the stage the run is at, in the setup window's words, as
// "download what jobs need". The menu is a person's, so it names no
// command; the setup checklist (host status's "setup:") has the details.
// A stage this Potluck has no words for (a record from another version) is
// named as the record names it.
func stageSaid(r *hostsetup.Run) string {
	w, ok := stageWords[stageAt(r)]
	if !ok || w == "" {
		return hostsetup.Label(stageAt(r))
	}
	return strings.ToLower(w[:1]) + w[1:]
}

// stoppedSaid is where the run stopped, as "Setup stopped while checking
// this Mac".
func stoppedSaid(r *hostsetup.Run) string {
	if s, ok := stopped[stageAt(r)]; ok {
		return strings.TrimSuffix(s, ".")
	}
	return "Setup stopped at " + stageSaid(r)
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

// times is n as a count of times: "once", "twice", "6 times".
func times(n int) string {
	switch n {
	case 1:
		return "once"
	case 2:
		return "twice"
	}
	return fmt.Sprintf("%d times", n)
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
