package cli

// The menu bar app's uninstall window: every word of it, for someone who has
// never used a terminal. It is made from the same plan and steps as the
// terminal's words, so the two say the same facts. No command, flag, path or
// file is in it but Details, which the window never shows, only copies for
// whoever helps.

import (
	"errors"
	"fmt"
	"strings"

	"github.com/sengirunners/potluck/kit/api"
)

// uninstallView is the uninstall window.
type uninstallView struct {
	// Heading is how the removal stands and Lead what that means and what
	// the buttons do; Failed is Lead when the password step was tried and
	// failed (an answer with admin_step only).
	Heading string `json:"heading"`
	Lead    string `json:"lead,omitempty"`
	Failed  string `json:"failed,omitempty"`
	// Warning is what is lost for good.
	Warning string `json:"warning,omitempty"`
	// Choice is a box the person may tick (nil: none).
	Choice *viewChoice `json:"choice,omitempty"`
	// Done and Left are the steps done and left; Stays is what stays.
	Done  []viewStep `json:"done"`
	Left  []viewStep `json:"left"`
	Stays []string   `json:"stays"`
	// Details is what a person sends to whoever helps them, in the
	// terminal's words.
	Details string `json:"details,omitempty"`
}

// viewStep is one step: what it will do and, done, what it did. The same ID
// in two runs is the same step.
type viewStep struct {
	ID   string `json:"id"`
	Will string `json:"will"`
	Did  string `json:"did"`
}

// viewChoice is a box to tick: its words, whether the plan shown has it
// ticked, and the arguments that plan with it the other way.
type viewChoice struct {
	Label string   `json:"label"`
	On    bool     `json:"on"`
	Args  []string `json:"args"`
}

func (s uninstallStep) view() viewStep { return viewStep{s.id, s.will, s.did} }

// viewOutcome is how a run of uninstall ended.
type viewOutcome int

const (
	viewPlan       viewOutcome = iota // a dry run
	viewDone                          // every step was done
	viewAdmin                         // it stopped at the password step
	viewBusy                          // jobs run, and Remove anyway can go on
	viewBusyOurs                      // jobs run in a group this Mac runs: nothing goes on
	viewBusyUnread                    // whether jobs run could not be told
	viewFailed                        // a step failed
)

// outcomeOf is how a run ended whose step s failed with err.
func outcomeOf(s uninstallStep, err error, forced bool) viewOutcome {
	var busy *needsForce
	switch {
	case errors.As(err, &busy) && !forced && busy.live:
		return viewBusy
	case errors.As(err, &busy) && !forced:
		return viewBusyUnread
	case s.id == "lending" && api.IsCode(err, api.CodeConflict):
		return viewBusyOurs
	}
	return viewFailed
}

const (
	askHelp = "If it stops again, copy the details and send them to the person who invited you."
	// forceDetail is what Remove anyway does.
	forceDetail = "Running jobs may keep going for a while. The people who started them will lose access."
)

func forceQuestion(live bool) string {
	if live {
		return "Remove Potluck while jobs are running on this Mac?"
	}
	return "Remove Potluck while jobs may still be running?"
}

// view is the window after a run with f that ended as o says.
func (p uninstallPlan) view(f uninstallFlags, done []viewStep, left []uninstallStep, o viewOutcome, ans uninstallAnswer) *uninstallView {
	v := &uninstallView{Done: done, Left: []viewStep{}, Stays: append([]string{}, p.stays...)}
	for _, s := range left {
		v.Left = append(v.Left, s.view())
	}
	switch o {
	case viewPlan:
		v.Heading, v.Lead = "Remove Potluck from this Mac", "Review what will be removed, then choose Remove… to continue."
		if len(left) == 0 {
			v.Heading, v.Lead = "Potluck is already removed from this Mac", "You can move this app to the Trash."
			break
		}
		if p.home != "" && !p.running && len(p.joined) == 0 {
			v.Lead += " Start Potluck first if you want it to notify the groups this Mac has joined."
		}
		v.Warning, v.Choice = p.lost(f), p.choice(f)
	case viewDone:
		v.Heading = "Potluck is removed from this Mac"
	case viewAdmin:
		v.Heading, v.Lead = "Not everything is removed yet", "Choose Remove… to finish. macOS will ask for this Mac's password."
		v.Failed = "Potluck couldn't finish the step that needs your password. Choose Remove… to try again. " + askHelp
	case viewBusy:
		v.Heading = "Jobs are still running on this Mac"
		v.Lead = "Wait for jobs to finish and try again, or choose Remove anyway… to proceed. " + forceDetail
	case viewBusyOurs:
		v.Heading = "Jobs are still running on this Mac"
		v.Lead = "This Mac runs the group these jobs belong to. Wait for them to finish, then choose Remove… again."
	case viewBusyUnread:
		v.Heading = "Potluck can't tell whether jobs are running on this Mac"
		v.Lead = "Choose Remove… to check again, or Remove anyway… to proceed. " + forceDetail
	case viewFailed:
		v.Heading = "Not everything is removed yet"
		v.Lead = fmt.Sprintf("Potluck couldn't finish this step: %s. Choose Remove… to try again. %s", v.Left[0].Will, askHelp)
		if v.Left[0].ID == tartReadStep {
			v.Lead = "Potluck couldn't check what Tart has stored. Choose Remove… to try again, or deselect the Tart option to keep Tart. " + askHelp
		} else if id := v.Left[0].ID; id == tartImagesStep || id == tartAppStep {
			v.Lead += " If the Tart option is shown, deselect it to keep the rest of Tart."
		}
	}
	v.Details = p.details(ans)
	return v
}

// lost is what removing the home loses for good ("" when it stays).
func (p uninstallPlan) lost(f uninstallFlags) string {
	if p.warning == "" {
		return ""
	}
	w := "This Mac's Potluck identity will be permanently deleted."
	switch {
	case len(p.poolsRun) > 0:
		w += " These groups will close for everyone: " + strings.Join(p.poolsRun, ", ") + "."
	case !p.poolsKnown:
		w += " Any group this Mac runs will close for everyone. Potluck couldn't check which groups this Mac runs."
	}
	return w
}

// confirmDetail is what the question before a removal with f says is lost.
func (p uninstallPlan) confirmDetail(f uninstallFlags) string {
	return strings.TrimSpace(p.lost(f) + p.alsoTart(f))
}

// alsoTart is what a removal with f takes of Tart, as a question says it.
func (p uninstallPlan) alsoTart(f uninstallFlags) string {
	if o := p.tart.offers(f.withTart); o != "" && f.withTart != tartNone {
		s := " This also removes " + o + "."
		if f.withTart == tartAll && (p.tart.takesApp() || p.tart.unread(tartAll) != nil) {
			s += " Any Tart virtual machines stored elsewhere will still need Tart to run."
		}
		return s
	}
	return ""
}

// choice is the box for --with-tart, when it would remove anything.
func (p uninstallPlan) choice(f uninstallFlags) *viewChoice {
	o := p.tart.offers(f.withTart)
	if o == "" {
		return nil
	}
	args := []string{"--json", "uninstall", "--dry-run"}
	if f.keepHome {
		args = append(args, "--keep-home")
	}
	if f.withTart == tartNone {
		args = append(args, "--with-tart")
	}
	return &viewChoice{Label: "Also remove " + o, On: f.withTart != tartNone, Args: args}
}

// details is the answer in the terminal's words.
func (p uninstallPlan) details(ans uninstallAnswer) string {
	out := []string{"Potluck " + api.Version + " uninstall"}
	list := func(head string, xs []string) {
		if len(xs) > 0 {
			out = append(out, head+":\n  - "+strings.Join(xs, "\n  - "))
		}
	}
	list("done", ans.Done)
	list("left", ans.Left)
	list("not removed", ans.NotRemoved)
	if ans.Error != "" {
		out = append(out, "error: "+ans.Error)
	}
	if ans.AdminStep != nil {
		out = append(out, "admin step: "+ans.AdminStep.command())
	}
	return strings.Join(out, "\n")
}
