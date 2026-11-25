package menubar

// The setup window, as the menu bar app draws it: every word of it, for
// someone who has never used a terminal. It is made from the checklist's
// checks (Check.words, Check.help), so the window and the terminal say the
// same facts. No command, path or file is in it but Details, which the
// window never shows, only copies for whoever helps.

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/sengirunners/potluck/hostsetup"
	"github.com/sengirunners/potluck/protocol"
)

// SetupView is the setup window.
type SetupView struct {
	// Heading is how this Mac stands, Lead the one thing to do about it or
	// what to expect, and Do its button (nil: nothing to press).
	Heading string `json:"heading"`
	Lead    string `json:"lead,omitempty"`
	Do      *Do    `json:"do,omitempty"`
	// Sections are the lines under it. Progress is the setup going on or
	// the one that stopped ("" title: none).
	Sections []ViewSection `json:"sections"`
	Progress ViewSection   `json:"progress"`
	// Details is what a person sends to whoever helps them, in the
	// terminal's words ("": nothing is wrong).
	Details string `json:"details,omitempty"`
	// Key changes when this window's steps may have, and never with what
	// --probe adds: the window asks plain host status often, and with
	// --probe only now and then or when Key is another.
	Key string `json:"key"`
}

// ViewSection is lines under a title.
type ViewSection struct {
	Title string    `json:"title"`
	Rows  []ViewRow `json:"rows,omitempty"`
}

// ViewRow is one line: its state (a Check state, or "" for not reached),
// its words, and its own button, if it has one.
type ViewRow struct {
	State string `json:"state"`
	Text  string `json:"text"`
	Do    *Do    `json:"do,omitempty"`
}

// stageWords are a host run's stages as the window names them, and stopped
// what it says when setup stopped in one.
var (
	stageWords = map[string]string{
		hostsetup.Preflight:  "Check this Mac",
		hostsetup.Images:     "Download what jobs need",
		hostsetup.Validation: "Test that jobs run safely",
		hostsetup.Join:       "Join the group that uses it",
		hostsetup.Capacity:   "Start lending",
	}
	stopped = map[string]string{
		hostsetup.Preflight:  "Setup stopped while checking this Mac.",
		hostsetup.Images:     "Setup stopped while downloading what jobs need.",
		hostsetup.Validation: "Setup stopped while testing that jobs run safely.",
		hostsetup.Join:       "Setup stopped while joining.",
		hostsetup.Capacity:   "Setup stopped just before lending could start.",
	}
)

// inviteAgain asks for the invite when running r again needs it.
func inviteAgain(r *hostsetup.Run) string {
	if r != nil && len(r.AgainArgs) > 0 && !r.NeedsInvite {
		return ""
	}
	return " Paste your invite again first; if it no longer works, ask for a new one."
}

// probeIDs are the checks --probe adds.
var probeIDs = []string{"mac", "tart", "softnet", "disk", "other"}

// setupView makes the window from the checklist.
func setupView(f Facts, st Status, s Setup) SetupView {
	next := viewNext(s)
	v := SetupView{Progress: progress(f, s)}
	shares := st.Lending.Sizes > 0
	v.Heading, v.Lead = viewHeading(f, st, s, next)
	if next != nil {
		v.Do = next.Do
	}
	main, extra := ViewSection{Title: "This Mac"}, ViewSection{Title: "Optional"}
	for _, c := range s.Checks {
		switch {
		case c.words == "", c.ID == "lending" && c.State == Todo && s.State != SetupSucceeded: // the setup line says it
		case c.optional:
			extra.Rows = append(extra.Rows, ViewRow{State: optionalState(c.State), Text: c.words, Do: c.Do})
		default:
			row := ViewRow{State: c.State, Text: c.words}
			if next != nil && next.ID == c.ID { // only beside the step it is another way to do
				row.Do = c.other
			}
			main.Rows = append(main.Rows, row)
		}
	}
	v.Sections = []ViewSection{main}
	if len(extra.Rows) > 0 {
		v.Sections = append(v.Sections, extra)
	}
	v.Details = viewDetails(f, st, s)
	plain := s // the key is the same with and without --probe
	plain.Checks = slices.DeleteFunc(slices.Clone(s.Checks), func(c Check) bool { return slices.Contains(probeIDs, c.ID) })
	v.Key = fmt.Sprintf("%s daemon=%t shares=%t listed=%t limits=%t mode=%s state=%s refused=%t early=%t", s.State, f.Daemon == "", shares,
		f.ListErr == "", f.LendErr == "", st.Mode, st.State, st.Refused != nil, endedEarly(f))
	if n := viewNext(plain); n != nil {
		v.Key += " next=" + n.ID
		if n.Do != nil {
			v.Key += " " + strings.Join(n.Do.Args, " ") + n.Do.URL
		}
	}
	return v
}

// optionalState draws an optional step that is open as a fact, not as
// something left to do.
func optionalState(state string) string {
	if state == Done {
		return Done
	}
	return Info
}

// viewNext is the check the window's heading and button are about: the
// setup while it goes on, else something in this Mac's way that nothing here
// fixes (so nobody installs things for nothing), else the first required one
// that is open; nil for none. An optional one never is.
func viewNext(s Setup) *Check {
	for i, c := range s.Checks {
		if c.ID == "setup" && c.State == Running {
			return &s.Checks[i]
		}
	}
	for i, c := range s.Checks {
		if c.State == Blocked && c.Do == nil && slices.Contains(probeIDs, c.ID) {
			return &s.Checks[i]
		}
	}
	for i, c := range s.Checks {
		if c.optional {
			continue
		}
		if c.State == Blocked || c.State == Todo && c.Do != nil {
			return &s.Checks[i]
		}
	}
	return nil
}

// viewHeading is how this Mac stands and what to do or expect.
func viewHeading(f Facts, st Status, s Setup, next *Check) (heading, lead string) {
	shares := st.Lending.Sizes > 0
	if next == nil {
		return lendingHeading(f, st)
	}
	lead = next.help
	switch {
	case next.ID == "mac" && !shares:
		heading = "This Mac can't lend"
	case next.State == Running && next.Do != nil:
		heading = "Sign in to finish setting up"
	case next.State == Running:
		heading, lead = "Setting up this Mac", "Nothing for you to do. This can take an hour or more; you can close this window and keep using this Mac."
	case next.ID == "daemon":
		heading = "Potluck isn't running"
	case next.ID == "setup" && next.State == Blocked && shares:
		heading = "This Mac is set up, but the last change didn't finish"
		lead += " What it lent before is still lent."
	case next.ID == "setup" && next.State == Blocked:
		heading = "Setup stopped"
	case shares, s.State == SetupNone && f.ListErr != "", s.State == SetupSucceeded && next.ID != "lending":
		heading = "This Mac needs attention"
	case next.ID == "lending":
		heading = "This Mac isn't lending"
	case s.State == SetupNone:
		heading = "This Mac isn't set up yet"
	default:
		heading = "Setup isn't finished"
	}
	return heading, lead
}

// lendingHeading is a Mac with no required step open.
func lendingHeading(f Facts, st Status) (heading, lead string) {
	const done = "It is set up; there is nothing else to do."
	switch {
	case f.ListErr != "" && st.Lending.Sizes == 0:
		return "Potluck can't tell what this Mac lends", "It didn't answer just now. Press Check again in a moment."
	case st.Lending.Sizes == 0:
		return "Potluck can't tell how setup went", "Its record of the last setup can't be read. Press Copy details and send what it copies to the person who invited you."
	case switchUnread(f, st):
		return "Lending is on hold", "Potluck can't read whether lending is on or off, so nothing new starts on this Mac. Press Copy details and send what it copies to the person who invited you."
	case st.Mode == protocol.HostClosed:
		return "Lending is paused", "Nothing new starts on this Mac until you choose Resume lending in the Potluck menu."
	case st.Mode == protocol.HostDrain:
		return "Lending is winding down", "Nothing new starts on this Mac while the jobs already here end. To go on lending, choose Keep lending in the Potluck menu."
	case f.LendErr != "":
		return "Lending is on hold", "Potluck can't read your lending limits, so nothing new starts on this Mac. Press Copy details and send what it copies to the person who invited you."
	case st.State == protocol.HostAccepting && st.Refused != nil:
		return "This Mac isn't taking jobs right now", "Potluck is holding new jobs back for now. The Potluck menu says why."
	case st.State != protocol.HostAccepting:
		return "This Mac is ready and waiting", "It takes no jobs while you use it or while your lending rules say not now. " + done
	}
	return "This Mac is lending", done
}

// switchUnread is that whether lending is on or off cannot be read.
func switchUnread(f Facts, st Status) bool {
	return f.Host == nil || st.Mode == protocol.HostUnreadable
}

// endedEarly is that a run in the background ended; with no record of one,
// before it kept one.
func endedEarly(f Facts) bool {
	return f.Background != nil && f.Background.State != "running" && f.Background.Last != ""
}

// progress is the setup going on, or the one that stopped: each stage.
func progress(f Facts, s Setup) ViewSection {
	if s.State != SetupRunning && s.State != SetupBlocked && s.State != SetupInterrupted {
		return ViewSection{}
	}
	v := ViewSection{Title: "The last setup"}
	if s.State == SetupRunning {
		v.Title = "Setup so far"
	}
	for _, n := range s.StageOrder {
		row := ViewRow{Text: stageWords[n.Name]}
		for _, stage := range s.Stages {
			if stage.Name != n.Name {
				continue
			}
			switch {
			case stage.Result == hostsetup.Succeeded:
				row.State = Done
			case stage.Result == hostsetup.Blocked || s.State != SetupRunning:
				row.State = Blocked
			default:
				row.State = Running
				if at, err := time.Parse(time.RFC3339, stage.At); err == nil {
					row.Text += " (last news at " + at.In(locOf(f)).Format("15:04") + ")"
				}
			}
		}
		v.Rows = append(v.Rows, row)
	}
	return v
}

// viewDetails is what is open, in the terminal's words, for whoever helps:
// only where something needs help, not on a Mac with steps simply left.
func viewDetails(f Facts, st Status, s Setup) string {
	var out []string
	help := false
	for _, c := range s.Checks {
		if c.optional || c.State != Blocked && c.State != Unknown && (c.State != Todo || c.Action == "") {
			continue
		}
		help = help || c.State != Todo
		out = append(out, c.ID+": "+c.State+": "+c.Detail)
		if c.Action != "" {
			out = append(out, "  "+c.Action)
		}
	}
	note := func(what, why string) {
		help = true
		out = append(out, what+": "+why)
	}
	if f.Daemon == "" && st.Lending.Sizes > 0 && switchUnread(f, st) {
		note("the host switch cannot be read", cmp.Or(f.HostErr, "no reason given"))
	}
	if f.LendErr != "" {
		note("the lending limits cannot be read", f.LendErr)
	}
	if s.State == SetupNone && st.Lending.Sizes == 0 && endedEarly(f) {
		note("the last setup ended before it kept a record", f.Background.Last)
	}
	if !help {
		return ""
	}
	return "Potluck " + f.Setup.Version + " setup\n" + strings.Join(out, "\n")
}
