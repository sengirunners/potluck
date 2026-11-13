package runners

import (
	"cmp"
	"errors"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/config"
)

// DefaultCommand is the program a broker's hints name when its Service
// does not say otherwise.
const DefaultCommand = "potluck"

// cmd names a command the person can run, in this broker's program: every
// hint, warning and job reason that says what to run is built here, so a
// program that embeds the broker under another name (Service.Command) shows
// its own. words are the command's words after the program: cmd("runners
// allow acme/app") is "potluck runners allow acme/app".
func (s *Service) cmd(words string) string {
	return cmp.Or(s.Command, DefaultCommand) + " " + words
}

// offMessage is what errOff says; offHint is built by off.
const offMessage = "the runner broker is not set up on this machine"

// errOff is the broker not being set up. It is returned through off, which
// adds the hint (it names a command), so test it with isOff.
var errOff = api.Errorf(api.CodeUnavailable, "%s", offMessage)

// off is errOff with the command that sets the broker up.
func (s *Service) off() error {
	return errOff.WithHint(s.cmd("runners setup <pool>") + " on the pool's machine (SPEC 20 \"GitHub runner broker\")")
}

// isOff reports whether err is errOff, with or without its hint.
func isOff(err error) bool {
	var e *api.Error
	return errors.As(err, &e) && e.Code == errOff.Code && e.Message == errOff.Message
}

// notices are what the pool's owner should know about config.json, in plain
// words: shown by runners ls (Setup.Notices) and the doctor, and logged once
// when the broker starts. They are read from config.json on disk, so a key
// the owner has deleted, as a notice says to, stops being reported without a
// restart; when the file cannot be read they come from the config in memory.
func (s *Service) notices() []string {
	c := s.conf()
	if fresh, err := config.LoadFrom(s.d.Config.Home); err == nil {
		c = fresh.File.Runners
	}
	var out []string
	if c.RetiredAllowPublicRepos {
		out = append(out, "allow_public_repos in config.json does nothing any more: a public repository is served only after its owner says yes "+
			"for it, one repository at a time ("+s.cmd("runners allow <owner/repo> --public")+"). Public repositories that were served under the old "+
			"key are not served until then; delete the key from config.json")
	}
	return out
}
