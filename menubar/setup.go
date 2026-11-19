package menubar

// The setup checklist: what is set up on this machine and what is not, from
// facts read now (the last host run's stages, the shared sizes, the menu bar
// item, the daily update), and the one thing to do next. potluck host status
// prints it and carries it in its JSON (setup), and the menu's title and note
// say the same when setup is going on or blocked.

import (
	"fmt"
	"strings"
	"time"

	"github.com/sengirunners/potluck/hostsetup"
	"github.com/sengirunners/potluck/protocol"
)

// SetupFacts are what the checklist is made from, beside the lending facts.
type SetupFacts struct {
	// Version is this program's version.
	Version string
	// Run is the last host run's record (nil: none); RunErr why it could
	// not be read; RunLive that a host run holds the home's lock now.
	Run     *hostsetup.Run
	RunErr  string
	RunLive bool
	// Menu is the menu bar item on a Mac (nil elsewhere).
	Menu *MenuApp
	// Updates is the daily update (nil: the program that answers keeps
	// itself current its own way).
	Updates *Updates
	// The commands the checklist names, as the program that answers spells
	// them.
	InstallMenu string // installs the menu bar item
	Upgrade     string // turns the daily update on
	Doctor      string // says which Tart and Softnet this Mac needs
}

// MenuApp is the menu bar item as this machine shows it.
type MenuApp struct {
	// Path is where the app is installed ("" when it is not).
	Path string
	// Seen is when the app last asked for this status: the proof that it
	// runs and draws (zero: never). Opening the app proves nothing.
	Seen time.Time
	// Open is the command that opens it.
	Open string
}

// MenuSeenWithin is how recent Seen must be for the app to count as
// running: it asks every 15 seconds.
const MenuSeenWithin = time.Minute

// Updates is the daily update's state.
type Updates struct {
	Enabled, Loaded bool
	// Foreign is another updater of this program ("" for none).
	Foreign string
	// Err is why its state cannot be read ("": it was).
	Err string
}

// Check states.
const (
	Done    = "done"
	Todo    = "todo"
	Blocked = "blocked"
	Running = "running"
	// Unknown is a step Potluck cannot read; the check says how to see it.
	Unknown = "unknown"
	// Info explains; nothing to do.
	Info = "info"
)

// Check is one line of the checklist. Action, when set, is the one thing
// that completes it.
type Check struct {
	ID     string `json:"id"`
	State  string `json:"state"`
	Detail string `json:"detail"`
	Action string `json:"action,omitempty"`
}

// Setup is the checklist as host status answers it.
type Setup struct {
	// State is the last host run's: none, running, succeeded, blocked or
	// interrupted (it stopped before it ended), or unreadable.
	State string `json:"state"`
	// Stages are that run's stages.
	Stages []hostsetup.Stage `json:"stages,omitempty"`
	Checks []Check           `json:"checks"`
	// Next is the one thing to do now ("" when nothing Potluck can read is
	// left).
	Next string `json:"next,omitempty"`
}

// The last host run's states.
const (
	SetupNone        = "none"
	SetupRunning     = "running"
	SetupSucceeded   = "succeeded"
	SetupBlocked     = "blocked"
	SetupInterrupted = "interrupted"
	SetupUnreadable  = "unreadable"
)

// runState is the last host run's state.
func runState(s SetupFacts) string {
	r := s.Run
	switch {
	case s.RunErr != "":
		return SetupUnreadable
	case r == nil:
		return SetupNone
	case r.Blocked() != nil:
		return SetupBlocked
	case r.Ended == "" && s.RunLive:
		return SetupRunning
	case r.Ended == "":
		return SetupInterrupted
	case r.Succeeded():
		return SetupSucceeded
	}
	return SetupInterrupted // ended short of the last stage with nothing blocked: not a record a run writes
}

// buildSetup makes the checklist from f and the lending l.
func buildSetup(f Facts, st Status) Setup {
	s := f.Setup
	out := Setup{State: runState(s)}
	if s.Run != nil {
		out.Stages = s.Run.Stages
	}
	add := func(id, state, detail, action string) {
		out.Checks = append(out.Checks, Check{ID: id, State: state, Detail: detail, Action: action})
	}
	version := s.Version
	if version == "" {
		version = "(version unknown)"
	}
	add("potluck", Done, "Potluck "+version+" is installed", "")
	if f.Daemon != "" {
		add("daemon", Todo, "the Potluck daemon is not running: "+f.Daemon, f.Start)
	} else {
		add("daemon", Done, "the Potluck daemon is running", "")
	}
	add(setupCheck(f, out.State, st.Lending.Sizes > 0))
	lendingCheck(&out, f, st)
	if m := s.Menu; m != nil {
		switch {
		case m.Path == "":
			add("menu", Todo, "the menu bar item is not installed (optional: it shows what this Mac lends and pauses it without a terminal)", s.InstallMenu)
		case !m.Seen.IsZero() && now().Sub(m.Seen) <= MenuSeenWithin:
			add("menu", Done, fmt.Sprintf("the menu bar item is running: it read this status at %s (%s)", m.Seen.In(locOf(f)).Format("15:04:05"), m.Path), "")
		case m.Seen.IsZero():
			add("menu", Todo, "the menu bar item is installed at "+m.Path+" but has never read this status, so it is not running", m.Open)
		default:
			add("menu", Todo, fmt.Sprintf("the menu bar item is installed at %s but has not read this status since %s, so it is not running now", m.Path, m.Seen.In(locOf(f)).Format("Jan 2 15:04")), m.Open)
		}
		if m.Path != "" {
			add("login", Unknown, "starting the menu bar item at login: Potluck cannot read your Login Items; to have it start at every login, add Potluck Menu in System Settings > General > Login Items", "")
		}
	}
	if u := s.Updates; u != nil {
		switch {
		case u.Err != "":
			add("updates", Unknown, "the daily update's state cannot be read: "+u.Err, "")
		case u.Foreign != "":
			add("updates", Done, "another updater runs this program's upgrade ("+u.Foreign+"); Potluck leaves it as it is", "")
		case u.Enabled && u.Loaded:
			add("updates", Done, "the daily update is on: it installs Potluck's newest signed release", "")
		case u.Enabled:
			add("updates", Todo, "the daily update is installed but not loaded, so it does not run", s.Upgrade)
		default:
			add("updates", Todo, "the daily update is off (optional: your choice; Potluck never turns it on by itself)", s.Upgrade)
		}
		if s.Menu != nil {
			add("coverage", Info, "the daily update covers Potluck itself only: to update the menu bar item, run its installer again ("+s.InstallMenu+"); Tart and Softnet are updated the way you installed them, and "+s.Doctor+" says which versions this Mac needs", "")
		}
	}
	for _, c := range out.Checks {
		if (c.State == Todo || c.State == Blocked) && c.Action != "" {
			out.Next = c.Action
			break
		}
	}
	return out
}

// RunAgain is how to run the last setup again (share: the command when the
// record names none): "run again: <command>", or, after a flag this
// machine's layer refused, "run again without --docker=true: <command
// without it>".
func RunAgain(r *hostsetup.Run, share string) string {
	again := share
	if r != nil && r.Again != "" {
		again = r.Again
	}
	if b := r.Blocked(); b != nil && b.Flag != "" {
		if b.Fix == "" {
			return "run again without " + b.Flag + " (the layer this machine shares on does not take it): " + again
		}
		return "run again " + b.Fix + " (the layer this machine shares on does not take " + b.Flag + "): " + again
	}
	return "run again: " + again
}

// setupCheck is the last host run as a check; shares: this machine shares
// sizes already.
func setupCheck(f Facts, state string, shares bool) (id, st, detail, action string) {
	r := f.Setup.Run
	again := f.Share
	if r != nil && r.Again != "" {
		again = r.Again
	}
	switch state {
	case SetupUnreadable:
		return "setup", Unknown, "the last setup's record cannot be read: " + f.Setup.RunErr, ""
	case SetupNone:
		return "setup", Todo, "this machine has not been set up to lend yet", f.Share
	case SetupRunning:
		c := r.Current()
		switch {
		case c == nil && len(r.Stages) == 0: // just started
			return "setup", Running, "setting up now: starting", ""
		case c == nil: // between two stages
			c = &r.Stages[len(r.Stages)-1]
		}
		return "setup", Running, "setting up now: " + hostsetup.Line(c.Name, c.Result, c.Detail), ""
	case SetupBlocked:
		b := r.Blocked()
		detail := fmt.Sprintf("setup blocked at %s (%d of %d): %s", hostsetup.Label(b.Name), hostsetup.Number(b.Name), len(hostsetup.Order), b.Detail)
		if shares {
			detail = fmt.Sprintf("the last setup run was blocked at %s (%d of %d): %s; what was shared before still lends",
				hostsetup.Label(b.Name), hostsetup.Number(b.Name), len(hostsetup.Order), b.Detail)
		}
		if b.Flag != "" {
			return "setup", Blocked, detail, RunAgain(r, f.Share)
		}
		return "setup", Blocked, detail, "fix what the " + hostsetup.Label(b.Name) + " stage says, then " + RunAgain(r, f.Share)
	case SetupInterrupted:
		at := "before its first stage"
		if n := len(r.Stages); n > 0 {
			at = "at " + hostsetup.Label(r.Stages[n-1].Name)
		}
		return "setup", Blocked, "the last setup stopped " + at + " before it finished (a pull that stopped resumes)", again
	}
	return "setup", Done, fmt.Sprintf("set up: all %d stages succeeded (%s)", len(hostsetup.Order), strings.Join(stageNames(), ", ")), ""
}

// lendingCheck adds what this machine lends now and when it may lend:
// eligibility (idle time, the switch) apart from joining and installing.
func lendingCheck(out *Setup, f Facts, st Status) {
	l := st.Lending
	switch {
	case f.Daemon != "":
		return
	case l.Sizes == 0 && f.ListErr != "":
		out.Checks = append(out.Checks, Check{ID: "lending", State: Unknown, Detail: "what this machine shares cannot be read: " + f.ListErr})
		return
	case l.Sizes == 0:
		c := Check{ID: "lending", State: Todo, Detail: "this machine shares no size with a pool yet"}
		if out.State == SetupNone || out.State == SetupSucceeded {
			c.Action = f.Share
		}
		out.Checks = append(out.Checks, c)
		return
	}
	out.Checks = append(out.Checks, Check{ID: "lending", State: Done, Detail: fmt.Sprintf("shares %s with its pools, up to %s at once", plural(l.Sizes, "size"), plural(l.Lent, "core"))})
	idle := 0
	for _, t := range f.Targets {
		if t.Kind == "box" && t.RunTarget.Host == "" {
			var n int
			if _, err := fmt.Sscan(t.Config["reclaim_idle_s"], &n); err == nil {
				idle = max(idle, n)
			}
		}
	}
	when := "it may lend any time, whether or not you are using it (" + f.Share + " --when away: only while you are away)"
	if idle > 0 {
		when = fmt.Sprintf("it lends only while you are away: a job starts after %d s with no keyboard or pointer use and ends when you come back (%s --when any: any time)", idle, f.Share)
	}
	now := "now it takes work"
	switch {
	case st.Mode == protocol.HostClosed || st.Mode == protocol.HostDrain:
		now = "now it is paused (" + st.Mode + "), so nothing new starts"
	case st.State == protocol.HostAccepting && st.Refused != nil:
		now = "now its admission controller refuses new work"
	case st.State != protocol.HostAccepting:
		now = "now it waits: someone is using this machine, or your lending rules say not now"
	}
	out.Checks = append(out.Checks, Check{ID: "when", State: Info, Detail: when + "; " + now})
}

func stageNames() []string {
	var out []string
	for _, s := range hostsetup.Order {
		out = append(out, hostsetup.Label(s))
	}
	return out
}

func locOf(f Facts) *time.Location {
	if f.Loc == nil {
		return time.UTC
	}
	return f.Loc
}
