package menubar

// The setup checklist: what is set up on this machine and what is not, from
// facts read now (the last host run's stages, the shared sizes, the menu bar
// item, the daily update), and the one thing to do next. potluck host status
// prints it and carries it in its JSON (setup), and the menu's title and note
// say the same when setup is going on or blocked.

import (
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/sengirunners/potluck/hostprobe"
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
	InstallMenu      string // installs the menu bar item
	Upgrade          string // turns the daily update on
	Doctor           string // says which Tart and Softnet this Mac needs
	SoftnetInstall   string // installs this Mac's Softnet build
	SoftnetUninstall string // removes the managed Softnet
	// TartInstall installs the Tart Potluck pins, set only with Mac and only
	// when running it would put a Tart on this Mac or update Potluck's own.
	TartInstall string
	// Exe is this program (symlinks resolved), InstallDir where Potluck is
	// installed, and BundledRelease a signed release the app carries
	// (POTLUCK_BUNDLED_RELEASE; "" for none); InstallBundled is the
	// command that installs it into InstallDir.
	Exe, InstallDir, BundledRelease, InstallBundled string
	// Mac is this Mac's preflight, part by part (host status --probe on a
	// Mac); nil: not read.
	Mac *hostprobe.MacParts
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
// that completes it, as words for a terminal; Do is the same thing for a
// program (the menu bar app), so it never reads Action to know what to run.
// Every check with an Action has a Do, but one: a setup blocked on a
// refused setting an earlier share kept that no flag of the command turns
// off, where running it again fails the same way.
type Check struct {
	ID     string `json:"id"`
	State  string `json:"state"`
	Detail string `json:"detail"`
	Action string `json:"action,omitempty"`
	Do     *Do    `json:"do,omitempty"`

	// For the setup window (SetupView), whose reader may never have used a
	// terminal: words is the check's line there ("": it has none), help
	// what to do about it when it is the next step, and optional that
	// lending works without it.
	words, help string
	optional    bool
	// other is another way to do it, a small button on its own line.
	other *Do
}

// say is c with its setup window words.
func (c Check) say(words, help string) Check {
	c.words, c.help = words, help
	return c
}

// Do is a check's action for a program. Exactly one of Args, URL and App
// says what to do.
type Do struct {
	// Label is the button's words.
	Label string `json:"label"`
	// Args are potluck's arguments that do it, run as the user (no shell).
	Args []string `json:"args,omitempty"`
	// Invite is that the app asks for an invite code and appends it to Args
	// as their last argument (they end in "--"); it is never in Args. When
	// the person gives none, Args run as they are, as the action's words
	// say: a host run with no argument shares with the one pool this machine
	// is in.
	Invite bool `json:"invite,omitempty"`
	// URL is a page to open (in the browser, or a file URL the system
	// opens) instead of running anything.
	URL string `json:"url,omitempty"`
	// App is an act only the app itself can do. No check gives one yet.
	App string `json:"app,omitempty"`
	// Auto is that the app may do it without a click. No check sets it yet.
	Auto bool `json:"auto,omitempty"`
}

// The arguments the checklist's Do runs: the same commands as SetupFacts'
// and Facts' words, as the program's arguments below its root. The app runs
// potluck, or a program built on Potluck with its commands at the root, as
// it does for the menu's own items (Menu.Windows); a record's again_args are
// spelled the same way.
var (
	startArgs   = []string{"daemon", "--detach"}
	upgradeArgs = []string{"--json", "upgrade", "enable"}
	// hostArgs run a setup with no record in the background; the invite
	// is appended after the "--", so a paste that starts with "-" is never
	// read as a flag. --yes: a press asks for the run, so one where this
	// machine already shares sizes goes on rather than only saying what it
	// would do.
	// --no-sudo: the app has no terminal to ask at, so a root step the run
	// stops at is its answer (admin_step), never skipped.
	hostArgs = []string{"--json", "host", "--yes", "--no-sudo", "--detach", "--"}
)

// menuInstallDoc is where installing the menu bar item is written down:
// its installer is a shell pipeline, not potluck's arguments.
const menuInstallDoc = "https://github.com/sengirunners/potluck/blob/main/docs/menubar.md#install-it"

// StageName is one stage of a host run with how a person reads it.
type StageName struct {
	Name  string `json:"name"`
	Label string `json:"label"`
}

// Setup is the checklist as host status answers it.
type Setup struct {
	// State is the last host run's: none, running, succeeded, blocked or
	// interrupted (it stopped before it ended), or unreadable.
	State string `json:"state"`
	// Stages are that run's stages. A stage's open_url shows only while
	// the run goes on.
	Stages []hostsetup.Stage `json:"stages,omitempty"`
	// AgainArgs and NeedsInvite are that run's: the program's arguments
	// that run the setup again, and whether the invite code must be
	// appended to them (hostsetup.Run).
	AgainArgs   []string `json:"again_args,omitempty"`
	NeedsInvite bool     `json:"needs_invite,omitempty"`
	// StageOrder is every stage a host run goes through, in order
	// (hostsetup.Order), reached or not.
	StageOrder []StageName `json:"stage_order"`
	Checks     []Check     `json:"checks"`
	// Next is the one thing to do now ("" when nothing Potluck can read is
	// left), and NextID the id of the check it is the action of. A blocked
	// check with no action stops there: NextID is its id and Next is
	// empty, since nothing Potluck runs fixes it (its detail says what to
	// do).
	Next   string `json:"next,omitempty"`
	NextID string `json:"next_id,omitempty"`
	// View is the setup window, in words for someone who has never used a
	// terminal.
	View SetupView `json:"view"`
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
	for _, n := range hostsetup.Order {
		out.StageOrder = append(out.StageOrder, StageName{Name: n, Label: hostsetup.Label(n)})
	}
	if s.Run != nil {
		out.Stages = slices.Clone(s.Run.Stages)
		for i := range out.Stages {
			// A page nobody waits on any more, or one the app should not
			// open (only an https page is a login).
			if u := out.Stages[i].OpenURL; out.State != SetupRunning || !strings.HasPrefix(u, "https://") {
				out.Stages[i].OpenURL = ""
			}
		}
		out.AgainArgs, out.NeedsInvite = s.Run.AgainArgs, s.Run.NeedsInvite
	}
	add := func(c Check) { out.Checks = append(out.Checks, c) }
	if s.Mac != nil {
		mac := macCheck(s.Mac)
		out.Checks = append(out.Checks, mac, potluckCheck(s))
		if mac.State != Blocked {
			out.Checks = append(out.Checks, probeChecks(s.Mac, s, st.Lending.Sizes > 0 || f.ListErr != "" || f.Daemon != "")...)
		}
	} else {
		out.Checks = append(out.Checks, potluckCheck(s))
	}
	if f.Daemon != "" {
		add(Check{ID: "daemon", State: Todo, Detail: "the Potluck daemon is not running: " + f.Daemon, Action: f.Start, Do: &Do{Label: "Start Potluck", Args: startArgs}}.
			say("Potluck isn't running", "Potluck runs quietly in the background. Start it to go on."))
	} else {
		add(Check{ID: "daemon", State: Done, Detail: "the Potluck daemon is running"}.say("Potluck is running", ""))
	}
	out.Checks = append(out.Checks, setupCheck(f, out.State, st.Lending.Sizes > 0))
	lendingCheck(&out, f, st)
	if m := s.Menu; m != nil {
		menu := Check{ID: "menu", State: Todo, Action: m.Open, optional: true} // no words: the window is the menu bar item's own
		switch {
		case m.Path == "":
			menu.Detail, menu.Action = "the menu bar item is not installed (optional: it shows what this Mac lends and pauses it without a terminal)", s.InstallMenu
			menu.Do = &Do{Label: "How to install it", URL: menuInstallDoc}
		case !m.Seen.IsZero() && now().Sub(m.Seen) <= MenuSeenWithin:
			menu.State, menu.Action = Done, ""
			menu.Detail = fmt.Sprintf("the menu bar item is running: it read this status at %s (%s)", m.Seen.In(locOf(f)).Format("15:04:05"), m.Path)
		case m.Seen.IsZero():
			menu.Detail, menu.Do = "the menu bar item is installed at "+m.Path+" but has never read this status, so it is not running", openApp(m.Path)
		default:
			menu.Detail = fmt.Sprintf("the menu bar item is installed at %s but has not read this status since %s, so it is not running now", m.Path, m.Seen.In(locOf(f)).Format("Jan 2 15:04"))
			menu.Do = openApp(m.Path)
		}
		add(menu)
		if m.Path != "" {
			add(Check{ID: "login", State: Unknown, optional: true, Detail: "starting the menu bar item at login: Potluck cannot read your Login Items; to have it start at every login, add Potluck Menu in System Settings > General > Login Items"}.
				say("To have this menu appear every time you log in, add Potluck Menu in System Settings > General > Login Items", ""))
		}
	}
	if u := s.Updates; u != nil {
		c := Check{ID: "updates", State: Done, optional: true}
		turnOn := &Do{Label: "Turn on daily updates", Args: upgradeArgs}
		switch {
		case u.Err != "":
			c.State, c.Detail = Unknown, "the daily update's state cannot be read: "+u.Err
			c = c.say("Couldn't check whether Potluck updates itself", "")
		case u.Foreign != "":
			c.Detail = "another updater runs this program's upgrade (" + u.Foreign + "); Potluck leaves it as it is"
			c = c.say("Something else on this Mac keeps Potluck up to date", "")
		case u.Enabled && u.Loaded:
			c.Detail = "the daily update is on: it installs Potluck's newest signed release"
			c = c.say("Potluck updates itself every day", "")
		case u.Enabled:
			c.State, c.Detail, c.Action = Todo, "the daily update is installed but not loaded, so it does not run", s.Upgrade
			c.Do = &Do{Label: "Turn daily updates back on", Args: upgradeArgs}
			c = c.say("Daily updates have stopped", "")
		default:
			c.State, c.Detail, c.Action, c.Do = Todo, "the daily update is off (optional: your choice; Potluck never turns it on by itself)", s.Upgrade, turnOn
			c = c.say("Potluck doesn't update itself", "")
		}
		add(c)
		if s.Menu != nil {
			add(Check{ID: "coverage", State: Info, optional: true, Detail: "the daily update covers Potluck itself only: to update the menu bar item, run its installer again (" + s.InstallMenu + "); Tart and Softnet are updated the way you installed them, and " + s.Doctor + " says which versions this Mac needs"})
		}
	}
	// The first step left that has an action is next. A blocked check with
	// none (this Mac cannot lend, or the person must free disk or update
	// what Potluck cannot) stops the walk: what follows cannot help, so
	// NextID names it and Next is empty.
	for _, c := range out.Checks {
		if (c.State == Todo || c.State == Blocked || c.State == Running) && c.Action != "" {
			out.Next, out.NextID = c.Action, c.ID
			break
		}
		if c.State == Blocked {
			out.NextID = c.ID
			break
		}
	}
	out.View = setupView(f, st, out)
	return out
}

// openApp opens the app at path (a file URL the system opens).
func openApp(path string) *Do {
	return &Do{Label: "Open the menu bar item", URL: (&url.URL{Scheme: "file", Path: path}).String()}
}

// againDo runs the last host run again in the background (label: the
// button's words): its own arguments when the record has them, else
// hostArgs, the invite asked for. Like hostArgs it goes on where this
// machine already shares sizes (--yes) and hands back a root step
// (--no-sudo).
func againDo(r *hostsetup.Run, label string) *Do {
	if r == nil || len(r.AgainArgs) == 0 { // never run, or a record written before again_args
		return &Do{Label: label, Args: hostArgs, Invite: true}
	}
	args := append([]string{"--json"}, r.AgainArgs...) // a record's again_args never hold --json or --detach (cli startSetup)
	for _, flag := range []string{"--yes", "--no-sudo"} {
		if !slices.ContainsFunc(args, func(a string) bool { return a == flag || a == flag+"=true" }) {
			args = append(args, flag)
		}
	}
	args = append(args, "--detach")
	if r.NeedsInvite {
		args = append(args, "--")
	}
	return &Do{Label: label, Args: args, Invite: r.NeedsInvite}
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
func setupCheck(f Facts, state string, shares bool) Check {
	r := f.Setup.Run
	again := f.Share
	if r != nil && r.Again != "" {
		again = r.Again
	}
	c := func(st, detail, action string, do *Do) Check {
		return Check{ID: "setup", State: st, Detail: detail, Action: action, Do: do}
	}
	const tellSomeone = "If it stops again, press Copy details and send what it copies to the person who invited you."
	switch state {
	case SetupUnreadable:
		return c(Unknown, "the last setup's record cannot be read: "+f.Setup.RunErr, "", nil).say("Couldn't tell how the last setup went", "")
	case SetupNone:
		if shares { // set up before a run kept a record, or the record is gone
			return c(Done, "this machine lends; it has no record of how it was set up", "", nil).say("Set up to lend", "")
		}
		if f.ListErr != "" {
			return c(Unknown, "this machine has no record of a setup, and what it shares cannot be read", "", nil).say("Couldn't tell whether this Mac is set up", "")
		}
		words, help := "Not set up yet", ""
		if f.Daemon != "" { // a Mac that lends and lost its record looks the same
			words = "Setup can be checked once Potluck is running"
		}
		if endedEarly(f) {
			help = "The last try stopped early; if it happens again, press Copy details and send what it copies to the person who invited you. "
		}
		return c(Todo, "this machine has not been set up to lend yet", f.Share, againDo(nil, "Set up this Mac")).say(words, help+"Paste the invite you were sent, then press the button. Setting up downloads what jobs need and tests it, which can take an hour or more. You can keep using this Mac meanwhile.")
	case SetupRunning:
		cur := r.Current()
		switch {
		case cur == nil && len(r.Stages) == 0: // just started
			return c(Running, "setting up now: starting", "", nil).say("Setting up now", "")
		case cur == nil: // between two stages
			cur = &r.Stages[len(r.Stages)-1]
		}
		detail := "setting up now: " + hostsetup.Line(cur.Name, cur.Result, cur.Detail)
		if u := cur.OpenURL; cur.Result == hostsetup.Running && strings.HasPrefix(u, "https://") { // the run waits on this page
			return c(Running, detail, "open this page and sign in for the setup to go on: "+u, &Do{Label: "Sign in", URL: u}).say("Waiting for you to sign in", "Press the button and sign in on the page that opens in your browser. Setup goes on by itself after that.")
		}
		return c(Running, detail, "", nil).say("Setting up now", "")
	case SetupBlocked:
		b := r.Blocked()
		detail := fmt.Sprintf("setup blocked at %s (%d of %d): %s", hostsetup.Label(b.Name), hostsetup.Number(b.Name), len(hostsetup.Order), b.Detail)
		if shares {
			detail = fmt.Sprintf("the last setup run was blocked at %s (%d of %d): %s; what was shared before still lends",
				hostsetup.Label(b.Name), hostsetup.Number(b.Name), len(hostsetup.Order), b.Detail)
		}
		words, try := "The last setup stopped before it finished", againDo(r, "Try again")
		switch {
		case b.Flag != "" && b.Fix == "": // an earlier share kept the refused setting and no flag turns it off: running again fails the same way
			return c(Blocked, detail, RunAgain(r, f.Share), nil).say(words, stopped[b.Name]+" Trying again won't fix it. Press Copy details and send what it copies to the person who invited you.")
		case b.Flag != "":
			return c(Blocked, detail, RunAgain(r, f.Share), try).say(words, stopped[b.Name]+" One of its settings doesn't work on this Mac; trying again leaves that setting out."+inviteAgain(r)+" "+tellSomeone)
		}
		return c(Blocked, detail, "fix what the "+hostsetup.Label(b.Name)+" stage says, then "+RunAgain(r, f.Share), try).say(words, stopped[b.Name]+inviteAgain(r)+" "+tellSomeone)
	case SetupInterrupted:
		at := "before its first stage"
		if n := len(r.Stages); n > 0 {
			at = "at " + hostsetup.Label(r.Stages[n-1].Name)
		}
		return c(Blocked, "the last setup stopped "+at+" before it finished (a pull that stopped resumes)", again, againDo(r, "Continue setup")).
			say("The last setup was cut off", "Setup was cut off before it finished. Press the button to go on from where it stopped."+inviteAgain(r))
	}
	return c(Done, fmt.Sprintf("set up: all %d stages succeeded (%s)", len(hostsetup.Order), strings.Join(stageNames(), ", ")), "", nil).say("Set up to lend", "")
}

// lendingCheck adds what this machine lends now and when it may lend:
// eligibility (idle time, the switch) apart from joining and installing.
func lendingCheck(out *Setup, f Facts, st Status) {
	l := st.Lending
	switch {
	case f.Daemon != "":
		return
	case l.Sizes == 0 && f.ListErr != "":
		out.Checks = append(out.Checks, Check{ID: "lending", State: Unknown, Detail: "what this machine shares cannot be read: " + f.ListErr}.say("Couldn't tell what this Mac lends just now", ""))
		return
	case l.Sizes == 0:
		c := Check{ID: "lending", State: Todo, Detail: "this machine shares no size with a pool yet"}.say("Not lending anything yet", "This Mac was set up but lends nothing now."+inviteAgain(f.Setup.Run)+" Press the button to lend it again.")
		if out.State == SetupNone || out.State == SetupSucceeded {
			c.Action, c.Do = f.Share, againDo(f.Setup.Run, "Lend this Mac")
		}
		out.Checks = append(out.Checks, c)
		return
	}
	out.Checks = append(out.Checks, Check{ID: "lending", State: Done, Detail: fmt.Sprintf("shares %s with its pools, up to %s at once", plural(l.Sizes, "size"), plural(l.Lent, "core"))}.
		say(fmt.Sprintf("Lends up to %s at once", plural(l.Lent, "processor core")), ""))
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
	words := "Lends at any time, even while you use it"
	if idle > 0 {
		when = fmt.Sprintf("it lends only while you are away: a job starts after %d s with no keyboard or pointer use and ends when you come back (%s --when any: any time)", idle, f.Share)
		words = "Lends only while you are away from this Mac"
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
	out.Checks = append(out.Checks, Check{ID: "when", State: Info, Detail: when + "; " + now}.say(words, ""))
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
