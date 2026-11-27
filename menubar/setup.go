package menubar

// The setup checklist: what is set up on this machine and what is not, from
// facts read now (the last host run's stages, the shared sizes, the menu bar
// item, the daily update), and the one thing to do next. potluck host status
// prints it and carries it in its JSON (setup), and the menu's title and note
// say the same when setup is going on or blocked.

import (
	"cmp"
	"fmt"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/hostprobe"
	"github.com/sengirunners/potluck/hostsetup"
	"github.com/sengirunners/potluck/protocol"
)

// SetupFacts are what the checklist is made from, beside the lending facts.
type SetupFacts struct {
	// AutoStart is that the app may start the daemon by itself: it is
	// Potluck's own and no person stopped it.
	AutoStart bool
	// Stopped is that `potluck daemon stop` stopped it and nothing started
	// it since.
	Stopped bool
	// Version is this program's version.
	Version string
	// Run is the last host run's record (nil: none); RunErr why it could
	// not be read; RunLive that a host run holds the home's lock now.
	Run     *hostsetup.Run
	RunErr  string
	RunLive bool
	// Pools are the pools this machine is in now, when PoolsRead: the
	// record says what a run did, and they what still stands.
	Pools     []Pool
	PoolsRead bool
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

// Pool is a pool this machine is in. Joined is when it joined (zero: the
// pool runs on this machine), at the machine with that name and key
// fingerprint.
type Pool struct {
	Slug, ID             string
	Joined               time.Time
	Machine, Fingerprint string
	// Lends is that it joined it as a machine that lends.
	Lends bool
}

// runNow is the last host run as running it again now must go: a pool it
// joined and was cut off before it noted (its invite is spent) is named, one
// renamed since goes by its name now, and one this machine has since left
// needs an invite.
func runNow(s SetupFacts) *hostsetup.Run {
	r := s.Run
	if r == nil || !s.PoolsRead {
		return r
	}
	if p := r.Pool(); p != "" {
		i := slices.IndexFunc(s.Pools, func(x Pool) bool { return r.PoolID != "" && x.ID == r.PoolID || r.PoolID == "" && x.Slug == p })
		switch {
		case i < 0:
			return r.WithoutPool()
		case s.Pools[i].Slug != p:
			return r.Renamed(s.Pools[i].Slug)
		}
		return r
	}
	if !r.NeedsInvite {
		return r
	}
	if p := joinedBy(r, s.Pools); p != nil {
		return r.WithPool(p.Slug)
	}
	c := *r
	if c.Else == "" {
		if p := lendsTo(s.Pools); p != nil {
			c.Else, c.ElseID = p.Slug, p.ID
		}
		return &c
	}
	i := slices.IndexFunc(s.Pools, func(p Pool) bool { return c.ElseID != "" && p.ID == c.ElseID || c.ElseID == "" && p.Slug == c.Else })
	if i < 0 { // left since: no other stands in for it
		c.Else, c.ElseID, c.Lost = "", "", true
		return &c
	}
	c.Else, c.ElseID = s.Pools[i].Slug, s.Pools[i].ID
	return &c
}

// A pool is r's invite's only if it was joined while r could join: from its
// Joining until it ended, or, of a run cut off, until it would have given the
// sign-in up (joinMost), and a little after (joinSlack): the daemon ends a
// join whose answer nobody waited for.
const (
	joinMost  = 12 * time.Minute
	joinSlack = time.Minute
)

// joinedBy is the one of pools that r spent its invite on (nil: none, or not
// known).
func joinedBy(r *hostsetup.Run, pools []Pool) *Pool {
	if r.Joining == nil {
		return nil
	}
	began, err := time.Parse(time.RFC3339, r.Joining.At)
	if err != nil {
		return nil
	}
	until := began.Add(joinMost)
	if ended, err := time.Parse(time.RFC3339, r.Ended); err == nil {
		until = ended
	}
	var at []Pool
	for _, p := range pools {
		if r.Joining.Names(p.Fingerprint, p.Machine) && !p.Joined.Before(began) && !p.Joined.After(until.Add(joinSlack)) {
			at = append(at, p)
		}
	}
	if len(at) != 1 {
		return nil
	}
	return &at[0]
}

// lendsTo is the pool a run that needs an invite may go on with instead of
// one: the only one this machine joined to lend, else the only one it is in
// (nil: none, or several).
func lendsTo(pools []Pool) *Pool {
	for _, only := range [][]Pool{slices.DeleteFunc(slices.Clone(pools), func(p Pool) bool { return !p.Lends }), pools} {
		if len(only) == 1 {
			return &only[0]
		}
		if len(only) > 1 {
			break
		}
	}
	return nil
}

// newRun is the run a setup with no record to go by starts: it needs an
// invite, or goes on with the pool lendsTo finds.
func newRun(s SetupFacts) *hostsetup.Run {
	r := &hostsetup.Run{AgainArgs: []string{"host"}, NeedsInvite: true}
	if p := lendsTo(s.Pools); s.PoolsRead && p != nil {
		r.Else, r.ElseID = p.Slug, p.ID
	}
	return r
}

// pressRun is the run the setup's button starts: the last one again, or a new
// one when no record says how (none, or one written before again_args).
func pressRun(s SetupFacts) *hostsetup.Run {
	if s.Run == nil || len(s.Run.AgainArgs) == 0 {
		return newRun(s)
	}
	return s.Run
}

// Stranded reports that t is shared only with pools this machine is in no
// more (each ended, or left): nothing can use it.
func (s SetupFacts) Stranded(t computeapi.RunTargetView) bool {
	return s.PoolsRead && len(t.Shared) > 0 && !slices.ContainsFunc(s.Pools, func(p Pool) bool {
		return slices.Contains(t.Shared, p.Slug) || slices.Contains(t.Shared, p.ID)
	})
}

// AppName is the Mac app's bundle, and AppNames every name it is installed
// under: this one, then the one it had before. Its executable (PotluckMenu)
// and bundle id (sh.potluck.menu) are the same under both.
const AppName = "Potluck.app"

var AppNames = []string{AppName, "Potluck Menu.app"}

// MenuApp is the menu bar item as this machine shows it.
type MenuApp struct {
	// Path is where the app is installed ("" when it is not).
	Path string
	// Seen is when the app last asked for this status: the proof that it
	// runs and draws (zero: never). Opening the app proves nothing.
	Seen time.Time
	// Open is the command that opens it.
	Open string
	// Login is the app's login item, as the app says it ("" when it has
	// not: an older app, or one that never asked).
	Login string
	// Old are the places an app under its old name asked from, still there.
	Old []string
}

// The app's login item, as only the app can read it: on, off, or refused
// (turned off in System Settings, where only the person turns it on).
const (
	LoginOn      = "on"
	LoginOff     = "off"
	LoginRefused = "refused"
)

// What only the app can do (Do.App): turn its login item on or off, or
// open the Login Items settings.
const (
	ActLoginOn       = "login_item"
	ActLoginOff      = "login_item_off"
	ActLoginSettings = "login_item_settings"
)

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
	// waits is a blocked check that waits on Potluck finishing something of
	// its own, not on this Mac: the steps before it that do not need it
	// (installing Potluck and Tart) are still worth doing.
	waits bool
	// other is another way to do it, or of a done one the way to undo it: a
	// small button on its own line.
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
	// InviteArgs, set only beside Args, is that the app asks for an invite
	// code. When the person gives one, these run instead of Args, with the
	// code and a newline on the run's stdin: it is never an argument, where
	// any user of this machine reads it. When they give none, Args run, with
	// nothing on stdin: with no record, a host run with no argument, which
	// shares with the one pool this machine is in; else one that goes on with
	// the pool the words name, or refuses (InsteadFlag).
	InviteArgs []string `json:"invite_args,omitempty"`
	// Invite, set with InviteArgs, is only for an app built before it, which
	// appends the code to Args: potluck updates itself and the app does not,
	// and without this its window would have no place for an invite.
	Invite bool `json:"invite,omitempty"`
	// URL is a page to open (in the browser, or a file URL the system
	// opens) instead of running anything.
	URL string `json:"url,omitempty"`
	// App is an act only the app itself can do (ActLoginOn and the rest).
	App string `json:"app,omitempty"`
	// Auto is that the app may do it without a click, once each time it
	// starts.
	Auto bool `json:"auto,omitempty"`
}

// The arguments the checklist's Do runs: the same commands as SetupFacts'
// and Facts' words, as the program's arguments below its root. The app runs
// potluck, or a program built on Potluck with its commands at the root, as
// it does for the menu's own items (Menu.Windows); a record's again_args are
// spelled the same way.
var (
	startArgs   = []string{"--json", "daemon", "--detach"}
	upgradeArgs = []string{"--json", "upgrade", "enable"}
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
	// Started is when that run began: the same until another run begins, so
	// a reader can tell whose result State and the view are.
	Started string `json:"started,omitempty"`
	// Stages are that run's stages. A stage's open_url shows only while
	// the run goes on.
	Stages []hostsetup.Stage `json:"stages,omitempty"`
	// AgainArgs and NeedsInvite are that run's: the program's arguments
	// that run the setup again, and whether they need the invite
	// code (hostsetup.Run).
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
	// OldApps are the app's places under its old name, as the app noted them
	// when it ran there: the renamed app moves them to the Trash.
	OldApps []string `json:"old_apps,omitempty"`
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
		out.Started, out.AgainArgs, out.NeedsInvite = s.Run.Started, s.Run.AgainArgs, s.Run.NeedsInvite
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
		// A Mac set up to lend, or part way there, starts it again when the
		// app starts (after a restart nothing else does), unless a person
		// stopped it; one never set up waits for a press.
		add(Check{ID: "daemon", State: Todo, Detail: "the Potluck daemon is not running: " + f.Daemon, Action: f.Start, Do: &Do{Label: "Start Potluck", Args: startArgs, Auto: out.State != SetupNone && s.AutoStart && !notInstalled(s)}}.
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
			add(loginCheck(m))
			out.OldApps = m.Old
		}
	}
	if u := s.Updates; u != nil && !notInstalled(s) { // its job would run that copy
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
	// What only the app can do is no step of a terminal's.
	for _, c := range out.Checks {
		if c.Do != nil && c.Do.App != "" {
			continue
		}
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

// loginCheck is the app's login item, optional: the setup window turns it on
// and off. Without it nothing starts Potluck after a restart.
func loginCheck(m *MenuApp) Check {
	const window = "in the Potluck menu, choose Set up and check…, then "
	c := Check{ID: "login", optional: true}
	if m.Login != "" && now().Sub(m.Seen) > MenuSeenWithin { // it may have changed since
		c.State, c.Detail = Unknown, "starting the menu bar item at login: only the app can tell, and it is not running (when it last ran: "+m.Login+")"
		return c
	}
	switch m.Login {
	case LoginOn:
		c.State, c.Detail = Done, "the menu bar item starts at login"
		return c.say("Potluck starts when you log in", "").or(&Do{Label: "Turn off", App: ActLoginOff})
	case LoginOff:
		c.State, c.Detail = Todo, "the menu bar item has not been set to start at login (optional: after a restart nothing else starts Potluck), unless you added it to Login Items yourself"
		c.Do = &Do{Label: "Start at login", App: ActLoginOn}
		c.Action = window + c.Do.Label
		return c.say("Potluck can open automatically when you log in", "")
	case LoginRefused:
		c.State, c.Detail = Todo, "the menu bar item's start at login is turned off in System Settings > General > Login Items"
		c.Do = &Do{Label: "Open Login Items", App: ActLoginSettings}
		c.Action = "turn Potluck on in System Settings > General > Login Items"
		return c.say("Allow Potluck in Login Items to open it automatically when you log in", "")
	}
	name := strings.TrimSuffix(filepath.Base(m.Path), ".app") // what Login Items calls it
	c.State, c.Detail = Unknown, "starting the menu bar item at login: the app has not said whether it does; to have it start at every login, add "+name+" in System Settings > General > Login Items"
	return c.say("To have this menu appear every time you log in, add "+name+" in System Settings > General > Login Items", "")
}

// openApp opens the app at path (a file URL the system opens).
func openApp(path string) *Do {
	return &Do{Label: "Open the menu bar item", URL: (&url.URL{Scheme: "file", Path: path}).String()}
}

// againDo runs the last host run again in the background (label: the
// button's words). A press asks for the run, so one where this machine
// already shares sizes goes on rather than only saying what it would do
// (--yes), and the app has no terminal to ask at, so a root step the run stops
// at is its answer (--no-sudo: admin_step), never skipped.
func againDo(r *hostsetup.Run, label string) *Do {
	full := func(again []string) []string {
		args := append([]string{"--json"}, again...) // a record's again_args never hold --json or --detach (cli startSetup)
		for _, flag := range []string{"--yes", "--no-sudo"} {
			if !slices.ContainsFunc(args, func(a string) bool { return a == flag || a == flag+"=true" }) {
				args = append(args, flag)
			}
		}
		return append(args, "--detach")
	}
	d := &Do{Label: label, Args: full(r.AgainArgs)}
	if !r.NeedsInvite {
		return d
	}
	d = askInvite(d)
	// With no invite given it goes on with the pool the words name, or
	// refuses: never with just any this machine is in. A flag, not the
	// argument: an app from before InviteArgs appends the invite as that.
	d.Args = append(full(r.AgainArgs), "--"+InsteadFlag+"="+r.Else, "--")
	return d
}

// InsteadFlag is host's flag for a run the checklist starts with no invite
// when its setup needs one: the pool it goes on with, or "" to refuse.
const InsteadFlag = "instead-of-invite"

// askInvite is d, a host run with no pool named, that takes an invite code.
// Args end in "--" so that a paste starting with "-", which an app built
// before InviteArgs appends to them, is never read as a flag.
func askInvite(d *Do) *Do {
	d.InviteArgs = append(slices.Clone(d.Args), "--invite-stdin")
	d.Args = append(slices.Clone(d.Args), "--")
	d.Invite = true
	return d
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
	const tellSomeone = "If it stops again, copy the details and send them to the person who invited you."
	switch state {
	case SetupUnreadable:
		detail, words := "the last setup's record cannot be read: "+f.Setup.RunErr, "Couldn't tell how the last setup went"
		if f.Setup.RunLive {
			return c(Running, "setting up now (its record cannot be read: "+f.Setup.RunErr+")", "", nil).say("Setting up now", "")
		}
		if shares || f.ListErr != "" || f.Daemon != "" { // what it lends says enough, or the daemon comes first
			return c(Unknown, detail, "", nil).say(words, "")
		}
		lost := newRun(f.Setup)
		return c(Todo, detail, f.Share, againDo(lost, "Set up again")).say(words, "Potluck couldn't read the last setup. Set up this Mac again to reuse what's already downloaded."+inviteAgain(lost)+" "+tellSomeone)
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
			help = "The last setup stopped early. If it happens again, copy the details and send them to the person who invited you. "
		}
		run, paste := newRun(f.Setup), "Paste your invite link, then choose Set up this Mac."
		if run.Else != "" {
			paste = "Paste your invite link, then choose Set up this Mac. Leave the invite field empty to lend to the group " + run.Else + "."
		}
		return c(Todo, "this machine has not been set up to lend yet", f.Share, againDo(run, "Set up this Mac")).say(words, help+paste+" Downloads and checks can take an hour or more. You can keep using your Mac.")
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
			return c(Running, detail, "open this page and sign in for the setup to go on: "+u, &Do{Label: "Sign in", URL: u}).say("Waiting for you to sign in", "Setup will continue after you sign in.")
		}
		return c(Running, detail, "", nil).say("Setting up now", "")
	case SetupBlocked:
		b := r.Blocked()
		detail := fmt.Sprintf("setup blocked at %s (%d of %d): %s", hostsetup.Label(b.Name), hostsetup.Number(b.Name), len(hostsetup.Order), b.Detail)
		if shares {
			detail = fmt.Sprintf("the last setup run was blocked at %s (%d of %d): %s; what was shared before still lends",
				hostsetup.Label(b.Name), hostsetup.Number(b.Name), len(hostsetup.Order), b.Detail)
		}
		words, try := "The last setup stopped before it finished", againDo(pressRun(f.Setup), "Try again")
		switch {
		case b.Flag != "" && b.Fix == "": // an earlier share kept the refused setting and no flag turns it off: running again fails the same way
			return c(Blocked, detail, RunAgain(r, f.Share), nil).say(words, stopped[b.Name]+" Copy details and send them to the person who invited you.")
		case b.Flag != "":
			return c(Blocked, detail, RunAgain(r, f.Share), try).say(words, stopped[b.Name]+" Choose Try again to skip the setting this Mac can't use."+inviteAgain(pressRun(f.Setup))+" "+tellSomeone)
		}
		return c(Blocked, detail, "fix what the "+hostsetup.Label(b.Name)+" stage says, then "+RunAgain(r, f.Share), try).say(words, stopped[b.Name]+seeTo[b.Name]+inviteAgain(pressRun(f.Setup))+" "+tellSomeone)
	case SetupInterrupted:
		at := "before its first stage"
		if n := len(r.Stages); n > 0 {
			at = "at " + hostsetup.Label(r.Stages[n-1].Name)
		}
		return c(Blocked, "the last setup stopped "+at+" before it finished (a pull that stopped resumes)", again, againDo(pressRun(f.Setup), "Continue setup")).
			say("Setup was interrupted", "Choose Continue setup to pick up where it stopped."+inviteAgain(pressRun(f.Setup)))
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
		c := Check{ID: "lending", State: Todo, Detail: "this machine shares no size with a pool yet"}.say("Not lending anything yet", "Setup is complete. Choose Lend this Mac to take jobs again."+inviteAgain(pressRun(f.Setup)))
		if out.State == SetupNone || out.State == SetupSucceeded {
			r := pressRun(f.Setup)
			c.Action, c.Do = f.Share, againDo(r, "Lend this Mac")
			if r.NeedsInvite {
				c.Action = cmp.Or(r.Again, f.Share)
			}
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
