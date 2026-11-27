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
	"slices"
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
	// Flag, on a blocked stage, is the flag it refused because the layer
	// this machine shares on does not take it (an OptionError), as "--docker".
	Flag string `json:"flag,omitempty"`
	// Fix is how the run's Again was changed for that refusal: "without
	// --docker=true" (the flag left out) or "with --docker=false" (turned
	// off where an earlier share kept it); "" when it could not be.
	Fix string `json:"fix,omitempty"`
	// OpenURL, on a running stage, is a page the person must open for it to
	// go on: the tailnet login a join waits for. Any change to the stage
	// clears it.
	OpenURL string `json:"open_url,omitempty"`
}

// OptionError is a host run's refusal of a flag that the layer this
// machine shares on does not take (a flag of another layer): running the
// same command again fails the same way. The code that refuses the flag
// returns it, so the record carries which flag, not words to match.
type OptionError struct {
	// Flag is the flag's name, without dashes ("docker").
	Flag string
	// Off is the argument that turns it off when an earlier share kept it
	// rather than this command ("--docker=false"); "" when none does.
	Off string
	Err error
}

func (e *OptionError) Error() string { return e.Err.Error() }
func (e *OptionError) Unwrap() error { return e.Err }

// Run is a host run's record.
type Run struct {
	Started string `json:"started"`
	// Ended is set once the run returned; a record with no Ended whose run
	// holds no host lock was cut off (Interrupted).
	Ended string `json:"ended,omitempty"`
	// Again is the command that runs this setup again, as a person types
	// it (an invite code stands as '<invite code>').
	Again string `json:"again,omitempty"`
	// AgainArgs is the same command as the program's arguments (the words
	// after the program), for a program to run: no shell, no quoting. The
	// invite code is never in it: NeedsInvite says to give it.
	AgainArgs []string `json:"again_args,omitempty"`
	// NeedsInvite is that AgainArgs needs the invite code, on stdin with
	// --invite-stdin: the run was given one and has not joined its pool yet
	// (once it has, the pool's name stands in its place).
	NeedsInvite bool `json:"needs_invite,omitempty"`
	// Else is a pool AgainArgs, which need the invite, may go on with instead
	// of one: the run's invite was refused, and this machine had joined this
	// pool at the machine it names.
	Else string `json:"else_pool,omitempty"`
	// ElseID is Else's id, whatever it is called by now.
	ElseID string `json:"else_pool_id,omitempty"`
	// InviteRefused is that the machine the invite names refused it: given
	// again it is refused again.
	InviteRefused bool `json:"invite_refused,omitempty"`
	// Lost is that AgainArgs named a pool before WithoutPool.
	Lost bool `json:"-"`
	// PoolID is the id of the pool AgainArgs name, whatever it is called by
	// now ("": not known yet, or none).
	PoolID string `json:"pool_id,omitempty"`
	// Joining is the join this run is spending its invite on (nil: none, or
	// it ended and the record says how). A pool this machine then joined
	// there is that invite's, if the run was cut off before it noted so.
	Joining *Joining `json:"joining,omitempty"`
	// Stages are the stages reached, in Order.
	Stages []Stage `json:"stages"`
}

// Joining is a join under way: the machine the invite names, by its key's
// fingerprint and (not for every invite) its name, and when it began.
type Joining struct {
	Fingerprint string `json:"fingerprint"`
	Machine     string `json:"machine,omitempty"`
	At          string `json:"at"`
}

// Names reports whether the machine with that fingerprint and name is the
// one joined.
func (j Joining) Names(fingerprint, machine string) bool {
	return strings.EqualFold(j.Fingerprint, fingerprint) && (j.Machine == "" || j.Machine == machine)
}

// Pool is the pool AgainArgs name ("": the invite's, or none).
func (r *Run) Pool() string {
	if i := r.poolArg(); i >= 0 {
		return r.AgainArgs[i]
	}
	return ""
}

// poolArg is where Pool is in AgainArgs: the host command's argument, which
// comes before its flags (-1: not there).
func (r *Run) poolArg() int {
	if r == nil || r.NeedsInvite {
		return -1
	}
	i := slices.Index(r.AgainArgs, "host") + 1
	if i == 0 || i == len(r.AgainArgs) || strings.HasPrefix(r.AgainArgs[i], "-") {
		return -1
	}
	return i
}

// WithPool is r, which needs an invite, as it stands once the invite's pool
// is joined: run again it names pool.
func (r Run) WithPool(pool string) *Run {
	if !r.NeedsInvite {
		return &r
	}
	if i := slices.Index(r.AgainArgs, "host") + 1; i > 0 {
		r.AgainArgs = slices.Insert(slices.Clone(r.AgainArgs), i, pool)
	}
	r.Again, r.NeedsInvite, r.Else, r.ElseID = strings.Replace(r.Again, InviteWord, pool, 1), false, "", ""
	return &r
}

// Renamed is r, whose pool is called pool now.
func (r Run) Renamed(pool string) *Run {
	i := r.poolArg()
	if i < 0 {
		return &r
	}
	old := r.AgainArgs[i]
	r.AgainArgs = slices.Clone(r.AgainArgs)
	r.AgainArgs[i] = pool
	r.Again = strings.Replace(r.Again, "host "+old, "host "+pool, 1)
	return &r
}

// WithoutPool is r, whose pool this machine is no longer in: run again it
// needs an invite.
func (r Run) WithoutPool() *Run {
	i := r.poolArg()
	if i < 0 {
		return &r
	}
	pool := r.AgainArgs[i]
	r.Lost = true
	r.AgainArgs = slices.Delete(slices.Clone(r.AgainArgs), i, i+1)
	r.Again, r.NeedsInvite = strings.Replace(r.Again, "host "+pool, "host "+InviteWord, 1), true
	return &r
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

// InviteWord stands for the invite code in Again.
const InviteWord = "'<invite code>'"

// Command is the command a host run was given, from which the record's
// Again, AgainArgs and NeedsInvite are made.
type Command struct {
	// Prefix is the command as a person types it, up to its argument
	// ("potluck host").
	Prefix string
	// Path is the same command as the program's arguments (["host"]).
	Path []string
	// Pool is the pool it shares with; "" when it was given an invite code.
	Pool string
	// Flags are the flags given, in order.
	Flags []Flag
}

// Flag is one flag of a Command.
type Flag struct {
	Name string
	// Text is the flag as it stands in Again, leading space included
	// (" --linux-slots 8").
	Text string
	// Args are its arguments for AgainArgs (["--linux-slots=8"]).
	Args []string
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
	// again is the command that runs the setup again; the record's Again,
	// AgainArgs and NeedsInvite are made from it (setAgain).
	again Command
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
func Start(home string, again Command, redact ...string) *Tracker {
	t := &Tracker{now: time.Now}
	for _, r := range redact {
		if r != "" {
			t.redact = append(t.redact, r)
		}
	}
	if home != "" {
		t.path = filepath.Join(home, FileName)
	}
	t.run.Started = t.stamp()
	t.again = again
	t.setAgain()
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
// with no failure a stage still running succeeded. A failure that is an
// OptionError names its flag on the blocked stage, and Again changes so
// that it no longer gives that flag: without its arguments, or, when an
// earlier share kept it, with the argument that turns it off. It answers
// the stage the run stopped at (nil when none).
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
	var oe *OptionError
	if b := t.run.Blocked(); b != nil && !blocked && errors.As(failure, &oe) { // the stage this failure blocked, not an earlier one
		b.Flag = "--" + oe.Flag
		flags := t.again.Flags
		i := slices.IndexFunc(flags, func(f Flag) bool { return f.Name == oe.Flag })
		switch {
		case i >= 0:
			b.Fix = "without " + strings.TrimSpace(flags[i].Text)
			t.again.Flags = slices.Delete(slices.Clone(flags), i, i+1)
		case oe.Off != "":
			b.Fix = "with " + oe.Off
			t.again.Flags = append(slices.Clone(flags), Flag{Name: oe.Flag, Text: " " + oe.Off, Args: []string{oe.Off}})
		}
		t.setAgain()
	}
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

// Joining says the run is about to spend its invite at j's machine (j.At is
// set here), so that a run cut off there is told from one that never tried.
func (t *Tracker) Joining(j Joining) {
	if t == nil {
		return
	}
	t.mu.Lock()
	j.At = t.stamp()
	t.run.Joining = &j
	t.mu.Unlock()
	t.save()
}

// NotJoined says that machine refused the join; refused is that it refused
// the invite itself. had and hadID are the one pool this machine joined there
// before ("": none, or not known which).
func (t *Tracker) NotJoined(refused bool, had, hadID string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.run.Joining, t.run.InviteRefused, t.run.Else, t.run.ElseID = nil, refused, had, hadID
	t.mu.Unlock()
	t.save()
}

// Joined says the run joined pool: from now on the command that runs this
// setup again names the pool, not the invite.
func (t *Tracker) Joined(pool, id string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.again.Pool = pool
	t.run.Joining, t.run.InviteRefused, t.run.Else, t.run.ElseID, t.run.PoolID = nil, false, "", "", id
	t.setAgain()
	t.mu.Unlock()
	t.save()
}

// Shares says the pool the command names has this id.
func (t *Tracker) Shares(id string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.run.PoolID = id
	t.mu.Unlock()
	t.save()
}

// Open sets the page the person must open for stage, running now, to go on
// (the tailnet login a join waits for); "" clears it. A stage that is not
// the one running is left alone.
func (t *Tracker) Open(stage, url string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	cur := t.run.Current()
	if cur == nil || cur.Name != stage || cur.OpenURL == t.clean(url) {
		t.mu.Unlock()
		return
	}
	cur.OpenURL = t.clean(url)
	t.mu.Unlock()
	t.save()
}

// setAgain makes the record's Again, AgainArgs and NeedsInvite from
// t.again. t.mu is held (or t is not shared yet).
func (t *Tracker) setAgain() {
	c := t.again
	word, args := InviteWord, slices.Clone(c.Path)
	if c.Pool != "" {
		word, args = c.Pool, append(args, c.Pool)
	}
	again := c.Prefix + " " + word
	for _, f := range c.Flags {
		again += f.Text
		args = append(args, f.Args...)
	}
	for i := range args {
		args[i] = t.clean(args[i])
	}
	t.run.Again, t.run.AgainArgs, t.run.NeedsInvite = t.clean(again), args, c.Pool == ""
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
	r.AgainArgs = slices.Clone(t.run.AgainArgs)
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
			t.run.Stages[i].Result, t.run.Stages[i].OpenURL = Succeeded, ""
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
// sees half of one, nor a Mac that lost power an empty one).
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
	if _, err = f.Write(b); err == nil {
		err = f.Sync()
	}
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
