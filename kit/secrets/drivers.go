package secrets

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/sengirunners/potluck/kit/api"
)

// MaxFile is the largest secret file read.
const MaxFile = 64 << 10

// FileDriver reads file:PATH. The path must be absolute and name a regular
// file no larger than MaxFile that only its owner may read or write (no
// group or other permission bits: chmod 600), as ssh requires of its keys.
// Surrounding whitespace is trimmed. The file is read on every Get, so a
// rotated secret is picked up at once.
type FileDriver struct{}

func (FileDriver) Get(_ context.Context, r Ref) (Value, error) {
	p := r.Name
	if !filepath.IsAbs(p) {
		return Value{}, api.Errorf(api.CodeBadRequest, "secret file %q is not an absolute path", p)
	}
	f, err := os.Open(p)
	if errors.Is(err, fs.ErrNotExist) {
		return Value{}, NotFound(api.Errorf(api.CodeUnavailable, "no secret file %s", p))
	}
	if err != nil {
		return Value{}, api.Errorf(api.CodeUnavailable, "cannot read secret file %s: %v", p, errors.Unwrap(err))
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return Value{}, api.Errorf(api.CodeUnavailable, "cannot read secret file %s: %v", p, errors.Unwrap(err))
	}
	if !st.Mode().IsRegular() {
		return Value{}, api.Errorf(api.CodeUnavailable, "secret file %s is not a regular file", p)
	}
	if runtime.GOOS != "windows" && st.Mode().Perm()&0o077 != 0 {
		return Value{}, api.Errorf(api.CodeUnavailable, "secret file %s is readable or writable by others (mode %04o)", p, st.Mode().Perm()).
			WithHint("chmod 600 " + p)
	}
	if st.Size() > MaxFile {
		return Value{}, api.Errorf(api.CodeUnavailable, "secret file %s is larger than %d bytes", p, MaxFile)
	}
	b, err := io.ReadAll(io.LimitReader(f, MaxFile+1))
	if err != nil {
		return Value{}, api.Errorf(api.CodeUnavailable, "cannot read secret file %s: %v", p, errors.Unwrap(err))
	}
	return NewValue(strings.TrimSpace(string(b))), nil
}

// Info: a file is class O and needs no one at the screen.
func (FileDriver) Info() Info { return Info{Class: ClassO, Headless: true} }

// Put writes v to file:PATH, 0600 (a new directory 0700), never over a
// different value. The value goes to a temporary file in the same directory
// that is then hard-linked into place, so a reader never sees half of it and
// a racing writer cannot be written over. A crash before the temporary file
// is removed may leave it (0600, named .NAME.tmp-*) beside the secret.
func (d FileDriver) Put(ctx context.Context, r Ref, v Value) error {
	if err := checkFilePut(r, v); err != nil {
		return err
	}
	return putWith(ctx, d, r, v, func() error {
		tmp, err := writeTemp(r.Name, v)
		if err != nil {
			return err
		}
		defer os.Remove(tmp)
		if err := os.Link(tmp, r.Name); err != nil {
			return api.Errorf(api.CodeUnavailable, "cannot store secret file %s: %v", r.Name, errors.Unwrap(err)).
				WithHint("Put needs a filesystem with hard links; keep secrets on a local disk")
		}
		return nil
	})
}

// Replace writes v to file:PATH over whatever file is there, through a
// temporary file and a rename. A symlink at PATH is replaced by the file,
// not followed. A crash before the rename may leave the temporary file
// (0600, named .NAME.tmp-*) beside it.
func (FileDriver) Replace(_ context.Context, r Ref, v Value) error {
	if err := checkFilePut(r, v); err != nil {
		return err
	}
	tmp, err := writeTemp(r.Name, v)
	if err != nil {
		return err
	}
	if err := os.Rename(tmp, r.Name); err != nil {
		os.Remove(tmp)
		return api.Errorf(api.CodeUnavailable, "cannot store secret file %s: %v", r.Name, errors.Unwrap(err))
	}
	return nil
}

// Delete removes file:PATH. A missing file is not an error; anything but a
// regular file (or a link to one) is refused.
func (FileDriver) Delete(_ context.Context, r Ref) error {
	p := r.Name
	if !filepath.IsAbs(p) {
		return api.Errorf(api.CodeBadRequest, "secret file %q is not an absolute path", p)
	}
	st, err := os.Lstat(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return api.Errorf(api.CodeUnavailable, "cannot remove secret file %s: %v", p, errors.Unwrap(err))
	}
	if !st.Mode().IsRegular() && st.Mode()&fs.ModeSymlink == 0 {
		return api.Errorf(api.CodeUnavailable, "secret file %s is not a regular file", p)
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return api.Errorf(api.CodeUnavailable, "cannot remove secret file %s: %v", p, errors.Unwrap(err))
	}
	return nil
}

func checkFilePut(r Ref, v Value) error {
	if err := checkPut(r, v); err != nil {
		return err
	}
	if !filepath.IsAbs(r.Name) {
		return api.Errorf(api.CodeBadRequest, "secret file %q is not an absolute path", r.Name)
	}
	if len(v.Reveal())+1 > MaxFile { // the file holds the value and a newline
		return api.Errorf(api.CodeBadRequest, "%s: a secret file holds at most %d bytes and a newline", r, MaxFile-1)
	}
	return nil
}

// writeTemp writes v and a newline to a new 0600 file beside path (making
// path's directory 0700 if it is missing), flushed to disk, and returns its
// name.
func writeTemp(path string, v Value) (string, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", api.Errorf(api.CodeUnavailable, "cannot make secret directory %s: %v", dir, errors.Unwrap(err))
	}
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return "", api.Errorf(api.CodeUnavailable, "cannot store secret file %s: %v", path, errors.Unwrap(err))
	}
	_, err = f.WriteString(v.Reveal() + "\n")
	if err == nil {
		err = f.Chmod(0o600)
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(f.Name())
		return "", api.Errorf(api.CodeUnavailable, "cannot store secret file %s: %v", path, errors.Unwrap(err))
	}
	return f.Name(), nil
}

// EnvDriver reads env:NAME from this process's environment. An unset
// variable is not found; a set, empty one is a zero Value.
type EnvDriver struct{}

func (EnvDriver) Get(_ context.Context, r Ref) (Value, error) {
	v, ok := os.LookupEnv(r.Name)
	if !ok {
		return Value{}, NotFound(api.Errorf(api.CodeUnavailable, "environment variable %s is not set", r.Name))
	}
	return NewValue(strings.TrimSpace(v)), nil
}

// Info: a variable lives in the process as its owner's, and needs no one at
// the screen.
func (EnvDriver) Info() Info { return Info{Class: ClassO, Headless: true} }

// Put, Replace and Delete refuse: a variable is set by whoever starts the
// process, and a change here would not outlive it.
func (EnvDriver) Put(_ context.Context, r Ref, _ Value) error     { return envRefuses(r) }
func (EnvDriver) Replace(_ context.Context, r Ref, _ Value) error { return envRefuses(r) }
func (EnvDriver) Delete(_ context.Context, r Ref) error           { return envRefuses(r) }

func envRefuses(r Ref) error {
	return api.Errorf(api.CodeBadRequest, "env:%s cannot be stored: the variable is set by whoever starts the process", r.Name).
		WithHint("store it with file:/path (chmod 600) or keychain:service, and point the reference there")
}

// KeychainDriver reads keychain:SERVICE[:ACCOUNT], a macOS generic-password
// item, with /usr/bin/security. The secret (its stdout) is never part of an
// error. A locked keychain fails rather than prompting a headless daemon.
type KeychainDriver struct {
	// Bin is the security binary (default /usr/bin/security).
	Bin string
}

func (k KeychainDriver) Get(ctx context.Context, r Ref) (Value, error) {
	bin, err := k.bin(r)
	if err != nil {
		return Value{}, err
	}
	args := []string{"find-generic-password", "-s", r.Name, "-w"}
	if r.Account != "" {
		args = append(args, "-a", r.Account)
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && ee.ExitCode() == 44 {
			return Value{}, NotFound(api.Errorf(api.CodeUnavailable, "no keychain item %q", r.Name).
				WithHint("security add-generic-password -s " + r.Name + " -a $USER -w   (it prompts for the secret)"))
		}
		return Value{}, api.Errorf(api.CodeUnavailable, "cannot read keychain item %q (%s)", r.Name, exitText(err))
	}
	return NewValue(strings.TrimSpace(out.String())), nil
}

func (k KeychainDriver) bin(r Ref) (string, error) {
	if runtime.GOOS != "darwin" && k.Bin == "" {
		return "", api.Errorf(api.CodeUnavailable, "keychain:%s: the macOS keychain is not available on %s", r.Name, runtime.GOOS).
			WithHint("use a file: secret (chmod 600) on this host")
	}
	if k.Bin == "" {
		return "/usr/bin/security", nil
	}
	return k.Bin, nil
}

// Info: anyone running as the owner can read an item with the same command
// (class O), and a locked keychain fails, so it is not for headless use.
func (KeychainDriver) Info() Info { return Info{Class: ClassO, Headless: false} }

// Put adds a generic-password item, never over a different value. The value
// reaches security on its stdin (security -i, hex-encoded with -X), never in
// argv.
func (k KeychainDriver) Put(ctx context.Context, r Ref, v Value) error {
	if err := k.checkPut(r, v); err != nil {
		return err
	}
	return putWith(ctx, k, r, v, func() error { return k.add(ctx, r, v, false) })
}

// Replace adds the item or updates it in place (security -U). Two Replaces
// racing on one item both write; the one whose value is not left there
// fails its read-back.
func (k KeychainDriver) Replace(ctx context.Context, r Ref, v Value) error {
	if err := k.checkPut(r, v); err != nil {
		return err
	}
	return k.add(ctx, r, v, true)
}

// Delete removes the item; a missing one is not an error.
func (k KeychainDriver) Delete(ctx context.Context, r Ref) error {
	bin, err := k.bin(r)
	if err != nil {
		return err
	}
	if err := checkKeychainNames(r); err != nil {
		return err
	}
	args := []string{"delete-generic-password", "-s", r.Name, "-a", r.Account}
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && ee.ExitCode() == 44 {
			return nil
		}
		return api.Errorf(api.CodeUnavailable, "cannot remove keychain item %q (%s)", r.Name, exitText(err))
	}
	return nil
}

// MaxKeychain is the largest secret the keychain driver stores.
const MaxKeychain = 4 << 10

// checkPut narrows the stored value to printable ASCII (security prints any
// other password as hex, so it would not read back the same) of at most
// MaxKeychain bytes.
func (k KeychainDriver) checkPut(r Ref, v Value) error {
	if _, err := k.bin(r); err != nil {
		return err
	}
	if err := checkPut(r, v); err != nil {
		return err
	}
	if err := checkKeychainNames(r); err != nil {
		return err
	}
	if len(v.Reveal()) > MaxKeychain {
		return api.Errorf(api.CodeBadRequest, "%s: the keychain stores secrets of at most %d bytes", r, MaxKeychain).
			WithHint("store it with file:/path (chmod 600) instead")
	}
	for _, c := range []byte(v.Reveal()) {
		if c < 0x20 || c > 0x7e {
			return api.Errorf(api.CodeBadRequest, "%s: the keychain stores only printable ASCII secrets", r).
				WithHint("store it with file:/path (chmod 600) instead")
		}
	}
	return nil
}

// checkKeychainNames wants an account, so a write names one item (security
// matches a missing account to any item of the service), and keeps the
// service and account to plain words for security -i's command line:
// letters, digits and . _ - @ + = /, not starting with "-".
func checkKeychainNames(r Ref) error {
	if r.Name == "" || r.Account == "" {
		return api.Errorf(api.CodeBadRequest, "keychain:%s: storing or removing needs a service and an account (keychain:service:account)", r.Name)
	}
	for _, s := range []string{r.Name, r.Account} {
		bad := strings.HasPrefix(s, "-")
		for _, c := range []byte(s) {
			plain := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte("._-@+=/", c) >= 0
			if !plain {
				bad = true
			}
		}
		if bad {
			return api.Errorf(api.CodeBadRequest, "keychain service and account may hold only letters, digits and . _ - @ + = /, and not start with -")
		}
	}
	return nil
}

// add runs add-generic-password through security -i. security reports a
// failed command on stderr, so any stderr output is a failure; its text is
// never shown, since it may echo the command line.
func (k KeychainDriver) add(ctx context.Context, r Ref, v Value, update bool) error {
	bin, err := k.bin(r)
	if err != nil {
		return err
	}
	line := "add-generic-password -s " + r.Name + " -a " + r.Account
	if update {
		line += " -U"
	}
	line += " -X " + hex.EncodeToString([]byte(v.Reveal())) + "\n"
	// What counts is what the keychain holds afterwards: security may warn
	// on stderr after storing, or (in principle) store something else.
	cmd := exec.CommandContext(ctx, bin, "-i")
	cmd.Stdin = strings.NewReader(line)
	var errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = io.Discard, &errOut
	err = cmd.Run()
	if got, gerr := k.Get(ctx, r); gerr == nil && got.Reveal() == v.Reveal() {
		return nil
	}
	switch {
	case err != nil:
		return api.Errorf(api.CodeUnavailable, "cannot store keychain item %q (%s)", r.Name, exitText(err))
	case strings.TrimSpace(errOut.String()) != "":
		return api.Errorf(api.CodeUnavailable, "cannot store keychain item %q (security reported an error)", r.Name).
			WithHint("is the keychain locked? security unlock-keychain")
	}
	return misstored{api.Errorf(api.CodeUnavailable, "keychain item %q does not read back what was stored", r.Name).
		WithHint("check it with security find-generic-password, and delete it before trying again")}
}

func exitText(err error) string {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return fmt.Sprintf("security exited %d", ee.ExitCode())
	}
	return err.Error()
}
