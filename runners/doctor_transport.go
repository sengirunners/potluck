package runners

import (
	"context"
	"time"
)

// doctorTransportFailAfter is how long the broker's rounds may keep failing
// (the checker's failing_since) before check 15 fails: a list that failed
// once backs off for a round or two (a wake before the network is back, one
// 502), and that is no reason to tell the owner.
const doctorTransportFailAfter = 30 * time.Minute

// doctorTransport is check 15: whether the broker hears of GitHub's waiting
// jobs for the installation, read from the checker's own record (Checked,
// the check field of GET /v0/runners/setup). Webhook mode keeps a catch-up
// check, so both modes have a last complete round to read.
//
// It passes on a complete round at most failWindow old (the checker's, so a
// slow round or a backed-off list is not mistaken for a stopped checker). It fails only when
// rounds keep failing: the latest failure within failWindow, and failing
// rounds in a row (the checker's failing_since, which a sleep restarts) for
// doctorTransportFailAfter or three rounds, whichever is longer. Anything
// else short of a pass is a warning: a round cut by the checker's budget or
// GitHub's rate limit (a designed throttle), an old round with nothing since
// (a Mac that just woke), or a failure too recent to say it lasts. An old
// round with nothing since fails when the daemon has stayed awake as long
// (the checker's heartbeat, AwakeSince): a round is stuck, or the checker
// stopped.
func (s *Service) doctorTransport(ctx context.Context, installation int64, out *DoctorReport) {
	record := s.cmd("runners ls --json")
	logFix := "check the daemon log for the runner check, GitHub connectivity and the App key"
	ch, err := s.Checked(ctx)
	if err != nil {
		out.set(15, "warn", "unavailable: the broker's record of its GitHub checks could not be read", record)
		return
	}
	var in *CheckedInstallation
	for i := range ch.Installations {
		if ch.Installations[i].Installation == installation {
			in = &ch.Installations[i]
		}
	}
	if in == nil {
		out.set(15, "warn", "unavailable: the broker has no check record for installation "+itoa(installation), record)
		return
	}
	now := s.now()
	every := s.every()
	window := s.failWindow()
	parse := func(at string) (time.Time, bool) {
		t, err := time.Parse(time.RFC3339, at)
		return t, err == nil
	}
	recent := func(at string) bool {
		t, ok := parse(at)
		return ok && now.Sub(t) <= window && t.Sub(now) <= time.Minute
	}
	lasting := func(since string) bool {
		t, ok := parse(since)
		return ok && now.Sub(t) >= max(doctorTransportFailAfter, 3*every)
	}
	how := "it asks GitHub for waiting jobs every " + every.String()
	if ch.Mode == "webhook" {
		how = "GitHub's webhook deliveries, with a catch-up check every " + every.String()
	}
	lastOK, okKnown := parse(in.LastOK)
	age := now.Sub(lastOK)
	since := "No complete check of GitHub yet"
	if okKnown {
		since = "No complete check of GitHub since " + in.LastOK
	}
	// A cut recorded with the complete round itself (a page cap) is that
	// round's note, not a later round's shortfall.
	cutAt, cutKnown := parse(in.CutAt)
	cutSince := in.Cut != "" && cutKnown && (!okKnown || cutAt.After(lastOK)) && recent(in.CutAt)
	errSince := in.Error != "" && recent(in.ErrorAt)
	// stuck: the latest round on record, whatever it found, ended a lasting
	// while ago, and the daemon has been awake since longer than that.
	latest, stuck := lastOK, false
	for _, at := range []string{in.ErrorAt, in.CutAt} {
		if t, ok := parse(at); ok && t.After(latest) {
			latest = t
		}
	}
	if !latest.IsZero() && lasting(latest.UTC().Format(time.RFC3339)) && lasting(ch.AwakeSince) {
		stuck = true
	}
	wait := "wait " + every.String() + ", then " + s.cmd("ci doctor") + " again; if it stays, " + logFix
	switch {
	case ch.Error != "" && recent(ch.ErrorAt) && lasting(ch.FailingSince):
		out.set(15, "fail", "The broker's checks of GitHub could not begin since "+ch.FailingSince+": "+ch.Error, logFix)
	case errSince && lasting(in.FailingSince):
		out.set(15, "fail", since+"; checks have failed since "+in.FailingSince+": "+in.Error, logFix)
	case ch.Error != "" && recent(ch.ErrorAt):
		out.set(15, "warn", "The broker's last check of GitHub could not begin: "+ch.Error, wait)
	case okKnown && age < -time.Minute:
		out.set(15, "warn", "unavailable: the last complete check of GitHub ("+in.LastOK+") is later than this machine's clock", "check this machine's clock")
	case errSince:
		out.set(15, "warn", since+"; a check since could not read: "+in.Error, wait)
	case okKnown && age <= window:
		msg := "The broker last completed a check of GitHub at " + in.LastOK + " (" + how + ")."
		if cutSince {
			out.set(15, "warn", msg+" A check since read less than everything: "+in.Cut, wait)
			return
		}
		if in.Cut != "" && in.CutAt == in.LastOK {
			msg += " That check read up to its limit: " + in.Cut + "."
		}
		out.set(15, "pass", msg, "")
	case stuck:
		out.set(15, "fail", "No round of the broker's checks of GitHub has ended since "+latest.UTC().Format(time.RFC3339)+
			", though this daemon has been awake since "+ch.AwakeSince+": a round is stuck, or the checker stopped", "restart the "+s.cmd("daemon")+"; if it stays, "+logFix)
	case cutSince:
		out.set(15, "warn", since+"; a check since read less than everything: "+in.Cut, wait)
	case !okKnown:
		out.set(15, "warn", "The broker has not completed a check of GitHub for this installation yet ("+how+").", wait)
	default:
		out.set(15, "warn", since+" and none has run since (asleep, or the checker is not running).", "wait "+every.String()+", then "+s.cmd("ci doctor")+" again; if it stays, restart the "+s.cmd("daemon"))
	}
}
