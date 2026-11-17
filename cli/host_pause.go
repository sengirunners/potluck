package cli

// A pause is a state of the host switch, not of the program that lends:
// host pause closes the switch (refused while work runs, which it would
// end), host resume hands it back to auto. host status, host set, the menu
// bar item and every driver read the same one file (vmhost.SetHost,
// vmhost.HostNow), so a paused machine shows as paused everywhere, keeps
// what it shares, and lends again at once on resume.

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/sengirunners/potluck/menubar"
	"github.com/sengirunners/potluck/protocol"
	"github.com/sengirunners/potluck/vmhost"
)

// hostSet writes the host switch file; tests stand in for it, as for
// hostNow.
var hostSet = vmhost.SetHost

// pauseAnswer is host pause's and host resume's --json answer. A refused
// pause (work runs) is state refused with its reason and exit 0: nothing
// was changed.
type pauseAnswer struct {
	State   string        `json:"state"` // paused, refused, resumed or not_paused
	Sizes   []string      `json:"sizes"` // this machine's shared sizes ([] when none)
	Until   string        `json:"until,omitempty"`
	Refused *pauseRefusal `json:"refused,omitempty"`
	Next    string        `json:"next,omitempty"` // the command that undoes, retries or follows it
}

// pauseRefusal is a pause refused before anything changed.
type pauseRefusal struct {
	Reason  string `json:"reason"`         // job_running, or job_unknown (a size's work cannot be read)
	Size    string `json:"size,omitempty"` // the shared size the work runs on
	Message string `json:"message"`
}

func (r *pauseRefusal) Error() string { return r.Message }

// paused reports whether the switch keeps new work off: closed or drain
// (an override that cannot be read holds too, but is no one's pause).
func paused(mode string) bool { return mode == protocol.HostClosed || mode == protocol.HostDrain }

func (a *app) hostPauseCmd() *cobra.Command {
	var forD, until string
	cmd := &cobra.Command{
		Use:   "pause [--for 2h | --until 23:00]",
		Short: "Stop lending this machine for now, keeping what it shares (" + a.cmd("host resume") + " lends it again)",
		Long: `Pause closes the host switch: no new work starts, and everything this
machine shares stays shared, so ` + a.cmd("host resume") + ` lends it again at
once. It is refused while work runs on a shared size, which it would end:
wait for it, or ` + a.cmd("host set closed") + ` ends it now. --for and --until
resume by themselves then. ` + a.cmd("host status") + ` and the menu bar item show
a paused machine as paused.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ans, err := a.hostPause(cmd.Context(), forD, until)
			if err != nil {
				return err
			}
			if a.asJSON() {
				return a.printJSON(ans)
			}
			if ans.Refused != nil {
				return ans.Refused
			}
			a.say("paused: no new work starts on this machine%s; %s lends it again", untilWords(ans.Until), ans.Next)
			return nil
		},
	}
	cmd.Flags().StringVar(&forD, "for", "", "resume after this long (30m, 2h)")
	cmd.Flags().StringVar(&until, "until", "", "resume at this local HH:MM or RFC 3339 time")
	return cmd
}

// hostPause closes the switch unless work runs on one of this machine's
// shared sizes. Work placed between the check and the write is ended as
// host set closed ends it; the check spares what already runs.
func (a *app) hostPause(ctx context.Context, forD, until string) (pauseAnswer, error) {
	f := a.hostFacts(ctx)
	switch {
	case f.Daemon != "":
		return pauseAnswer{}, fmt.Errorf("whether work runs here cannot be read (%s): start the daemon (%s), or %s ends any now", f.Daemon, a.cmd("daemon --detach"), a.cmd("host set closed"))
	case f.ListErr != "":
		return pauseAnswer{}, fmt.Errorf("whether work runs here cannot be read (%s): try again, or %s ends any now", f.ListErr, a.cmd("host set closed"))
	}
	ans := pauseAnswer{Sizes: sharedSizes(f)}
	for _, t := range f.Targets {
		if t.Kind == "box" && t.RunTarget.Host == "" && t.Error != "" { // fail closed: work may run there
			ans.State, ans.Sizes, ans.Next = "refused", []string{}, a.cmd("host pause")
			ans.Refused = &pauseRefusal{Reason: "job_unknown", Size: t.Name,
				Message: fmt.Sprintf("whether work runs on %s cannot be read (%s): try again, or %s ends any now", t.Name, t.Error, a.cmd("host set closed"))}
			return ans, nil
		}
		if t.Kind == "box" && t.RunTarget.Host == "" && len(t.Live) > len(t.Stopped) {
			ans.State, ans.Sizes, ans.Next = "refused", []string{}, a.cmd("host pause")
			ans.Refused = &pauseRefusal{Reason: "job_running", Size: t.Name,
				Message: fmt.Sprintf("work is running on %s: wait for it to finish, then run this again (%s ends it now)", t.Name, a.cmd("host set closed"))}
			return ans, nil
		}
	}
	o, err := hostSet(protocol.HostClosed, forD, until, "")
	if err != nil {
		return pauseAnswer{}, err
	}
	ans.State, ans.Next = "paused", a.cmd("host resume")
	if o.Until != nil {
		ans.Until = o.Until.UTC().Format(time.RFC3339)
	}
	return ans, nil
}

func (a *app) hostResumeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "resume",
		Short: "Lend this machine again after " + a.cmd("host pause") + " (the host switch back to auto)",
		Long: `Resume hands the host switch back to auto when it is closed or draining
(` + a.cmd("host pause") + `, or the menu bar item's Stop now and Wind down):
this machine takes work again by its shared sizes' own rules. A switch that
is open or auto is left as it is.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ans, err := a.hostResume(cmd.Context())
			if err != nil {
				return err
			}
			if a.asJSON() {
				return a.printJSON(ans)
			}
			switch {
			case ans.State == "not_paused":
				a.say("not paused: this machine takes work by its own rules")
			case len(ans.Sizes) == 0:
				a.say("resumed, but this machine shares nothing: %s shares it", ans.Next)
			default:
				a.say("resumed: this machine takes work again (%s)", strings.Join(ans.Sizes, ", "))
			}
			return nil
		},
	}
}

// hostResume hands a closed, draining or unreadable switch back to auto.
// It needs no daemon: the sizes are named when one answers.
func (a *app) hostResume(ctx context.Context) (pauseAnswer, error) {
	f := a.hostFacts(ctx)
	ans := pauseAnswer{State: "not_paused", Sizes: sharedSizes(f)}
	if mode := f.Host.Mode; paused(mode) || mode == protocol.HostUnreadable {
		if _, err := hostSet(protocol.HostAuto, "", "", ""); err != nil {
			return pauseAnswer{}, err
		}
		ans.State = "resumed"
	}
	if len(ans.Sizes) == 0 && f.Daemon == "" {
		ans.Next = a.cmd("host")
	}
	return ans, nil
}

// sharedSizes names this machine's shared sizes in f ([] for none).
func sharedSizes(f menubar.Facts) []string {
	out := []string{}
	for _, t := range f.Targets {
		if t.Kind == "box" && t.RunTarget.Host == "" {
			out = append(out, t.Name)
		}
	}
	return out
}

// untilWords is ", until <time>" for an RFC 3339 time, "" for none.
func untilWords(until string) string {
	if until == "" {
		return ""
	}
	return " until " + until
}

// unpauseAfterStop hands a paused switch back to auto once host stop
// removed what this machine shared, leaving nothing, so the next host run
// lends at once; it says
// so on w and answers the mode it replaced ("" when it changed nothing).
func unpauseAfterStop(w io.Writer) (string, error) {
	h, err := hostNow(os.Getuid(), -1)
	if err != nil || !paused(h.Mode) {
		return "", nil // an unreadable switch is its owner's to fix (host set)
	}
	if _, err := hostSet(protocol.HostAuto, "", "", ""); err != nil {
		return "", fmt.Errorf("the host switch is still %s: %w", h.Mode, err)
	}
	fmt.Fprintf(w, "the host switch was %s; it is back to auto, so the next share lends at once\n", h.Mode)
	return h.Mode, nil
}

// pausedNote says, after a share, that a paused switch keeps the shared
// sizes idle ("" when it does not).
func (a *app) pausedNote() string {
	h, err := hostNow(os.Getuid(), -1)
	if err != nil || !paused(h.Mode) {
		return ""
	}
	return fmt.Sprintf("note: the host switch is %s (paused): nothing new starts until %s", h.Mode, a.cmd("host resume"))
}
