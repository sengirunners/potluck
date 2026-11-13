package runners

import (
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/release"
)

// doctorBuild is check 17. A settings problem (doctorSettings) is its
// answer; otherwise it compares the build this daemon runs
// (api.Version, api.Commit) against the release this home installed last
// through the release channel (kit/release's State.Runs, the comparison the
// daemon's machine numbers make). It never says a build is the newest: only the channel's
// manifest knows that, and the doctor does not fetch it.
func (s *Service) doctorBuild(out *DoctorReport) {
	if s.doctorSettings(out) {
		return
	}
	build := api.Version + " (commit " + api.Commit + ")"
	st, installed, err := release.LoadState(s.d.Config.Home)
	if err != nil {
		out.set(17, "warn", "unavailable: the release this machine installed could not be read: "+oneLine(err.Error()), "")
		return
	}
	if installed && !release.NamesCommit(st.Commit) {
		out.set(17, "warn", "unavailable: the record of the release this machine installed ("+st.Version+") names no commit", "")
		return
	}
	installedRelease := "release " + st.Version + " (commit " + st.Commit + "), which this machine installed last"
	switch st.Runs(installed, api.Version, api.Commit) {
	case release.RunsDev:
		out.set(17, "warn", "unavailable: this daemon runs build "+build+", a development build that names no commit, so no release is compared with it", "")
	case release.RunsSource:
		if !installed {
			out.set(17, "warn", "unavailable: this daemon runs build "+build+", made from source, and this machine installed no release through the release channel to compare it with", "")
			return
		}
		out.set(17, "warn", "This daemon runs build "+build+", made from source, not "+installedRelease,
			"a restart runs the same build: to run the release, start the "+s.cmd("daemon")+" from the installed release's program instead of the one built from source")
	case release.RunsUninstalled:
		out.set(17, "warn", "unavailable: this daemon runs release build "+build+", which was not installed through the release channel, so there is no installed release to compare it with", "")
	case release.RunsInstalled:
		out.set(17, "pass", "This daemon runs release "+st.Version+" (commit "+api.Commit+"), the one this machine installed last.", "")
	default:
		out.set(17, "warn", "This daemon runs release build "+build+", not "+installedRelease, "restart the "+s.cmd("daemon")+" to run the installed release")
	}
}
