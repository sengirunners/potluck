package vmhost

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sengirunners/potluck/protocol"
)

// What each image's last GitHub runner refresh (runner_refresh.go) found and
// did, kept in the state directory so describe can say it: the supervisor that
// did the refresh and the driver process that answers describe are different
// processes. A binding has several images and each box runs one, so there is
// one result per image, keyed by the image as the binding names it. The
// policy (the minimum, the pinned release) stays in runner_refresh.go; this is
// only its results. The wire type and its states are protocol.RunnerStatus.

func (s store) runnerStatusFile() string { return filepath.Join(s.dir, "runner-status.json") }

// readRunners is the stored results by image; a missing, corrupt or
// half-written file is none.
func (s store) readRunners() map[string]protocol.RunnerStatus {
	b, err := os.ReadFile(s.runnerStatusFile())
	var m map[string]protocol.RunnerStatus
	if err != nil || json.Unmarshal(b, &m) != nil {
		return nil
	}
	return m
}

// changeRunners rewrites the stored results under the file's lock, so two
// boxes of different images finishing together keep both. Best effort:
// describe leaves a result out that was never written, and a failed write
// must not fail a box. The file is replaced whole at 0600.
func (s store) changeRunners(f func(map[string]protocol.RunnerStatus)) {
	_ = flocked(filepath.Join(s.dir, "runner-status.lock"), func() error {
		m := s.readRunners()
		if m == nil {
			m = map[string]protocol.RunnerStatus{}
		}
		f(m)
		b, err := json.Marshal(m)
		if err != nil {
			return err
		}
		tmp, err := os.CreateTemp(s.dir, "runner-status-*")
		if err != nil {
			return err
		}
		defer os.Remove(tmp.Name())
		_, werr := tmp.Write(b)
		if cerr := tmp.Close(); werr == nil {
			werr = cerr
		}
		if werr != nil {
			return werr
		}
		if err := os.Chmod(tmp.Name(), 0o600); err != nil {
			return err
		}
		return os.Rename(tmp.Name(), s.runnerStatusFile())
	})
}

// recordRunner replaces image's stored result.
func (s store) recordRunner(st protocol.RunnerStatus) {
	s.changeRunners(func(m map[string]protocol.RunnerStatus) { m[st.Image] = st })
}

// clearRunner removes image's stored result: a box of that image has no
// runner to report on.
func (s store) clearRunner(image string) {
	s.changeRunners(func(m map[string]protocol.RunnerStatus) { delete(m, image) })
}

// runnerStatuses are the results describe shows: one per image the binding
// has now, in the binding's order (cut at protocol.MaxRunners, failures kept
// first). An entry that is not a valid result is absent (the producer enforces
// what a reader may rely on), and so is one the current minimum no longer
// vouches for: a runner "not_needed" or "refreshed" at a version below it (a
// driver upgrade raised the minimum) is unknown until a box of that image is
// next checked. A "failed" result is not about a version, so the minimum does
// not hide it: it stays until the next box of that image replaces it.
func (s store) runnerStatuses(images []string) []protocol.RunnerStatus {
	m := s.readRunners()
	var out []protocol.RunnerStatus
	for _, im := range images {
		st, ok := m[im]
		if !ok || st.Image != im || !st.Valid() {
			continue
		}
		if st.Refresh != protocol.RunnerFailed && runnerTooOld(st.Version) {
			continue
		}
		out = append(out, st)
	}
	return protocol.CleanRunners(out, protocol.MaxRunners)
}

// runnerImages are the images the binding's boxes run: a box binding's pinned
// images, else the one image every run clones.
func (c config) runnerImages() []string {
	if len(c.Images) > 0 {
		return c.Images
	}
	if c.Image != "" {
		return []string{c.Image}
	}
	return nil
}

// runnerVersionOf is a version as describe states it: no leading v, and ""
// for the probe's "-" (a runner that gave none).
// A version a result may not carry (too long, or with a control character:
// the guest wrote it) is "" too, so the result still shows.
func runnerVersionOf(v string) string {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if v == "-" || !protocol.PlainRunnerVersion(v) {
		return ""
	}
	return v
}

// refreshError is a refresh failure with the short reason describe gives. Its
// text is the detail acquire's caller and the host's log get.
type refreshError struct {
	why protocol.RunnerWhy
	err error
}

func (e *refreshError) Error() string { return e.err.Error() }
func (e *refreshError) Unwrap() error { return e.err }

// failWhy marks err as a failure of kind why, unless it already says one
// (the innermost reason, which saw the failure, stands).
func failWhy(why protocol.RunnerWhy, err error) error {
	var re *refreshError
	if err == nil || errors.As(err, &re) {
		return err
	}
	return &refreshError{why: why, err: err}
}

// whyOf is err's reason, def when it names none.
func whyOf(err error, def protocol.RunnerWhy) protocol.RunnerWhy {
	var re *refreshError
	if errors.As(err, &re) {
		return re.why
	}
	return def
}

// failedRunner is a failed refresh's result for image, with its error. The
// box ended with no working runner, so Version is empty; ImageVersion is what
// the image shipped ("" when that could not be read).
func failedRunner(image, imageVersion string, why protocol.RunnerWhy, err error) (*protocol.RunnerStatus, error) {
	return &protocol.RunnerStatus{Image: image, ImageVersion: runnerVersionOf(imageVersion), Refresh: protocol.RunnerFailed,
		Why: why, At: time.Now().UTC()}, err
}
