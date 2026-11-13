package secrets

import (
	"bytes"
	"context"
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

// KeychainDriver reads keychain:SERVICE[:ACCOUNT], a macOS generic-password
// item, with /usr/bin/security. The secret (its stdout) is never part of an
// error. A locked keychain fails rather than prompting a headless daemon.
type KeychainDriver struct {
	// Bin is the security binary (default /usr/bin/security).
	Bin string
}

func (k KeychainDriver) Get(ctx context.Context, r Ref) (Value, error) {
	if runtime.GOOS != "darwin" && k.Bin == "" {
		return Value{}, api.Errorf(api.CodeUnavailable, "keychain:%s: the macOS keychain is not available on %s", r.Name, runtime.GOOS).
			WithHint("use a file: secret (chmod 600) on this host")
	}
	bin := k.Bin
	if bin == "" {
		bin = "/usr/bin/security"
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

func exitText(err error) string {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return fmt.Sprintf("security exited %d", ee.ExitCode())
	}
	return err.Error()
}
