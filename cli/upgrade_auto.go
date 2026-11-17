package cli

// potluck upgrade enable | disable | status: the daily update as this user's
// own scheduled job (a LaunchAgent on macOS, a systemd --user timer on
// Linux; never root). The job runs `potluck upgrade --dir <dir>`, which
// installs only a release signed by a key this potluck trusts, never an
// older one, and puts the old programs and daemon back when the new daemon
// does not come up. The writer is kit/updatejob.

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/updatejob"
)

// UpdateLog is the file the daily job writes to, under the home.
const UpdateLog = "update.log"

// updateNames are this program's job names. Another updater is any unit or
// agent that runs `<potluck> upgrade`.
var updateNames = updatejob.Names{
	Label:    "sh.potluck.update",
	Service:  "potluck-update.service",
	Timer:    "potluck-update.timer",
	Mark:     "Written by potluck (potluck upgrade enable)",
	Remove:   "potluck upgrade disable",
	Installs: "the newest signed potluck release",
	Program:  "potluck",
	Foreign:  []string{"upgrade"},
}

// newUpdateSchedule is this machine's (tests replace it: no test writes or
// loads a real job).
var newUpdateSchedule = func() (*updatejob.Schedule, error) { return updatejob.New(updateNames) }

// executable is os.Executable (tests replace it).
var executable = os.Executable

// jobEnvKeep are the non-secret variables from the shell that ran enable
// which the daemon the job restarts needs: where Docker and Tart are, and
// the names this machine and its owner go by.
var jobEnvKeep = []string{"DOCKER_HOST", "TART_HOME", EnvMachine, EnvHuman}

// updateSpec is the job for cfg's home with this potluck: it runs from the
// directory this potluck is installed in, which this user must be able to
// write.
func updateSpec(cfg *config.Config) (updatejob.Spec, error) {
	p, err := executable()
	if err != nil {
		return updatejob.Spec{}, fmt.Errorf("cannot tell where this potluck is installed (%v)", err)
	}
	if r, err := filepath.EvalSymlinks(p); err == nil {
		p = r
	}
	if p, err = filepath.Abs(p); err != nil {
		return updatejob.Spec{}, err
	}
	dir := filepath.Dir(p)
	if err := writableDir(dir); err != nil {
		return updatejob.Spec{}, err
	}
	env := []updatejob.EnvVar{
		{Key: "PATH", Value: updatejob.Path(dir, os.Getenv("PATH"))},
		{Key: EnvHome, Value: cfg.Home},
	}
	// The restarted daemon inherits the job's environment, so what steers it
	// travels along. Secrets (the tailnet key, box credentials) never go
	// into a file on disk.
	for _, k := range jobEnvKeep {
		if v := os.Getenv(k); v != "" {
			env = append(env, updatejob.EnvVar{Key: k, Value: v})
		}
	}
	return updatejob.Spec{
		Argv:   []string{p, "upgrade", "--dir", dir},
		Env:    env,
		Log:    cfg.Path(UpdateLog),
		Minute: updatejob.Minute(cfg.Machine),
	}, nil
}

// writableDir refuses, in one sentence, a directory this user cannot write:
// an upgrade there could only fail at the swap.
func writableDir(dir string) error {
	f, err := os.CreateTemp(dir, ".potluck-update-check-*")
	if err != nil {
		why := "this user cannot write " + dir
		if !errors.Is(err, fs.ErrPermission) {
			why = fmt.Sprintf("potluck cannot write in %s (%v)", dir, err)
		}
		return fmt.Errorf("potluck cannot update itself where it is installed: %s; reinstall potluck in a directory you own (such as ~/.local/bin)", why)
	}
	name := f.Name()
	f.Close()
	return os.Remove(name)
}

func (a *app) upgradeEnableCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "enable",
		Short: "Install this machine's daily update: the newest signed potluck release (a LaunchAgent or a systemd user timer; no root)",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			cfg, err := config.LoadClient()
			if err != nil {
				return err
			}
			s, err := newUpdateSchedule()
			if err != nil {
				return err
			}
			if err := s.Supported(); err != nil {
				return err
			}
			if f := s.Foreign(); f != "" {
				a.say("updates: this machine already has its own potluck updater (%s); potluck left it as it is", f)
				return nil
			}
			sp, err := updateSpec(cfg)
			if err != nil {
				return err
			}
			if err := cfg.EnsureHome(); err != nil { // the log lives there
				return err
			}
			r, err := s.Install(sp)
			if err != nil {
				return err
			}
			if r.Foreign != "" { // appeared meanwhile
				a.say("updates: this machine already has its own potluck updater (%s); potluck left it as it is", r.Foreign)
				return nil
			}
			note := ""
			if n := s.LingerNote(os.Getenv("USER")); n != "" {
				note = "; " + n
			}
			a.say("updates: every day at about 03:%02d potluck installs its newest signed release here (turn off: potluck upgrade disable)%s", sp.Minute, note)
			return nil
		},
	}
}

func (a *app) upgradeDisableCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "disable",
		Short: "Remove the daily update that potluck upgrade enable installed",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			s, err := newUpdateSchedule()
			if err != nil {
				return err
			}
			removed, foreign, err := s.Remove()
			switch {
			case removed:
				a.say("daily update removed: potluck updates here only when you run potluck upgrade")
			case foreign == "" && err == nil:
				a.say("potluck had no daily update installed here")
			}
			if foreign != "" {
				a.say("this machine's potluck updater %s is not one potluck installed; potluck left it as it is", foreign)
			}
			return err
		},
	}
}

// updateStatus is `potluck upgrade status`: whether the daily update is on,
// where its job is, and how its last run ended.
type updateStatus struct {
	Enabled  bool     `json:"enabled"`
	Loaded   bool     `json:"loaded"`
	Files    []string `json:"files,omitempty"`
	Foreign  string   `json:"foreign,omitempty"` // another updater's file
	Log      string   `json:"log"`
	LastRun  string   `json:"last_run,omitempty"` // when the log was last written
	LastLine string   `json:"last_line,omitempty"`
}

func (a *app) upgradeStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Say whether the daily update is on, and how its last run ended",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			cfg, err := config.LoadClient()
			if err != nil {
				return err
			}
			s, err := newUpdateSchedule()
			if err != nil {
				return err
			}
			if err := s.Supported(); err != nil {
				return err
			}
			st := updateStatus{Enabled: s.Installed(), Foreign: s.Foreign(), Log: cfg.Path(UpdateLog)}
			if st.Enabled {
				st.Files = s.Files()
				st.Loaded = s.Loaded()
			}
			if fi, err := os.Stat(st.Log); err == nil {
				st.LastRun = fi.ModTime().Format(time.RFC3339)
				st.LastLine = lastLine(st.Log)
			}
			if a.asJSON() {
				return a.printJSON(st)
			}
			switch {
			case st.Enabled && st.Loaded:
				a.say("daily update: on (%s)", tildePath(st.Files[0]))
			case st.Enabled:
				a.say("daily update: installed but not loaded (%s); potluck upgrade enable loads it again", tildePath(st.Files[0]))
			default:
				a.say("daily update: off (turn on: potluck upgrade enable)")
			}
			if st.Foreign != "" {
				a.say("another potluck updater on this machine: %s (potluck leaves it as it is)", tildePath(st.Foreign))
			}
			if st.LastRun != "" {
				a.say("last run: %s: %s", strings.Replace(st.LastRun, "T", " ", 1), st.LastLine)
			}
			return nil
		},
	}
}
