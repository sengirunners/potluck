package secrets

import (
	"context"
	"errors"
	"strings"

	"github.com/sengirunners/potluck/kit/api"
)

// Class is where a secret may live and who can read its bytes: the key
// inventory's four classes, with the same names.
type Class string

const (
	// ClassP is person-present: hardware that needs a touch, never a file.
	ClassP Class = "P"
	// ClassS is service-isolated: an account or environment no agent
	// session runs in.
	ClassS Class = "S"
	// ClassO is owner-user at rest: the keychain or a 0600 file. Every
	// process of that OS user, agents included, can read it.
	ClassO Class = "O"
	// ClassE is ephemeral in a box: what one job needs, on a memory
	// filesystem, for the box's life.
	ClassE Class = "E"
)

// Info is what a driver says about the secrets it holds: their class, and
// whether it works headless (a daemon with no one at the screen and the
// keychain possibly locked).
type Info struct {
	Class    Class
	Headless bool
}

// Describer is a driver that reports its Info. Every built-in driver is one.
type Describer interface {
	Info() Info
}

// Putter is a driver that can store secrets. Put never writes over a
// different value: the same value already there is success, another one is
// an error matching ErrExists. Replace writes over whatever is there
// (rotation) and is atomic: a reader sees the old value or the new one.
// Delete removes the secret; a missing one is not an error. A driver that
// cannot store at all may still implement Putter to refuse with a reason.
//
// A value is stored only if Get would read it back the same: not empty, no
// surrounding whitespace, at most MaxFile bytes (a driver may narrow this).
// Errors never contain the value.
type Putter interface {
	Put(ctx context.Context, r Ref, v Value) error
	Replace(ctx context.Context, r Ref, v Value) error
	Delete(ctx context.Context, r Ref) error
}

// Put stores v at ref, never over a different value (see Putter).
func (r *Reader) Put(ctx context.Context, ref Ref, v Value) error {
	p, err := r.putter(ref)
	if err != nil {
		return err
	}
	return p.Put(ctx, ref, v)
}

// Replace stores v at ref over whatever is there (see Putter).
func (r *Reader) Replace(ctx context.Context, ref Ref, v Value) error {
	p, err := r.putter(ref)
	if err != nil {
		return err
	}
	return p.Replace(ctx, ref, v)
}

// Delete removes the secret at ref; a missing one is not an error.
func (r *Reader) Delete(ctx context.Context, ref Ref) error {
	p, err := r.putter(ref)
	if err != nil {
		return err
	}
	return p.Delete(ctx, ref)
}

// Info is what the driver for scheme reports, and false if there is no such
// driver or it reports nothing.
func (r *Reader) Info(scheme string) (Info, bool) {
	if r == nil {
		return Info{}, false
	}
	d, ok := r.drivers[scheme].(Describer)
	if !ok {
		return Info{}, false
	}
	return d.Info(), true
}

func (r *Reader) putter(ref Ref) (Putter, error) {
	d, err := r.driver(ref)
	if err != nil {
		return nil, err
	}
	p, ok := d.(Putter)
	if !ok {
		return nil, api.Errorf(api.CodeBadRequest, "secret driver %q cannot store a secret", ref.Scheme)
	}
	return p, nil
}

// Put stores v at ref with the Default drivers.
func Put(ctx context.Context, ref Ref, v Value) error { return Default().Put(ctx, ref, v) }

// Replace stores v at ref over whatever is there, with the Default drivers.
func Replace(ctx context.Context, ref Ref, v Value) error { return Default().Replace(ctx, ref, v) }

// Delete removes the secret at ref with the Default drivers.
func Delete(ctx context.Context, ref Ref) error { return Default().Delete(ctx, ref) }

// checkPut refuses a ref that names nothing and a value Get would not read
// back the same.
func checkPut(r Ref, v Value) error {
	s := v.Reveal()
	switch {
	case r.Name == "":
		return api.Errorf(api.CodeBadRequest, "secret reference %s names nothing", r.Scheme)
	case s == "":
		return api.Errorf(api.CodeBadRequest, "%s: refusing to store an empty secret", r)
	case strings.TrimSpace(s) != s:
		return api.Errorf(api.CodeBadRequest, "%s: a secret with surrounding whitespace would not read back the same", r)
	case len(s) > MaxFile:
		return api.Errorf(api.CodeBadRequest, "%s: a secret larger than %d bytes is not stored", r, MaxFile)
	}
	return nil
}

// already compares what d holds at r with v: nil if it is v, an ErrExists
// error if it is something else, and Get's error (ErrNotFound when absent)
// otherwise.
func already(ctx context.Context, d Driver, r Ref, v Value) error {
	old, err := d.Get(ctx, r)
	if err != nil {
		return err
	}
	if old.Reveal() == v.Reveal() {
		return nil
	}
	return Exists(api.Errorf(api.CodeConflict, "%s already holds another value: never written over", r).
		WithHint("delete it first, or replace it on purpose"))
}

// misstored is a store that reported success but reads back something
// else: Put reports it as is, not as a value already there.
type misstored struct{ e *api.Error }

func (m misstored) Error() string { return m.e.Error() }
func (m misstored) Unwrap() error { return m.e }

// putWith is Put for a driver whose create step fails rather than write over
// an existing secret: absent, create; there already, compare. A create that
// loses a race to another writer is compared again.
func putWith(ctx context.Context, d Driver, r Ref, v Value, create func() error) error {
	if err := already(ctx, d, r, v); !errors.Is(err, ErrNotFound) {
		return err
	}
	cerr := create()
	var m misstored
	if cerr == nil || errors.As(cerr, &m) {
		return cerr
	}
	if err := already(ctx, d, r, v); err == nil || errors.Is(err, ErrExists) {
		return err
	}
	return cerr
}
