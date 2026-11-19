// Package hostsetup is a host run's stages as it goes: preflight, the
// images, validation, the join and usable capacity, each running, succeeded
// or blocked. potluck host writes them (a Tracker) to the home's
// host-setup.json as each stage changes, and prints each change as one line;
// potluck host status and the macOS menu bar item read that file (Read), so
// the terminal and the menu say the same thing about a setup that is going
// on, finished, or blocked.
package hostsetup

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// FileName is the record's file in the home.
const FileName = "host-setup.json"

// The stages, in the order a host run goes through them.
const (
	// Preflight is everything checked before a download: the layer, the
	// image's fit, the disk, the network helper, the admin step.
	Preflight = "preflight"
	// Images is pulling each image, or reusing one already here.
	Images = "images"
	// Validation is the conformance checks on each size.
	Validation = "validation"
	// Join is joining the pool (an invite), or finding it already joined.
	Join = "join"
	// Capacity is sharing the sizes that passed and seeing the pool list
	// them: what the pool can use.
	Capacity = "capacity"
)

// Order is every stage, in order.
var Order = []string{Preflight, Images, Validation, Join, Capacity}

// Label is a stage as a person reads it.
func Label(stage string) string {
	if stage == Capacity {
		return "usable capacity"
	}
	return stage
}

// A stage's result.
const (
	Running   = "running"
	Succeeded = "succeeded"
	Blocked   = "blocked"
)

// Stage is one stage's latest word.
type Stage struct {
	Name   string `json:"name"`
	Result string `json:"result"`
	Detail string `json:"detail,omitempty"`
	At     string `json:"at"`
}

// Run is a host run's record.
type Run struct {
	Started string `json:"started"`
	// Ended is set once the run returned; a record with no Ended whose run
	// holds no host lock was cut off (Interrupted).
	Ended string `json:"ended,omitempty"`
	// Again is the command that runs this setup again, as a person types
	// it (an invite code stands as <invite code>).
	Again string `json:"again,omitempty"`
	// Stages are the stages reached, in Order.
	Stages []Stage `json:"stages"`
}

// Blocked is the stage the run stopped at, or nil.
func (r *Run) Blocked() *Stage {
	if r == nil {
		return nil
	}
	for i := range r.Stages {
		if r.Stages[i].Result == Blocked {
			return &r.Stages[i]
		}
	}
	return nil
}

// Current is the stage running now (the last one reached), or nil.
func (r *Run) Current() *Stage {
	if r == nil || len(r.Stages) == 0 {
		return nil
	}
	if s := &r.Stages[len(r.Stages)-1]; s.Result == Running {
		return s
	}
	return nil
}

// Succeeded reports whether the run ended with every stage succeeded.
func (r *Run) Succeeded() bool {
	if r == nil || r.Ended == "" || len(r.Stages) != len(Order) {
		return false
	}
	for _, s := range r.Stages {
		if s.Result != Succeeded {
			return false
		}
	}
	return true
}

// Read is the home's record: nil, nil when no host run wrote one.
func Read(home string) (*Run, error) {
	b, err := os.ReadFile(filepath.Join(home, FileName))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var r Run
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, fmt.Errorf("%s: %w", FileName, err)
	}
	return &r, nil
}

// Tracker records one host run's stages. Its methods are safe on a nil
// Tracker, which records nothing (a dry run) but still answers each line.
type Tracker struct {
	mu   sync.Mutex
	path string
	run  Run
	// redact is text that must never reach the record or a line (the
	// invite code).
	redact []string
	now    func() time.Time
	// err is the first failure to write the record (the run goes on: the
	// record serves host status, not the run).
	err error
}

// Err is the first failure to write the record, or nil.
func (t *Tracker) Err() error {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.err
}

// Start begins home's record for a new run, replacing the last one's.
// again is the command that runs it again; redact is text never written
// (the invite code). home "" records nothing.
func Start(home, again string, redact ...string) *Tracker {
	t := &Tracker{now: time.Now}
	for _, r := range redact {
		if r != "" {
			t.redact = append(t.redact, r)
		}
	}
	if home != "" {
		t.path = filepath.Join(home, FileName)
	}
	t.run.Started, t.run.Again = t.stamp(), t.clean(again)
	t.run.Stages = []Stage{}
	t.save()
	return t
}

// Begin marks stage running with detail (a later stage's Begin ends the
// run of an earlier one as succeeded) and answers its line.
func (t *Tracker) Begin(stage, detail string) string {
	return t.set(stage, Running, detail)
}

// Succeed marks stage succeeded and answers its line.
func (t *Tracker) Succeed(stage, detail string) string {
	return t.set(stage, Succeeded, detail)
}

// Block marks stage blocked and answers its line.
func (t *Tracker) Block(stage, detail string) string {
	return t.set(stage, Blocked, detail)
}

// End ends the run. A failure blocks the stage running now (or, when none
// runs, the first stage not reached), unless a stage is blocked already;
// with no failure a stage still running succeeded. It answers the stage
// the run stopped at (nil when none).
func (t *Tracker) End(failure error) *Stage {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	cur, blocked := t.run.Current(), t.run.Blocked() != nil
	t.mu.Unlock()
	switch {
	case failure != nil && !blocked && cur != nil:
		t.Block(cur.Name, failure.Error())
	case failure != nil && !blocked:
		t.Block(t.nextStage(), failure.Error())
	case failure == nil && cur != nil:
		t.Succeed(cur.Name, cur.Detail)
	}
	t.mu.Lock()
	t.run.Ended = t.stamp()
	b := t.run.Blocked()
	var out *Stage
	if b != nil {
		c := *b
		out = &c
	}
	t.mu.Unlock()
	t.save()
	return out
}

// SetAgain changes the command that runs this setup again (after a join,
// the pool's name stands for the invite).
func (t *Tracker) SetAgain(again string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.run.Again = t.clean(again)
	t.mu.Unlock()
	t.save()
}

// Run is a copy of the record as it stands.
func (t *Tracker) Run() Run {
	if t == nil {
		return Run{}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	r := t.run
	r.Stages = append([]Stage(nil), t.run.Stages...)
	return r
}

// nextStage is the first stage not reached yet (the last stage when all
// were).
func (t *Tracker) nextStage() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return Order[min(len(t.run.Stages), len(Order)-1)]
}

func (t *Tracker) set(stage, result, detail string) string {
	if t == nil {
		return Line(stage, result, detail)
	}
	t.mu.Lock()
	detail = t.clean(detail)
	st := Stage{Name: stage, Result: result, Detail: detail, At: t.stamp()}
	i := len(t.run.Stages) - 1
	switch {
	case i >= 0 && t.run.Stages[i].Name == stage:
		t.run.Stages[i] = st
	default:
		if i >= 0 && t.run.Stages[i].Result == Running { // reached the next stage: the last one went through
			t.run.Stages[i].Result = Succeeded
		}
		t.run.Stages = append(t.run.Stages, st)
	}
	t.mu.Unlock()
	t.save()
	return Line(stage, result, detail)
}

// Number is a stage's place in Order, from 1 (0 for none).
func Number(stage string) int {
	for i, s := range Order {
		if s == stage {
			return i + 1
		}
	}
	return 0
}

// Line is a stage change as one line: "[3/5] validation: blocked: ...";
// a running stage has no result word.
func Line(stage, result, detail string) string {
	s := fmt.Sprintf("[%d/%d] %s", Number(stage), len(Order), Label(stage))
	if result != Running {
		s += ": " + result
	}
	if detail != "" {
		s += ": " + detail
	}
	return s
}

func (t *Tracker) clean(s string) string {
	for _, r := range t.redact {
		s = strings.ReplaceAll(s, r, "<invite code>")
	}
	return s
}

func (t *Tracker) stamp() string {
	return t.now().UTC().Format(time.RFC3339)
}

// save writes the record (a temp file renamed over it, so a reader never
// sees half of one).
func (t *Tracker) save() {
	if t == nil || t.path == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	b, err := json.MarshalIndent(t.run, "", "  ")
	if err == nil {
		err = writeFile(t.path, append(b, '\n'))
	}
	if err != nil && t.err == nil {
		t.err = err
	}
}

func writeFile(path string, b []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), "."+FileName+".*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, err = f.Write(b)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(tmp, 0o600)
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		os.Remove(tmp)
	}
	return err
}
