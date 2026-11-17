package vmhost

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/creack/pty"
)

// A person who types --help gets text and exit 0; an unknown
// word gets the usage and exit 2; the daemon's run (a pipe, no arguments) is
// left to read its request.
func TestDriverUsageForAPerson(t *testing.T) {
	pipe, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer pipe.Close()
	w.Close()
	null, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer null.Close()
	if code, done := usageOnly(nil, null, io.Discard, io.Discard); code != 0 || done {
		t.Errorf("stdin /dev/null is not a terminal: code %d done %v", code, done)
	}
	for _, c := range []struct {
		argv []string
		code int
		done bool
		out  string
	}{
		{[]string{"--help"}, 0, true, "potluck host set drain"},
		{[]string{"-h"}, 0, true, "usage: potluck-driver-tart"},
		{[]string{"frobnicate"}, 2, true, `unknown argument "frobnicate"`},
		{nil, 0, false, ""},
	} {
		var out, errs bytes.Buffer
		code, done := usageOnly(c.argv, pipe, &out, &errs)
		if code != c.code || done != c.done || !strings.Contains(out.String()+errs.String(), c.out) {
			t.Errorf("%v: code %d done %v, stdout %q stderr %q; want code %d done %v containing %q",
				c.argv, code, done, out.String(), errs.String(), c.code, c.done, c.out)
		}
	}
}

// On a real terminal with no request, the driver prints usage and exits 2.
func TestDriverUsageOnATerminal(t *testing.T) {
	ptmx, tty, err := pty.Open()
	if err != nil {
		t.Skip("no pty:", err)
	}
	defer ptmx.Close()
	defer tty.Close()
	var out, errs bytes.Buffer
	if code, done := usageOnly(nil, tty, &out, &errs); code != 2 || !done || !strings.Contains(errs.String(), "potluck host") {
		t.Errorf("a terminal: code %d done %v stderr %q", code, done, errs.String())
	}
}

// host set says what it did, in words an owner can act on.
func TestHostOverrideSummary(t *testing.T) {
	d, until := 1800, time.Date(2026, 11, 16, 23, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		o    HostOverride
		want string
	}{
		{HostOverride{Mode: modeDrain}, "host drain: no new work; running work finishes, and is cut at each size's drain deadline (30 minutes after the drain began unless a size sets another)"},
		{HostOverride{Mode: modeDrain, Since: until.Add(-10 * time.Minute), DeadlineS: &d}, "host drain: no new work; running work finishes, and is cut at " + until.Add(20*time.Minute).Local().Format("Mon Jan 2 15:04:05")},
		// A timed drain may still be cut at a size's own deadline, so it says so.
		{HostOverride{Mode: modeDrain, Since: until.Add(-time.Hour), Until: &until}, "host drain: no new work; running work finishes, and may be cut at each size's drain deadline (30 minutes after the drain began unless a size sets another); back to auto at " + until.Local().Format(time.RFC3339)},
		// A drain with its own deadline that returns to auto first is never cut.
		{HostOverride{Mode: modeDrain, Since: until.Add(-10 * time.Minute), Until: &until, DeadlineS: &d}, "host drain: no new work; running work finishes; back to auto at " + until.Local().Format(time.RFC3339)},
		{HostOverride{Mode: modeClosed}, "host closed: no new work; running work ends now"},
		{HostOverride{Mode: modeOpen, Until: &until}, "host open: taking work whatever the schedule says; back to auto at " + until.Local().Format(time.RFC3339)},
		{HostOverride{Mode: modeAuto}, "host auto: following the sizes' own rules (schedule, idle time)"},
	} {
		if got := c.o.Summary(); got != c.want {
			t.Errorf("%+v: %q, want %q", c.o, got, c.want)
		}
	}
}

// `host --help` prints the usage on standard output and exits 0; a bare `host` is a mistake (exit 2).
func TestHostHelpExitsZero(t *testing.T) {
	var out, errw bytes.Buffer
	if code := hostCmd([]string{"--help"}, strings.NewReader(""), &out, &errw); code != 0 || !strings.Contains(out.String(), "host set") {
		t.Errorf("host --help: code %d, out %q", code, out.String())
	}
	out.Reset()
	if code := hostCmd(nil, strings.NewReader(""), &out, &errw); code != 2 {
		t.Errorf("bare host: code %d", code)
	}
}
