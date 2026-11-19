// Package hometerm is a home's term: whether its daemon may serve, and at
// which epoch. A team project has one authority (SPEC 17.3). When a home
// moves to another machine, or a standby takes over, the epoch goes up and
// the old home is fenced, so at most one home serves at any epoch and a
// member can tell the newer home from the older one.
//
// The term is one file in the home (File), never in a backup set: a set
// restored on another machine must not carry the old machine's term. A home
// with no file serves at epoch 0, so a home that never moved needs nothing.
// A file that is there but cannot be read as a term is an error, never
// taken for "serving".
package hometerm

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/sengirunners/potluck/kit/atomicfile"
)

// File is the term's name in the home.
const File = "term.json"

// State says what a home's daemon may do.
type State string

const (
	// Serving: the daemon serves its team projects. The only state in
	// which it accepts writes.
	Serving State = "serving"
	// Frozen: a move has stopped this home before its last backup set; it
	// serves again at the same epoch (Unfreeze) if the move is called off.
	Frozen State = "frozen"
	// Fenced: Successor serves at Epoch or later; this home never serves
	// again at an epoch below that.
	Fenced State = "fenced"
	// Standby: this home holds a copy of Predecessor's data and serves only
	// once promoted to a higher epoch.
	Standby State = "standby"
)

func (s State) valid() bool {
	switch s {
	case Serving, Frozen, Fenced, Standby:
		return true
	}
	return false
}

// Peer names another home by the key a member pins (SPEC 17.11): the
// transport, the address, and the key's fingerprint.
type Peer struct {
	Via         string `json:"via"` // tailnet | tunnel
	URL         string `json:"url"`
	Fingerprint string `json:"fingerprint"`
}

func (p *Peer) check(field string) error {
	if p == nil {
		return fmt.Errorf("%s: missing", field)
	}
	if p.Via != "tailnet" && p.Via != "tunnel" {
		return fmt.Errorf("%s: via %q is neither tailnet nor tunnel", field, p.Via)
	}
	if p.URL == "" || p.Fingerprint == "" {
		return fmt.Errorf("%s: url and fingerprint are both required", field)
	}
	return nil
}

// Term is a home's term. The zero Term is not valid; Load gives a home
// with no file Initial.
type Term struct {
	Epoch uint64 `json:"epoch"`
	State State  `json:"state"`
	// Successor is the home that serves after this one: required when
	// Fenced, optional otherwise (a serving home names the standby it would
	// move to).
	Successor *Peer `json:"successor,omitempty"`
	// Predecessor is the home this one took over from or follows: required
	// when Standby.
	Predecessor *Peer `json:"predecessor,omitempty"`
}

// Initial is the term of a home with no term file.
func Initial() Term { return Term{State: Serving} }

// Serves reports whether the daemon may serve this home.
func (t Term) Serves() bool { return t.State == Serving }

// Check reports a term that is not whole.
func (t Term) Check() error {
	if !t.State.valid() {
		return fmt.Errorf("state %q is not a term state", t.State)
	}
	if t.Successor != nil {
		if err := t.Successor.check("successor"); err != nil {
			return err
		}
	}
	if t.Predecessor != nil {
		if err := t.Predecessor.check("predecessor"); err != nil {
			return err
		}
	}
	switch {
	case t.State == Fenced && t.Successor == nil:
		return errors.New("a fenced term names no successor")
	case t.State == Fenced && t.Epoch == 0:
		return errors.New("a fenced term is at epoch 0, which only a home that never moved has")
	case t.State == Standby && t.Predecessor == nil:
		return errors.New("a standby term names no predecessor")
	case t.State == Standby && t.Successor != nil:
		return errors.New("a standby term names a successor; only a home that has served names one")
	}
	return nil
}

// Load reads home's term: Initial when there is no file. A file that
// cannot be read, does not parse, or is not whole is an error.
func Load(home string) (Term, error) {
	path := filepath.Join(home, File)
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Initial(), nil
	}
	if err != nil {
		return Term{}, err
	}
	var t Term
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&t); err != nil {
		return Term{}, fmt.Errorf("%s: %v", path, err)
	}
	if err := dec.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		return Term{}, fmt.Errorf("%s: more than one term, or something after it", path)
	}
	if err := t.Check(); err != nil {
		return Term{}, fmt.Errorf("%s: %v", path, err)
	}
	return t, nil
}

// Apply changes home's term through move (one of the transitions below,
// applied to the term on disk) and writes the result durably: a crash
// leaves the old term or the new one. The caller holds the home's lock
// (config.LockHome), so no other writer runs between the read and the
// write. Whatever move returns, Apply refuses a result that is not whole,
// that lowers the epoch, that leaves a fenced home at its fenced epoch, or
// that makes a standby of a home that has served; the rest of each move's
// rules are the transition's.
func Apply(home string, move func(Term) (Term, error)) (Term, error) {
	cur, err := Load(home)
	if err != nil {
		return Term{}, err
	}
	next, err := move(cur)
	if err != nil {
		return Term{}, err
	}
	if err := next.Check(); err != nil {
		return Term{}, fmt.Errorf("%w: %v", ErrTransition, err)
	}
	switch {
	case next.Epoch < cur.Epoch:
		return Term{}, refuse(cur, "the new term's epoch %d is lower", next.Epoch)
	case cur.State == Fenced && next.Epoch == cur.Epoch && next.State != Fenced:
		return Term{}, refuse(cur, "a fenced home changes only at a higher epoch")
	case cur.State != Standby && next.State == Standby:
		return Term{}, refuse(cur, "a home that has served never becomes a standby")
	}
	return next, write(home, next)
}

// Create writes t as the term of a home that has no term file yet (a new
// standby). A home that has one is refused: it changes only through Apply.
// The caller holds the home's lock.
func Create(home string, t Term) error {
	if err := t.Check(); err != nil {
		return err
	}
	if _, err := os.Lstat(filepath.Join(home, File)); !errors.Is(err, fs.ErrNotExist) {
		if err == nil {
			return fmt.Errorf("%w: %s already has a term", ErrTransition, home)
		}
		return err
	}
	return write(home, t)
}

func write(home string, t Term) error {
	b, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return err
	}
	return atomicfile.WritePrivate(filepath.Join(home, File), append(b, '\n'))
}

// ErrTransition is the error every refused transition wraps.
var ErrTransition = errors.New("term transition refused")

func refuse(from Term, format string, args ...any) error {
	return fmt.Errorf("%w: %s at epoch %d: %s", ErrTransition, from.State, from.Epoch, fmt.Sprintf(format, args...))
}

// Freeze stops a serving home for a move, at the same epoch.
func (t Term) Freeze() (Term, error) {
	if t.State != Serving {
		return Term{}, refuse(t, "only a serving home freezes")
	}
	t.State = Frozen
	return t, nil
}

// Unfreeze returns a frozen home to serving at the same epoch: the move was
// called off before anything else served.
func (t Term) Unfreeze() (Term, error) {
	if t.State != Frozen {
		return Term{}, refuse(t, "only a frozen home unfreezes")
	}
	t.State = Serving
	return t, nil
}

// Designate names succ as the home a serving or frozen home would move to.
func (t Term) Designate(succ Peer) (Term, error) {
	if t.State != Serving && t.State != Frozen {
		return Term{}, refuse(t, "only a serving or frozen home names a successor")
	}
	if err := succ.check("successor"); err != nil {
		return Term{}, fmt.Errorf("%w: %v", ErrTransition, err)
	}
	t.Successor = &succ
	return t, nil
}

// Fence records that succ serves at epoch, which must be above this home's:
// the term takes succ's epoch, and this home never serves below it. A home already fenced is fenced
// again only by a higher epoch.
func (t Term) Fence(succ Peer, epoch uint64) (Term, error) {
	if t.State == Standby {
		return Term{}, refuse(t, "a standby has never served; there is nothing to fence")
	}
	if epoch <= t.Epoch {
		return Term{}, refuse(t, "the successor's epoch %d is not above this home's", epoch)
	}
	if err := succ.check("successor"); err != nil {
		return Term{}, fmt.Errorf("%w: %v", ErrTransition, err)
	}
	return Term{Epoch: epoch, State: Fenced, Successor: &succ, Predecessor: t.Predecessor}, nil
}

// NewStandby is the term of a fresh home that follows pred, holding its data
// from epoch on. It is the term a new standby starts with, not a
// transition: a home that has served never becomes a standby.
func NewStandby(pred Peer, epoch uint64) (Term, error) {
	t := Term{Epoch: epoch, State: Standby, Predecessor: &pred}
	if err := t.Check(); err != nil {
		return Term{}, err
	}
	return t, nil
}

// Promote makes a standby, or a fenced home taking its data back, serve at
// epoch, which must be above both this home's epoch and the epoch of the
// data it serves (setEpoch, from the backup set it restored). The successor
// is cleared: a new one is designated later.
func (t Term) Promote(epoch, setEpoch uint64) (Term, error) {
	if t.State != Standby && t.State != Fenced {
		return Term{}, refuse(t, "only a standby or a fenced home is promoted")
	}
	if epoch <= t.Epoch || epoch <= setEpoch {
		return Term{}, refuse(t, "epoch %d is not above both this home's epoch (%d) and its data's (%d)", epoch, t.Epoch, setEpoch)
	}
	pred := t.Predecessor
	if t.State == Fenced {
		pred = t.Successor // the home that served after this one
	}
	return Term{Epoch: epoch, State: Serving, Predecessor: pred}, nil
}

// CheckServes is nil when home's term lets a daemon serve it. Every other
// term, and one that cannot be read, is an error that says what the home is.
func CheckServes(home string) error {
	t, err := Load(home)
	if err != nil {
		return fmt.Errorf("%v: no daemon serves this home until its term can be read", err)
	}
	if t.Serves() {
		return nil
	}
	switch t.State {
	case Fenced:
		return fmt.Errorf("%s was fenced at epoch %d: %s serves its team projects now, so no daemon serves this copy", home, t.Epoch, t.Successor.URL)
	case Frozen:
		return fmt.Errorf("%s is frozen at epoch %d for a move to another home: no daemon serves it while the move runs", home, t.Epoch)
	default: // Standby
		return fmt.Errorf("%s is a standby at epoch %d for %s: it serves only once promoted", home, t.Epoch, t.Predecessor.URL)
	}
}
