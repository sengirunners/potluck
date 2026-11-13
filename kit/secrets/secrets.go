// Package secrets is the one way zm and its adapters read a secret (an API
// key, a token, a password). A Ref names where the secret lives; a Driver per
// scheme reads it; the Value that comes back masks itself when printed,
// logged or encoded, so a secret leaves only through Reveal (see Value for
// the one fmt hole).
//
// ReadFile, CopyFile and WriteFile (files.go) are the password-file helpers
// the work and homes modules share, so a binding's secret is stored one way;
// Store (store.go) is a set of them by name, homes' password slots.
//
// Known reads outside it: the CLI's ZM_TOKEN (kit/client, the caller's
// own bearer token from its environment) and the beads adapter's
// BEADS_DOLT_PASSWORD, read only to redact it from bd's output.
//
// Drivers:
//
//	file:PATH                 a regular file, owner-only (0600 or tighter)
//	keychain:SERVICE[:ACCOUNT] a macOS generic-password item (/usr/bin/security)
//	env:NAME                  an environment variable of the reading process
//
// The daemon and serve-mode adapters must work headless and with the
// keychain locked, so the keychain is for interactive hosts only; files are
// the default everywhere else. A 1Password (op) driver with a service-account
// token is the planned next one; a driver is any type with Get, added with
// Reader.With.
//
// Why not go-keyring: on macOS it shells out to /usr/bin/security as this
// does, and on Linux it needs a D-Bus secret service that a headless server
// box lacks. The rest (reading a file, an env var) needs no library.
package secrets

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/sengirunners/potluck/kit/api"
)

// Value is a secret. Its String, GoString, Format and JSON/text encodings are
// "[secret]"; Reveal returns the secret itself. One hole: fmt prints an
// unexported struct field without calling its methods, so a Value held in
// one shows as a pointer, never the text (the secret sits behind it).
type Value struct{ p *string }

// NewValue wraps s (tests and drivers).
func NewValue(s string) Value {
	if s == "" {
		return Value{}
	}
	return Value{p: &s}
}

const masked = "[secret]"

func (v Value) Reveal() string {
	if v.p == nil {
		return ""
	}
	return *v.p
}
func (v Value) IsZero() bool                  { return v.Reveal() == "" }
func (v Value) String() string                { return masked }
func (v Value) GoString() string              { return masked }
func (v Value) Format(f fmt.State, _ rune)    { _, _ = f.Write([]byte(masked)) }
func (v Value) MarshalJSON() ([]byte, error)  { return json.Marshal(masked) }
func (v Value) MarshalText() ([]byte, error)  { return []byte(masked), nil }
func (v *Value) UnmarshalJSON(b []byte) error { return errors.New("secrets: a Value is never decoded") }
func (v *Value) UnmarshalText(b []byte) error { return errors.New("secrets: a Value is never decoded") }

// Ref names a secret: Scheme picks the driver, Name and Account are the
// driver's (a path; a keychain service and account; a variable name).
type Ref struct{ Scheme, Name, Account string }

// File, Keychain and Env build Refs.
func File(path string) Ref { return Ref{Scheme: "file", Name: path} }
func Keychain(service, account string) Ref {
	return Ref{Scheme: "keychain", Name: service, Account: account}
}
func Env(name string) Ref { return Ref{Scheme: "env", Name: name} }

// String is the Ref's text form (it holds no secret).
func (r Ref) String() string {
	if r.Account != "" {
		return r.Scheme + ":" + r.Name + ":" + r.Account
	}
	return r.Scheme + ":" + r.Name
}

// Parse reads a Ref's text form: "file:/abs/path", "keychain:service",
// "keychain:service:account", "env:NAME". A leading "~/" in a file path is
// the user's home.
func Parse(s string) (Ref, error) {
	scheme, rest, ok := strings.Cut(s, ":")
	if !ok || rest == "" {
		return Ref{}, api.Errorf(api.CodeBadRequest, "secret reference must be scheme:name").
			WithHint("file:/path/to/key, keychain:service[:account] or env:NAME")
	}
	r := Ref{Scheme: scheme, Name: rest}
	switch scheme {
	case "file":
		r.Name = ExpandHome(rest)
	case "keychain":
		r.Name, r.Account, _ = strings.Cut(rest, ":")
	case "env":
	default:
		return Ref{}, api.Errorf(api.CodeBadRequest, "unknown secret reference scheme").
			WithHint("file:, keychain: or env:")
	}
	return r, nil
}

// ExpandHome turns a leading "~/" into the user's home directory.
func ExpandHome(p string) string {
	if rest, ok := strings.CutPrefix(p, "~/"); ok {
		if h, err := os.UserHomeDir(); err == nil {
			return h + "/" + rest
		}
	}
	return p
}

// Driver reads secrets of one scheme. A missing secret is an error that
// errors.Is ErrNotFound; an empty one is a zero Value and no error.
type Driver interface {
	Get(ctx context.Context, r Ref) (Value, error)
}

// DriverFunc adapts a function to Driver.
type DriverFunc func(ctx context.Context, r Ref) (Value, error)

func (f DriverFunc) Get(ctx context.Context, r Ref) (Value, error) { return f(ctx, r) }

// ErrNotFound is what errors.Is matches when the secret does not exist (the
// error itself is an *api.Error with code unavailable and a hint).
var ErrNotFound = errors.New("secret not found")

// NotFound wraps an *api.Error so it also matches ErrNotFound.
func NotFound(e *api.Error) error { return missing{e} }

type missing struct{ e *api.Error }

func (m missing) Error() string        { return m.e.Error() }
func (m missing) Unwrap() error        { return m.e }
func (m missing) Is(target error) bool { return target == ErrNotFound }

// Recode returns err with its api code replaced by code, keeping its message
// and hint (a missing or loose secret file named in a request is the
// caller's bad_request, not the daemon's unavailable).
func Recode(err error, code string) error {
	if err == nil {
		return nil
	}
	var e *api.Error
	if errors.As(err, &e) {
		c := *e
		c.Code = code
		return &c
	}
	return api.Errorf(code, "%v", err)
}

// Prefix returns err with prefix put before its message, keeping its code
// and hint (which config key or flag named the secret).
func Prefix(err error, prefix string) error {
	var e *api.Error
	if errors.As(err, &e) {
		c := *e
		c.Message = prefix + c.Message
		if errors.Is(err, ErrNotFound) {
			return NotFound(&c)
		}
		return &c
	}
	return fmt.Errorf("%s%w", prefix, err)
}

// Reader reads Refs through its drivers. The zero Reader has none; Default
// has file, keychain and env.
type Reader struct{ drivers map[string]Driver }

// Default is a Reader with the built-in drivers.
func Default() *Reader {
	return (&Reader{}).With("file", FileDriver{}).With("keychain", KeychainDriver{}).With("env", EnvDriver{})
}

// With returns a copy of r with d serving scheme (replacing any driver there).
func (r *Reader) With(scheme string, d Driver) *Reader {
	c := &Reader{drivers: map[string]Driver{}}
	if r != nil {
		for k, v := range r.drivers {
			c.drivers[k] = v
		}
	}
	c.drivers[scheme] = d
	return c
}

// Get reads ref. Errors never contain the secret.
func (r *Reader) Get(ctx context.Context, ref Ref) (Value, error) {
	var d Driver
	if r != nil {
		d = r.drivers[ref.Scheme]
	}
	if d == nil {
		return Value{}, api.Errorf(api.CodeUnavailable, "no secret driver for %q", ref.Scheme)
	}
	if ref.Name == "" {
		return Value{}, api.Errorf(api.CodeBadRequest, "secret reference %s names nothing", ref.Scheme)
	}
	return d.Get(ctx, ref)
}

// Get reads ref with the Default drivers.
func Get(ctx context.Context, ref Ref) (Value, error) { return Default().Get(ctx, ref) }
